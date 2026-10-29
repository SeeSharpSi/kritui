'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const { once } = require('node:events');
const fs = require('node:fs/promises');
const http = require('node:http');
const path = require('node:path');
const { after, before, test } = require('node:test');
const { chromium } = require('playwright');

const { buildApp, cleanupBuild } = require('./lib/build');
const { errorResponse, finalResponse, toolCallResponse } = require('./lib/fake-provider');
const { resultsRoot, runIsolatedCase } = require('./lib/isolated-case');
const {
  assertNoStaleCompletionFailure,
  changeInboxDelivery,
  clickInboxSteerNow,
  clickRetry,
  inboxTexts,
  openChat,
  reloadChat,
  removeInboxItem,
  selectWebfetch,
  signup,
  submitPrompt,
  waitCompletionActive,
  waitCompletionDone,
  waitCompletionError,
  waitForHistoryText,
  waitForInboxCount,
  waitForInboxText,
  waitForNoCompletionControl,
} = require('./lib/ui');
const { waitUntil } = require('./lib/wait');

const password = 'kritui-playwright-password';
const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || '/usr/bin/chromium';
const titleGenerationSystemPrompt = `You are a model that generates chat titles based on the user's first chat message.
You will be given the user's message, and must generate a title with these rules:
1. Concise; 10 words max
2. No trailing period
3. No markdown; just plain text
4. Clearly shows what the message was requesting and what the chat will be about`;

const shareUI = Object.freeze({
  preparationSelector: '[data-share-preparation]',
  preparationAttribute: 'data-share-preparation',
  tokenAttribute: 'data-share-token',
  snapshotSelector: 'template[data-share-snapshot]',
  snapshotAttribute: 'data-share-snapshot',
  rootSelector: '[data-share-root]',
  stateAttribute: 'data-share-state',
  payloadSelector: '#shared-response-payload',
  payloadID: 'shared-response-payload',
  versionAttribute: 'data-format-version',
  nonceAttribute: 'data-nonce',
  ciphertextAttribute: 'data-ciphertext',
  frameSelector: '#shared-response-frame',
  statusSelector: '#shared-response-status',
});

let browser;
let build;

function username(tag) {
  return `e2e_${tag}_${Date.now().toString(36).slice(-6)}_${process.pid.toString(36)}`.slice(0, 32);
}

function lastUser(call) {
  return [...call.messages].reverse().find((message) => message.role === 'user')?.content;
}

function userContents(call) {
  return call.messages.filter((message) => message.role === 'user').map((message) => message.content);
}

function assertNoCallOverride(calls, expected) {
  assert.equal(calls.length, expected, `expected ${expected} provider calls, saw ${calls.length}`);
}

function toolCallIDs(locator) {
  return locator.evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-tool-call-id') ?? ''));
}

async function waitForToolCallIDs(locator, expected, label) {
  try {
    await waitUntil(async () => {
      const actual = await toolCallIDs(locator);
      return actual.length === expected.length && actual.every((id, index) => id === expected[index]);
    }, {
      timeout: 15000,
      label,
    });
  } catch (error) {
    const actual = await toolCallIDs(locator).catch(() => ['<unreadable>']);
    throw new Error(`${error.message} Observed tool calls: ${JSON.stringify(actual)}; expected: ${JSON.stringify(expected)}.`);
  }
}

async function assertNoResumeControl(page) {
  assert.equal(
    await page.getByRole('button', { name: /resume/i }).count(),
    0,
    'pending inbox must not render a Resume control',
  );
  assert.equal(
    await page.getByText(/^Resume$/).count(),
    0,
    'pending inbox must not render visible Resume text',
  );
}

async function assertInboxAboveToolbar(page) {
  const inbox = page.locator('#prompt-inbox');
  const controls = page.locator('#settings-button, .chat-options > summary, #history-button');
  assert.equal(await controls.count(), 3, 'expected Settings, Model, and History controls');
  const inboxBox = await inbox.boundingBox();
  assert.ok(inboxBox, 'pending inbox has no bounding box');
  for (let index = 0; index < 3; index += 1) {
    const control = controls.nth(index);
    const box = await control.boundingBox();
    assert.ok(box, `toolbar control ${index} has no bounding box`);
    const label = (await control.innerText()).trim() || `toolbar control ${index}`;
    assert.ok(
      inboxBox.y + inboxBox.height <= box.y + 1,
      `pending inbox bottom ${inboxBox.y + inboxBox.height} must be at or above ${label} top ${box.y}`,
    );
  }
}

async function assertInboxChromeRemoved(page) {
  const inbox = page.locator('#prompt-inbox');
  assert.equal(
    await inbox.locator('.prompt-inbox-heading:visible, .prompt-inbox-title:visible, .prompt-inbox-count:visible').count(),
    0,
    'pending inbox heading and counter must not be visible',
  );
  const text = (await inbox.innerText()).trim();
  assert.ok(!/Pending inputs/i.test(text), `pending inbox must not show its heading, got: ${text}`);
  assert.ok(!/\b\d+\s+pending\b/i.test(text), `pending inbox must not show a pending counter, got: ${text}`);
}

function assertClose(actual, expected, tolerance, label) {
  assert.ok(
    Math.abs(actual - expected) <= tolerance,
    `${label}: got ${actual}, want ${expected} ± ${tolerance}`,
  );
}

function colorChannels(value) {
  const numbers = (value.match(/-?(?:\d+\.?\d*|\.\d+)(?:e[-+]?\d+)?/gi) ?? []).map(Number);
  assert.ok(numbers.length >= 3, `could not read color channels from ${value}`);
  const channels = numbers.slice(0, 3);
  return value.trim().startsWith('color(') ? channels.map((channel) => channel * 255) : channels;
}

function assertDominantColor(value, dominantIndex, label) {
  const channels = colorChannels(value);
  const dominant = channels[dominantIndex];
  const others = channels.filter((_, index) => index !== dominantIndex);
  assert.ok(
    dominant > others[0] + 8 && dominant > others[1] + 8,
    `${label} must have a dominant ${dominantIndex === 0 ? 'red' : 'green'} channel, got ${value}`,
  );
}

async function elementMetrics(locator) {
  return locator.evaluate((node) => {
    const rect = node.getBoundingClientRect();
    const style = getComputedStyle(node);
    return {
      x: rect.x,
      y: rect.y,
      top: rect.top,
      left: rect.left,
      width: rect.width,
      height: rect.height,
      right: rect.right,
      bottom: rect.bottom,
      color: style.color,
      backgroundColor: style.backgroundColor,
      font: style.font,
      padding: style.padding,
      borderRadius: style.borderRadius,
      boxSizing: style.boxSizing,
      borderTopWidth: style.borderTopWidth,
      borderRightWidth: style.borderRightWidth,
      borderBottomWidth: style.borderBottomWidth,
      borderLeftWidth: style.borderLeftWidth,
      borderTopStyle: style.borderTopStyle,
      borderRightStyle: style.borderRightStyle,
      borderBottomStyle: style.borderBottomStyle,
      borderLeftStyle: style.borderLeftStyle,
      borderTopColor: style.borderTopColor,
      borderRightColor: style.borderRightColor,
      borderBottomColor: style.borderBottomColor,
      borderLeftColor: style.borderLeftColor,
    };
  });
}

async function waitForStableLayout(page) {
  await page.evaluate(() => new Promise((resolve) => {
    requestAnimationFrame(() => requestAnimationFrame(resolve));
  }));
}

async function useViewport(page, width, height) {
  await page.setViewportSize({ width, height });
  await page.waitForFunction((expected) => window.innerWidth === expected, width);
  await waitForStableLayout(page);
}

async function iconPaths(locator) {
  return locator.locator('svg path').evaluateAll((nodes) => nodes.map((node) => node.getAttribute('d')));
}

async function assertEqualActionChrome(save, logout, label) {
  const saveMetrics = await elementMetrics(save);
  const logoutMetrics = await elementMetrics(logout);
  assertClose(saveMetrics.height, logoutMetrics.height, 0.5, `${label} Save all and Log out height`);
  for (const property of [
    'font',
    'padding',
    'borderRadius',
    'boxSizing',
    'backgroundColor',
    'borderTopWidth',
    'borderRightWidth',
    'borderBottomWidth',
    'borderLeftWidth',
    'borderTopStyle',
    'borderRightStyle',
    'borderBottomStyle',
    'borderLeftStyle',
  ]) {
    assert.equal(saveMetrics[property], logoutMetrics[property], `${label} ${property} must match between actions`);
  }
  assertDominantColor(saveMetrics.color, 1, `${label} Save all text`);
  assertDominantColor(saveMetrics.borderTopColor, 1, `${label} Save all border`);
  assertDominantColor(logoutMetrics.color, 0, `${label} Log out text`);
  assertDominantColor(logoutMetrics.borderTopColor, 0, `${label} Log out border`);
  assert.notEqual(saveMetrics.color, logoutMetrics.color, `${label} actions must use different semantic colors`);
  return { save: saveMetrics, logout: logoutMetrics };
}

async function assertBottomNavigationGeometry(page, label) {
  const settings = await elementMetrics(page.locator('#settings-button'));
  const model = await elementMetrics(page.locator('.chat-options > summary'));
  const history = await elementMetrics(page.locator('#history-button'));

  assertClose(settings.y, model.y, 0.5, `${label} Settings/Model top edges`);
  assertClose(model.y, history.y, 0.5, `${label} Model/History top edges`);
  assertClose(settings.height, model.height, 0.5, `${label} Settings/Model heights`);
  assertClose(model.height, history.height, 0.5, `${label} Model/History heights`);
  assertClose(settings.width, history.width, 0.5, `${label} Settings/History widths`);
  assertClose(settings.width - model.width, 1, 0.5, `${label} Model width must fit between shared borders`);

  const settingsModelOverlap = settings.right - model.left;
  const modelHistoryOverlap = model.right - history.left;
  assert.ok(
    settingsModelOverlap >= 0.5 && settingsModelOverlap <= 1.5,
    `${label} Settings/Model must share one vertical border, overlap was ${settingsModelOverlap}px`,
  );
  assert.ok(
    modelHistoryOverlap >= 0.5 && modelHistoryOverlap <= 1.5,
    `${label} Model/History must share one vertical border, overlap was ${modelHistoryOverlap}px`,
  );
  return { settings, model, history };
}

async function assertModelMenuGeometry(page, label) {
  const controls = await assertBottomNavigationGeometry(page, label);
  const options = await elementMetrics(page.locator('.chat-options'));
  const list = await elementMetrics(page.locator('.chat-options-list'));
  const summary = controls.model;

  assertClose(list.left, options.left, 0.5, `${label} model list left edge`);
  assertClose(list.right, options.right, 0.5, `${label} model list right edge`);
  assertClose(list.bottom - summary.top, 1, 0.5, `${label} model list bottom border must overlap the nav top border`);
  assert.equal(list.borderBottomWidth, summary.borderTopWidth, `${label} shared border widths must match`);
  assert.equal(list.borderBottomStyle, 'solid', `${label} model list bottom border must be solid`);
  assert.equal(summary.borderTopStyle, 'solid', `${label} nav top border must be solid`);
  return { list, summary, options };
}

async function waitForModelOptions(page, open) {
  await waitUntil(async () => {
    const isOpen = await page.locator('.chat-options').evaluate((element) => element.open);
    const listVisible = await page.locator('.chat-options-list').isVisible().catch(() => false);
    return isOpen === open && listVisible === open;
  }, {
    timeout: 5000,
    label: `model options to be ${open ? 'open' : 'closed'}`,
  });
}

async function waitForPanels(page, visiblePanelID) {
  await waitUntil(async () => {
    const visible = await page.locator('#page-panel > .panel-page').evaluateAll((panels) => (
      panels.filter((panel) => !panel.hidden).map((panel) => panel.id)
    ));
    if (visiblePanelID === null) {
      return visible.length === 0;
    }
    return visible.length === 1 && visible[0] === visiblePanelID;
  }, {
    timeout: 5000,
    label: visiblePanelID === null ? 'all panels to be closed' : `${visiblePanelID} panel to be open`,
  });
}

async function assertSameChat(page, formAction, label) {
  assert.equal(
    await page.locator('#message-form').getAttribute('hx-post'),
    formAction,
    `${label} must not replace the active chat`,
  );
}

async function openModelOptions(page) {
  const options = page.locator('.chat-options');
  if (!(await options.evaluate((element) => element.open))) {
    await options.locator(':scope > summary').click();
  }
  await waitForModelOptions(page, true);
}

async function exercisePanelAndModelNavigation(page, formAction, label) {
  await openModelOptions(page);
  await waitForPanels(page, null);
  await assertModelMenuGeometry(page, `${label} model open`);

  await page.locator('#settings-button').click();
  await waitForPanels(page, 'settings-page');
  await waitForModelOptions(page, false);
  await assertSameChat(page, formAction, `${label} opening Settings`);

  await page.locator('.chat-options > summary').click();
  await waitForPanels(page, null);
  await waitForModelOptions(page, true);
  await assertSameChat(page, formAction, `${label} opening Model from Settings`);

  await page.locator('#history-button').click();
  await waitForPanels(page, 'history-page');
  await waitForModelOptions(page, false);
  await assertSameChat(page, formAction, `${label} opening History`);

  await page.locator('.chat-options > summary').focus();
  await page.keyboard.press('Enter');
  await waitForPanels(page, null);
  await waitForModelOptions(page, true);
  await assertSameChat(page, formAction, `${label} keyboard opening Model from History`);

  await page.locator('.chat-options > summary').focus();
  await page.keyboard.press('Enter');
  await waitForModelOptions(page, false);
  await page.locator('.chat-options > summary').focus();
  await page.keyboard.press('Enter');
  await waitForModelOptions(page, true);
  await waitForPanels(page, null);
  await assertModelMenuGeometry(page, `${label} keyboard model open`);
}

async function openHistoryEditor(page) {
  await page.locator('#history-page').waitFor({ state: 'visible' });
  const entry = page.locator('#history-entries .history-entry.current').first();
  await entry.waitFor({ state: 'visible', timeout: 10000 });
  await entry.locator('.history-edit').click();
  await entry.locator('.history-save').waitFor({ state: 'visible' });
  return entry;
}

async function assertHistoryRenameActions(entry, label) {
  const save = entry.locator('.history-save');
  const cancel = entry.locator('.history-cancel');
  assert.equal(await save.isVisible(), true, `${label} history save must be visible while editing`);
  assert.equal(await cancel.isVisible(), true, `${label} history cancel must be visible while editing`);
  assert.equal(
    await save.evaluate((button) => button.nextElementSibling?.classList.contains('history-cancel')),
    true,
    `${label} history checkmark must sit immediately before the X`,
  );

  const saveMetrics = await elementMetrics(save);
  const cancelMetrics = await elementMetrics(cancel);
  assertClose(saveMetrics.width, cancelMetrics.width, 0.5, `${label} history rename button widths`);
  assertClose(saveMetrics.height, cancelMetrics.height, 0.5, `${label} history rename button heights`);
  assertClose(
    saveMetrics.y + saveMetrics.height / 2,
    cancelMetrics.y + cancelMetrics.height / 2,
    0.5,
    `${label} history rename button vertical centers`,
  );
  const gap = cancelMetrics.x - saveMetrics.right;
  assert.ok(gap >= -0.5 && gap <= 16, `${label} history save/X gap must stay compact, got ${gap}px`);
  assert.deepEqual(await iconPaths(save), ['M20 6 9 17l-5-5'], `${label} history save icon path`);
  assert.deepEqual(
    await iconPaths(cancel),
    ['M18 6 6 18', 'm6 6 12 12'],
    `${label} history cancel icon paths`,
  );
}

async function renameHistoryWith(page, entry, title, method, label) {
  const input = entry.locator('.history-rename input');
  await input.fill(title);
  const responsePromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'PUT' && /^\/chats\/\d+$/.test(url.pathname);
  }, { timeout: 15000 });
  if (method === 'click') {
    await entry.locator('.history-save').click();
  } else {
    await input.press('Enter');
  }
  const response = await responsePromise;
  const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
  assert.ok(
    response.status() >= 200 && response.status() < 300,
    `${label} history rename returned ${response.status()}${body ? `: ${body}` : ''}`,
  );
  await waitUntil(async () => {
    const titles = await page.locator('#history-entries .history-title-link strong').allTextContents();
    return titles.some((value) => value.trim() === title);
  }, {
    timeout: 10000,
    label: `${label} history title ${JSON.stringify(title)}`,
  });
  assert.equal(await entry.locator('.history-rename input').isVisible(), false, `${label} history editor must close`);
}

function deferredGate() {
  let release;
  const promise = new Promise((resolve) => {
    release = resolve;
  });
  return {
    promise,
    release: () => release(),
  };
}

const elisionResultMarker = 'E2E_TOOL_RESULT_BODY_MARKER';
const elisionErrorText = 'websearch: SearXNG returned no results';
const elisionMarker = '[Tool result elided]';

// SearchFixture is a deterministic in-process SearXNG stand-in. Queries
// containing "empty" return no results so websearch produces a stable tool
// error; all other queries return a body long enough to exceed a tiny budget.
class SearchFixture {
  constructor() {
    this.server = null;
    this.port = null;
    this.queries = [];
  }

  async start() {
    this.server = http.createServer((request, response) => {
      const url = new URL(request.url, 'http://127.0.0.1');
      if (request.method !== 'GET' || url.pathname !== '/search') {
        response.writeHead(404, { 'Content-Type': 'application/json' });
        response.end(JSON.stringify({ error: { message: 'unknown search fixture route' } }));
        return;
      }
      const query = url.searchParams.get('q') ?? '';
      this.queries.push(query);
      const results = /empty/i.test(query)
        ? []
        : [{
          title: 'Deterministic elision fixture result',
          url: 'https://fixture.invalid/elision',
          content: `${elisionResultMarker} ${'Deterministic search result payload for tool elision verification. '.repeat(48)}`,
          engine: 'fixture',
          publishedDate: '2026-01-01T00:00:00Z',
          score: 1,
        }];
      response.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      response.end(JSON.stringify({ query, results }));
    });
    this.server.listen(0, '127.0.0.1');
    await once(this.server, 'listening');
    this.port = this.server.address().port;
    return this;
  }

  get url() {
    if (!this.port) {
      throw new Error('search fixture is not started');
    }
    return `http://127.0.0.1:${this.port}`;
  }

  async close() {
    const server = this.server;
    this.server = null;
    if (!server) {
      return;
    }
    const closed = new Promise((resolve) => server.close(resolve));
    if (typeof server.closeAllConnections === 'function') {
      server.closeAllConnections();
    }
    await Promise.race([
      closed,
      new Promise((resolve) => setTimeout(resolve, 2000)),
    ]);
  }
}

function multiToolCallResponse(calls, { tokens = 10 } = {}) {
  return {
    status: 200,
    body: {
      model: 'fake-model',
      choices: [
        {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: calls.map(({ id, name, argumentsValue }) => ({
              id,
              type: 'function',
              function: {
                name,
                arguments: typeof argumentsValue === 'string'
                  ? argumentsValue
                  : JSON.stringify(argumentsValue ?? {}),
              },
            })),
          },
          finish_reason: 'tool_calls',
        },
      ],
      usage: { total_tokens: tokens },
    },
  };
}

function toolResultContent(call, toolCallID) {
  const message = [...call.messages].reverse().find(
    (entry) => entry.role === 'tool' && entry.tool_call_id === toolCallID,
  );
  return message?.content ?? '';
}

async function selectChatTool(page, name) {
  const options = page.locator('details.chat-options');
  assert.equal(await options.count(), 1, 'chat options are missing');
  if (!(await options.evaluate((element) => element.open))) {
    await options.locator(':scope > summary').click();
  }
  const picker = page.locator('#tool-picker');
  assert.equal(await picker.count(), 1, 'tool picker is missing');
  await picker.locator(':scope > summary').waitFor({ state: 'visible', timeout: 10000 });
  if (!(await picker.evaluate((element) => element.open))) {
    await picker.locator(':scope > summary').click();
  }
  const checkbox = picker.locator(`input[name="tool"][value="${name}"]`);
  assert.equal(await checkbox.count(), 1, `${name} tool checkbox is missing`);
  await checkbox.check({ force: true });
  assert.equal(await checkbox.isChecked(), true, `${name} tool checkbox did not turn on`);
}

async function configureToolResultElision(page, { mode, tokenBudget, userTurns } = {}) {
  await page.locator('#settings-button').click();
  await waitForPanels(page, 'settings-page');
  const modeControl = page.locator('#tool-result-elision-mode');
  await modeControl.waitFor({ state: 'visible' });
  await modeControl.selectOption(mode);
  if (mode === 'tool_result_budget') {
    const budget = page.locator('#tool-result-elision-token-budget');
    await page.waitForFunction(() => {
      const input = document.querySelector('#tool-result-elision-token-budget');
      return input !== null && !input.disabled;
    });
    await budget.fill(String(tokenBudget));
  }
  if (mode === 'last_user_turns') {
    await page.locator('#tool-result-elision-user-turns').fill(String(userTurns));
  }
  await page.locator('#settings-page .settings-save').click();
  await page.locator('[data-settings-saved]').waitFor({ state: 'visible', timeout: 15000 });
  assert.equal(
    await page.locator('#tool-result-elision-mode').inputValue(),
    mode,
    `tool-result elision mode ${mode} did not persist`,
  );
  await page.locator('#settings-button').click();
  await waitForPanels(page, null);
}

// expectedToolNameColors resolves the contract colors in the live document.
// Chromium may serialize color-mix() as color(srgb ...), so comparing against a
// probe element avoids assuming an rgb() representation.
async function expectedToolNameColors(page) {
  return page.evaluate(() => {
    const probe = document.createElement('span');
    probe.setAttribute('aria-hidden', 'true');
    probe.style.cssText = 'position:absolute;left:-9999px;top:0;font-size:0;';
    document.body.appendChild(probe);
    const colorFor = (value) => {
      probe.style.color = value;
      return getComputedStyle(probe).color;
    };
    const colors = {
      text: colorFor('var(--color-text)'),
      muted: colorFor('var(--color-text-muted)'),
      error: colorFor('var(--color-error)'),
      errorMuted: colorFor('color-mix(in srgb, var(--color-error) 55%, var(--color-text-muted))'),
    };
    probe.remove();
    return colors;
  });
}

async function toolCallAppearance(scope) {
  return scope.locator('.tool-call[data-tool-call-id]').evaluateAll((nodes) => nodes.map((node) => {
    const name = node.querySelector('.tool-call-toggle, strong');
    const code = node.querySelector(':scope > code');
    const span = code ? code.querySelector(':scope > span') : null;
    return {
      id: node.getAttribute('data-tool-call-id') ?? '',
      elided: node.classList.contains('tool-elided'),
      errored: node.classList.contains('tool-error'),
      name: (name?.textContent ?? '').trim(),
      color: name ? getComputedStyle(name).color : '',
      args: code ? (code.textContent ?? '') : '',
      argsColor: code ? getComputedStyle(code).color : '',
      argsSpanColor: span ? getComputedStyle(span).color : '',
      text: (node.textContent ?? '').replace(/\s+/g, ' ').trim(),
    };
  }));
}

async function assertToolCallAppearance(page, scope, expected, colors, label) {
  let observed = [];
  await waitUntil(async () => {
    observed = await toolCallAppearance(scope);
    return observed.length === expected.length && expected.every((want, index) => (
      observed[index].id === want.id &&
      observed[index].elided === want.elided &&
      observed[index].errored === want.errored &&
      observed[index].name === want.name
    ));
  }, {
    timeout: 15000,
    label: `${label} to publish expected tool-call IDs and elision flags`,
  });

  for (let index = 0; index < expected.length; index += 1) {
    const want = expected[index];
    const actual = observed[index];
    const expectedColor = want.elided
      ? (want.errored ? colors.errorMuted : colors.muted)
      : (want.errored ? colors.error : colors.text);
    assert.equal(
      actual.color,
      expectedColor,
      `${label} ${want.id} name color: got ${actual.color}, want ${expectedColor}`,
    );
    const expectedArgsColor = want.errored ? colors.errorMuted : colors.muted;
    if (actual.argsColor) {
      assert.equal(
        actual.argsColor,
        expectedArgsColor,
        `${label} ${want.id} argument code color: got ${actual.argsColor}, want ${expectedArgsColor}`,
      );
    }
    if (actual.argsSpanColor) {
      assert.equal(
        actual.argsSpanColor,
        expectedArgsColor,
        `${label} ${want.id} argument span color: got ${actual.argsSpanColor}, want ${expectedArgsColor}`,
      );
    }
    if (actual.args) {
      assert.ok(
        actual.args.includes('{') && actual.args.includes('}'),
        `${label} ${want.id} must keep literal argument braces, got ${JSON.stringify(actual.args)}`,
      );
    }
    assert.ok(
      !/elided/i.test(actual.text),
      `${label} ${want.id} must not expose a visible elision badge or marker, got ${JSON.stringify(actual.text)}`,
    );
  }
  return observed;
}

async function completedToolDetails(page, expected, colors, label) {
  const details = page.locator('#message-history .completed-tool-calls');
  await waitUntil(async () => (await details.count()) === 1, {
    timeout: 15000,
    label: `${label} to render one completed tool group`,
  });
  const block = details.first();
  assert.equal(await block.evaluate((element) => element.open), false, `${label} must start collapsed`);
  await assertToolCallAppearance(page, block, expected, colors, `${label} collapsed`);
  await block.locator('summary').click();
  await waitUntil(() => block.evaluate((element) => element.open), {
    timeout: 5000,
    label: `${label} to expand`,
  });
  await assertToolCallAppearance(page, block, expected, colors, `${label} expanded`);
  return block;
}

async function assertToolResultBodiesHidden(page, label) {
  const history = page.locator('#message-history');
  assert.equal(
    (await history.innerText()).includes(elisionResultMarker),
    false,
    `${label} must not render successful tool-result bodies`,
  );
  const html = await page.content();
  assert.equal(html.includes(elisionResultMarker), false, `${label} must not render tool-result bodies anywhere`);
  assert.equal(html.includes(elisionMarker), false, `${label} must not render the internal elision marker`);
}

async function assertVisibleToolError(block, toolCallID, label, colors) {
  const call = block.locator(`.tool-call[data-tool-call-id="${toolCallID}"]`);
  const error = call.locator('.tool-call-error');
  assert.equal(await error.count(), 1, `${label} must keep the original tool error element`);
  assert.equal((await error.textContent()).trim(), elisionErrorText, `${label} must preserve the original error text`);
  assert.equal(await call.evaluate((element) => element.classList.contains('tool-error')), true,
    `${label} must mark the errored tool call`);
  if (colors) {
    assert.equal(
      await error.evaluate((element) => getComputedStyle(element).color),
      colors.error,
      `${label} must keep the full error text color`,
    );
  }
  if (!(await call.evaluate((element) => element.classList.contains('expanded')))) {
    await call.locator('.tool-call-toggle').click();
  }
  await waitUntil(() => error.isVisible(), {
    timeout: 5000,
    label: `${label} to show the original tool error after expanding its tool call`,
  });
}

function historyRowForChat(page, chatID) {
  return page.locator(`#history-entries .history-entry:has(.history-title-link[href="/?chat=${chatID}"])`).first();
}

async function createPersistedChat(page, provider, prompt, reply) {
  const { chatID } = await openChat(page);
  const callIndex = provider.calls.length;
  await submitPrompt(page, prompt);
  await waitCompletionActive(page);
  await provider.waitForCallCount(callIndex + 1);
  await provider.release(callIndex, finalResponse(reply));
  await waitCompletionDone(page);
  await waitForHistoryText(page, reply);
  return { chatID, prompt, reply };
}

async function historyDeleteState(row) {
  return row.locator('.history-delete').evaluate((button) => {
    const spinner = button.querySelector('.history-delete-spinner');
    const svg = button.querySelector('svg');
    const rect = button.getBoundingClientRect();
    return {
      pending: button.classList.contains('htmx-request'),
      indicator: button.getAttribute('hx-indicator'),
      spinnerCount: button.querySelectorAll('.history-delete-spinner').length,
      spinnerAriaHidden: spinner?.getAttribute('aria-hidden') ?? '',
      spinnerVisible: Boolean(spinner?.getClientRects().length),
      spinnerAnimation: spinner ? getComputedStyle(spinner, '::before').animationName : '',
      svgCount: button.querySelectorAll('svg').length,
      svgVisible: Boolean(svg?.getClientRects().length),
      svgDisplay: svg ? getComputedStyle(svg).display : '',
      width: rect.width,
      height: rect.height,
    };
  });
}

async function historyLoaderState(loader) {
  return loader.evaluate((node) => {
    const spinner = node.querySelector('.braille-spinner');
    const loaderStyle = getComputedStyle(node);
    const spinnerStyle = spinner ? getComputedStyle(spinner) : null;
    return {
      pending: node.classList.contains('htmx-request'),
      indicator: node.getAttribute('hx-indicator'),
      spinnerCount: node.querySelectorAll('.braille-spinner').length,
      spinnerVisible: Boolean(spinner?.getClientRects().length),
      loaderFontSize: Number.parseFloat(loaderStyle.fontSize),
      spinnerFontSize: spinnerStyle ? Number.parseFloat(spinnerStyle.fontSize) : 0,
      spinnerAnimation: spinner ? getComputedStyle(spinner, '::before').animationName : '',
    };
  });
}

async function assertGlobalRequestOverlayInactive(page, label) {
  const overlay = page.locator('#request-overlay');
  await waitUntil(async () => {
    return overlay.evaluate((element) => !element.classList.contains('htmx-request')).catch(() => false);
  }, {
    timeout: 10000,
    label: `${label}: global request overlay must not be active`,
  });
  assert.equal(await overlay.isVisible(), false, `${label}: global request overlay must stay hidden`);
}

async function openMessageEditor(page) {
  const message = page.locator('#message-history .message.user').first();
  await message.scrollIntoViewIfNeeded();
  await message.locator('.message-edit-toggle').click();
  await message.locator('.message-edit-input').waitFor({ state: 'visible' });
  return message;
}

async function assertMessageEditActions(message, label) {
  const save = message.locator('.message-edit-save');
  const cancel = message.locator('.message-edit-cancel');
  assert.equal(await save.isVisible(), true, `${label} message save must be visible while editing`);
  assert.equal(await cancel.isVisible(), true, `${label} message cancel must be visible while editing`);
  assert.equal(await save.getAttribute('type'), 'submit', `${label} message checkmark must submit`);
  assert.equal(await cancel.getAttribute('type'), 'button', `${label} message X must not submit`);
  assert.equal(
    await save.getAttribute('form'),
    await message.locator('.message-edit-form').getAttribute('id'),
    `${label} message checkmark must submit the edit form`,
  );
  assert.equal(
    await save.evaluate((button) => button.nextElementSibling?.classList.contains('message-edit-cancel')),
    true,
    `${label} message checkmark must sit immediately before the X`,
  );

  const saveMetrics = await elementMetrics(save);
  const cancelMetrics = await elementMetrics(cancel);
  assertClose(saveMetrics.width, cancelMetrics.width, 0.5, `${label} message edit button widths`);
  assertClose(saveMetrics.height, cancelMetrics.height, 0.5, `${label} message edit button heights`);
  assertClose(
    saveMetrics.y + saveMetrics.height / 2,
    cancelMetrics.y + cancelMetrics.height / 2,
    0.5,
    `${label} message edit button vertical centers`,
  );
  const gap = cancelMetrics.x - saveMetrics.right;
  assert.ok(gap >= -0.5 && gap <= 10, `${label} message save/X gap must stay compact, got ${gap}px`);
  assert.deepEqual(await iconPaths(save), ['M20 6 9 17l-5-5'], `${label} message save icon path`);
  assert.deepEqual(
    await iconPaths(cancel),
    ['M18 6 6 18', 'm6 6 12 12'],
    `${label} message cancel icon paths`,
  );
}

async function submitMessageEdit(page, provider, message, text, method, label) {
  const input = message.locator('.message-edit-input');
  await input.fill(text);
  const callsBefore = provider.calls.length;
  const responsePromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'PUT' && /^\/chats\/\d+\/messages\/\d+$/.test(url.pathname);
  }, { timeout: 15000 });
  if (method === 'click') {
    await message.locator('.message-edit-save').click();
  } else {
    await input.press('Enter');
  }
  const response = await responsePromise;
  const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
  assert.ok(
    response.status() >= 200 && response.status() < 300,
    `${label} message edit returned ${response.status()}${body ? `: ${body}` : ''}`,
  );
  await waitForHistoryText(page, text);
  await waitCompletionActive(page);
  await provider.waitForCallCount(callsBefore + 1);
  await provider.release(callsBefore, finalResponse(`answer after ${label}`));
  await waitCompletionDone(page);
  await waitForHistoryText(page, `answer after ${label}`);
}

function isTitleGenerationCall(call) {
  return call.messages[0]?.role === 'system' &&
    call.messages[0]?.content === titleGenerationSystemPrompt;
}

async function enableChatTitleGeneration(page, chatID) {
  await page.locator('#settings-button').click();
  await waitForPanels(page, 'settings-page');

  const titleToggle = page.locator('#title-generation-enabled');
  await titleToggle.waitFor({ state: 'visible' });
  assert.equal(await titleToggle.isChecked(), false, 'title generation must default to unchecked');

  const defaultModel = page.locator('#default-model');
  const titleModel = page.locator('#title-generation-model');
  assert.equal(await defaultModel.count(), 1, 'default model control is missing');
  assert.equal(await titleModel.count(), 1, 'title model control is missing');

  const optionValues = (locator) => locator.locator('option').evaluateAll(
    (options) => options.map((option) => option.value.trim()).filter(Boolean),
  );
  const defaultModels = await optionValues(defaultModel);
  const titleModels = await optionValues(titleModel);
  assert.ok(defaultModels.length > 0, 'default model list must not be empty');
  assert.deepEqual(titleModels, defaultModels, 'title model options must match the default model options');
  const configuredTitleModel = titleModels[0];

  await titleModel.selectOption(configuredTitleModel);
  await titleToggle.check();
  assert.equal(await titleToggle.isChecked(), true, 'title generation toggle did not turn on');

  await page.locator('#settings-page .settings-save').click();
  await page.locator('[data-settings-saved]').waitFor({ state: 'visible', timeout: 15000 });
  await page.locator('#settings-button').click();
  await waitForPanels(page, null);

  await reloadChat(page, chatID);
  await page.locator('#settings-button').click();
  await waitForPanels(page, 'settings-page');
  assert.equal(
    await page.locator('#title-generation-enabled').isChecked(),
    true,
    'enabled title generation did not persist across reload',
  );
  assert.equal(
    await page.locator('#title-generation-model').inputValue(),
    configuredTitleModel,
    'configured title model did not persist across reload',
  );
  await page.locator('#settings-button').click();
  await waitForPanels(page, null);
  return configuredTitleModel;
}

async function waitForConcurrentCompletionCalls(provider) {
  const startedAt = Date.now();
  const calls = await waitUntil(() => {
    const title = provider.calls.find(isTitleGenerationCall);
    const primary = provider.calls.find((call) => !isTitleGenerationCall(call));
    return title !== undefined && primary !== undefined ? { title, primary } : false;
  }, {
    timeout: 15000,
    label: 'primary completion and independent title provider requests to run concurrently before the primary response is released',
  });
  return { ...calls, elapsed: Date.now() - startedAt };
}

function trackChatTitleHTTPRequests(page) {
  const state = { started: [], finished: [], failed: [], responses: [] };
  const isTitleRequest = (request) => {
    if (request.method() !== 'POST') {
      return false;
    }
    return /^\/chats\/\d+\/title$/.test(new URL(request.url()).pathname);
  };
  page.on('request', (request) => {
    if (isTitleRequest(request)) {
      state.started.push(request);
    }
  });
  page.on('requestfinished', (request) => {
    if (isTitleRequest(request)) {
      state.finished.push(request);
    }
  });
  page.on('requestfailed', (request) => {
    if (isTitleRequest(request)) {
      state.failed.push(request);
    }
  });
  page.on('response', (response) => {
    if (isTitleRequest(response.request())) {
      state.responses.push(response);
    }
  });
  return state;
}

async function chatSummaryHTML(env, chatID, label) {
  const response = await env.page.request.get(`${env.baseURL}/chats/${chatID}/summary`);
  const html = await response.text();
  assert.equal(response.status(), 200, `${label} summary returned ${response.status()}`);
  return html;
}

async function selectTheme(page, themeID) {
  await page.locator('#settings-button').click();
  await waitForPanels(page, 'settings-page');
  await page.locator('#theme').selectOption(themeID);
  await page.locator('#settings-page .settings-save').click();
  await page.locator('[data-settings-saved]').waitFor({ state: 'visible', timeout: 15000 });
  await waitUntil(async () => page.evaluate((expected) => document.documentElement.dataset.theme === expected, themeID), {
    timeout: 10000,
    label: `document theme ${themeID} to apply after saving settings`,
  });
  await page.locator('#settings-button').click();
  await waitForPanels(page, null);
}

async function renameChat(page, chatID, title) {
  const status = await page.evaluate(async ({ id, nextTitle }) => {
    const body = new URLSearchParams({ title: nextTitle });
    const response = await fetch(`/chats/${id}?current=${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
    });
    return response.status;
  }, { id: chatID, nextTitle: title });
  assert.ok(status >= 200 && status < 300, `chat rename returned ${status}`);
}

async function capturePublicScreenshot(t, page, label) {
  const directory = path.join(resultsRoot, 'screenshots');
  await fs.mkdir(directory, { recursive: true });
  const file = path.join(directory, `${Date.now()}-${label}.png`);
  const frame = page.locator(shareUI.frameSelector);
  if (await frame.isVisible().catch(() => false)) {
    await frame.screenshot({ path: file });
  } else {
    await page.screenshot({ path: file, fullPage: true });
  }
  const message = `Public share ${label} screenshot: ${file}`;
  t.diagnostic(message);
  console.log(message);
  return file;
}

function parseShareLink(value, baseURL, label = 'share link') {
  const match = /^\/s\/([A-Za-z0-9_-]{43})#key=([A-Za-z0-9_-]{43})$/.exec(value ?? '');
  assert.ok(
    match,
    `${label} must contain a canonical 43-character token and a #key= canonical 32-byte key, got ${JSON.stringify(value)}`,
  );
  const token = match[1];
  const key = match[2];
  assert.equal(Buffer.from(token, 'base64url').length, 32, `${label} token must decode to 32 bytes`);
  assert.equal(Buffer.from(key, 'base64url').length, 32, `${label} fragment key must decode to 32 bytes`);
  return {
    token,
    key,
    sharePath: `/s/${token}`,
    hash: `#key=${key}`,
    shareURL: new URL(`/s/${token}#key=${key}`, baseURL).href,
    rawURL: new URL(`/s/${token}`, baseURL).href,
  };
}

function parsePreparedShareHTML(html, label) {
  assert.match(html, new RegExp(`\\b${shareUI.preparationAttribute}\\b`),
    `${label} must return the inert preparation fragment`);
  const token = new RegExp(`\\b${shareUI.tokenAttribute}="([A-Za-z0-9_-]{43})"`).exec(html)?.[1];
  assert.ok(token, `${label} preparation must include a canonical ${shareUI.tokenAttribute}`);
  const snapshotBase64 = new RegExp(
    `<template[^>]*\\b${shareUI.snapshotAttribute}\\b[^>]*>([A-Za-z0-9_-]+)</template>`,
  ).exec(html)?.[1];
  assert.ok(snapshotBase64, `${label} preparation must include a base64url ${shareUI.snapshotAttribute} template`);
  const snapshotBytes = Buffer.from(snapshotBase64, 'base64url');
  assert.ok(snapshotBytes.length > 0, `${label} prepared snapshot must not be empty`);
  assert.equal(snapshotBytes.toString('base64url'), snapshotBase64, `${label} prepared snapshot must be canonical base64url`);
  return {
    token,
    snapshotBase64,
    snapshotHTML: snapshotBytes.toString('utf8'),
  };
}

async function performShare(page, shareButton, shareAction) {
  const preparePath = new URL(shareAction, page.url()).pathname;
  const publishPath = `${preparePath}/publish`;
  const preparePromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'POST' && url.pathname === preparePath;
  }, { timeout: 20000 });
  const publishPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'POST' && url.pathname === publishPath;
  }, { timeout: 20000 });

  await shareButton.click();
  const prepareResponse = await preparePromise;
  const publishResponse = await publishPromise;
  const prepareBody = prepareResponse.status() >= 400 ? await prepareResponse.text().catch(() => '') : '';
  const publishBody = publishResponse.status() >= 400 ? await publishResponse.text().catch(() => '') : '';
  assert.match(prepareResponse.headers()['content-type'] ?? '', /^text\/html/,
    'share preparation must return a hypermedia HTML fragment');
  assert.match(publishResponse.headers()['content-type'] ?? '', /^text\/html/,
    'share publish must return a hypermedia HTML fragment');
  assert.ok(
    prepareResponse.status() >= 200 && prepareResponse.status() < 300,
    `POST ${preparePath} returned ${prepareResponse.status()}${prepareBody ? `: ${prepareBody}` : ''}`,
  );
  assert.ok(
    publishResponse.status() >= 200 && publishResponse.status() < 300,
    `POST ${publishPath} returned ${publishResponse.status()}${publishBody ? `: ${publishBody}` : ''}`,
  );
  const prepareHTML = await prepareResponse.text();
  const publishHTML = await publishResponse.text();
  assert.ok(!prepareHTML.includes('#key='), 'prepare response must not contain a fragment key');
  assert.ok(!publishHTML.includes('#key='), 'publish response must not contain a fragment key');
  return {
    prepareRequest: prepareResponse.request(),
    publishRequest: publishResponse.request(),
    prepareHTML,
    publishHTML,
    prepare: parsePreparedShareHTML(prepareHTML, 'share preparation response'),
  };
}

function parsePublishedShareRequest(request, label) {
  const body = request.postData();
  assert.ok(body, `${label} publish request must contain a form body`);
  const params = new URLSearchParams(body);
  assert.deepEqual(
    [...new Set(params.keys())].sort(),
    ['ciphertext', 'format_version', 'nonce', 'token'],
    `${label} publish form must contain exactly token, format_version, nonce, and ciphertext`,
  );
  for (const name of ['token', 'format_version', 'nonce', 'ciphertext']) {
    assert.equal(params.getAll(name).length, 1, `${label} publish form must contain one ${name} value`);
  }
  const token = params.get('token');
  assert.match(token ?? '', /^[A-Za-z0-9_-]{43}$/, `${label} publish token must be canonical`);
  assert.equal(Buffer.from(token, 'base64url').length, 32, `${label} publish token must decode to 32 bytes`);
  assert.equal(params.get('format_version'), '1', `${label} publish payload must use format version 1`);
  const nonce = params.get('nonce');
  const nonceBytes = Buffer.from(nonce ?? '', 'base64url');
  assert.equal(nonceBytes.length, 12, `${label} publish nonce must decode to 12 bytes`);
  assert.equal(nonceBytes.toString('base64url'), nonce, `${label} publish nonce must be canonical base64url`);
  const ciphertext = params.get('ciphertext');
  const ciphertextBytes = Buffer.from(ciphertext ?? '', 'base64url');
  assert.ok(ciphertextBytes.length >= 16, `${label} publish ciphertext must include the 16-byte GCM tag`);
  assert.equal(
    ciphertextBytes.toString('base64url'),
    ciphertext,
    `${label} publish ciphertext must be canonical base64url`,
  );
  return {
    token,
    formatVersion: params.get('format_version'),
    nonce,
    nonceBytes,
    ciphertext,
    ciphertextBytes,
    body,
    params,
  };
}

function decryptPublishedShare({ key, nonce, ciphertext }, label) {
  const keyBytes = Buffer.from(key, 'base64url');
  const nonceBytes = Buffer.from(nonce, 'base64url');
  const ciphertextBytes = Buffer.from(ciphertext, 'base64url');
  assert.equal(keyBytes.length, 32, `${label} decryption key must be 32 bytes`);
  assert.equal(nonceBytes.length, 12, `${label} decryption nonce must be 12 bytes`);
  assert.ok(ciphertextBytes.length >= 16, `${label} ciphertext must include the GCM tag`);
  const tag = ciphertextBytes.subarray(ciphertextBytes.length - 16);
  const encrypted = ciphertextBytes.subarray(0, ciphertextBytes.length - 16);
  const decipher = crypto.createDecipheriv('aes-256-gcm', keyBytes, nonceBytes);
  decipher.setAAD(Buffer.alloc(0));
  decipher.setAuthTag(tag);
  return Buffer.concat([decipher.update(encrypted), decipher.final()]).toString('utf8');
}

function assertPublishedSharePayload(published, prepared, key, plaintextMarkers, label) {
  assert.equal(published.token, prepared.token, `${label} publish token must match the prepared token`);
  assert.equal(published.ciphertextBytes.includes(Buffer.from(key)), false, `${label} ciphertext must not contain the key`);
  for (const marker of plaintextMarkers) {
    assert.equal(
      published.ciphertextBytes.includes(Buffer.from(marker)),
      false,
      `${label} ciphertext must not contain plaintext marker ${JSON.stringify(marker)}`,
    );
    assert.ok(
      !published.body.includes(marker),
      `${label} publish request body must not contain plaintext marker ${JSON.stringify(marker)}`,
    );
  }
  const decrypted = decryptPublishedShare({
    key,
    nonce: published.nonce,
    ciphertext: published.ciphertext,
  }, label);
  assert.equal(decrypted, prepared.snapshotHTML, `${label} independent AES-GCM decryption must reproduce the prepared snapshot`);
  for (const marker of plaintextMarkers) {
    assert.ok(decrypted.includes(marker), `${label} decrypted snapshot must contain ${JSON.stringify(marker)}`);
  }
  return decrypted;
}

function parseShareBootstrapHTML(html, label) {
  const templateTag = new RegExp(`<template[^>]*\\bid="${shareUI.payloadID}"[^>]*>`).exec(html)?.[0];
  assert.ok(templateTag, `${label} must contain the deterministic shared-response payload template`);
  const attribute = (name) => new RegExp(`\\b${name}="([^"]*)"`).exec(templateTag)?.[1];
  const version = attribute(shareUI.versionAttribute);
  const nonce = attribute(shareUI.nonceAttribute);
  const ciphertext = attribute(shareUI.ciphertextAttribute);
  assert.equal(version, '1', `${label} bootstrap must declare format version 1`);
  assert.ok(nonce, `${label} bootstrap must embed the nonce`);
  assert.ok(ciphertext, `${label} bootstrap must embed the ciphertext`);
  const nonceBytes = Buffer.from(nonce, 'base64url');
  const ciphertextBytes = Buffer.from(ciphertext, 'base64url');
  assert.equal(nonceBytes.length, 12, `${label} bootstrap nonce must decode to 12 bytes`);
  assert.equal(nonceBytes.toString('base64url'), nonce, `${label} bootstrap nonce must be canonical base64url`);
  assert.ok(ciphertextBytes.length >= 16, `${label} bootstrap ciphertext must include the GCM tag`);
  assert.equal(
    ciphertextBytes.toString('base64url'),
    ciphertext,
    `${label} bootstrap ciphertext must be canonical base64url`,
  );
  return {
    version,
    nonce,
    nonceBytes,
    ciphertext,
    ciphertextBytes,
  };
}

function assertShareBootstrapMatches(bootstrap, published, label) {
  assert.equal(bootstrap.version, String(published.formatVersion), `${label} bootstrap format version changed`);
  assert.equal(bootstrap.nonce, published.nonce, `${label} bootstrap nonce must match the published nonce`);
  assert.equal(bootstrap.ciphertext, published.ciphertext, `${label} bootstrap ciphertext must match the published ciphertext`);
}

function assertShareCSP(headers, label) {
  const csp = headers['content-security-policy'] ?? '';
  assert.ok(csp, `${label} must send a Content-Security-Policy`);
  assert.match(csp, /default-src 'none'/, `${label} CSP must deny resources by default`);
  const scriptDirective = csp.split(';').map((part) => part.trim()).find((part) => part.startsWith('script-src'));
  assert.ok(scriptDirective, `${label} CSP must declare script-src`);
  assert.match(scriptDirective, /'sha256-[A-Za-z0-9+/=]+'/, `${label} CSP must authorize the inline decryptor by hash`);
  assert.ok(!scriptDirective.includes("'unsafe-inline'"), `${label} CSP must not allow unsafe inline scripts`);
  assert.ok(!scriptDirective.includes("'unsafe-eval'"), `${label} CSP must not allow eval`);
}

function assertNoSecretInRequests(requests, secrets, label) {
  for (const request of requests) {
    const url = request.url();
    const headers = JSON.stringify(request.headers());
    const postData = request.postData() ?? '';
    let postDataBuffer = null;
    try {
      postDataBuffer = request.postDataBuffer();
    } catch {
      postDataBuffer = null;
    }
    for (const secret of secrets) {
      assert.ok(!url.includes(secret), `${label} leaked a secret in ${request.method()} ${url}`);
      assert.ok(!headers.includes(secret), `${label} leaked a secret in request headers for ${url}`);
      assert.ok(!postData.includes(secret), `${label} leaked a secret in request body for ${url}`);
      if (postDataBuffer) {
        assert.equal(
          postDataBuffer.includes(Buffer.from(secret)),
          false,
          `${label} leaked a secret in the raw request body for ${url}`,
        );
      }
    }
  }
}

function assertNoPlaintext(html, markers, label) {
  for (const marker of markers) {
    assert.ok(!html.includes(marker), `${label} must not contain plaintext marker ${JSON.stringify(marker)}`);
  }
}

async function prepareShareFixture(page, baseURL, shareAction) {
  const response = await page.request.post(new URL(shareAction, baseURL).href, {
    form: {},
    headers: { 'HX-Request': 'true' },
    maxRedirects: 0,
  });
  const html = await response.text();
  assert.equal(response.status(), 200, `prepare fixture returned ${response.status()}: ${html}`);
  assert.match(response.headers()['content-type'] ?? '', /^text\/html/, 'prepare fixture must return HTML');
  return parsePreparedShareHTML(html, 'prepare fixture');
}

async function encryptShareFixture(page, html) {
  return page.evaluate(async (plaintext) => {
    const encoder = new TextEncoder();
    const rawKey = crypto.getRandomValues(new Uint8Array(32));
    const nonce = crypto.getRandomValues(new Uint8Array(12));
    const key = await crypto.subtle.importKey(
      'raw',
      rawKey,
      { name: 'AES-GCM' },
      false,
      ['encrypt'],
    );
    const ciphertext = new Uint8Array(await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: nonce, tagLength: 128 },
      key,
      encoder.encode(plaintext),
    ));
    const base64url = (bytes) => {
      let binary = '';
      for (const byte of bytes) {
        binary += String.fromCharCode(byte);
      }
      return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    };
    return {
      key: base64url(rawKey),
      nonce: base64url(nonce),
      ciphertext: base64url(ciphertext),
    };
  }, html);
}

async function publishShareFixture(page, baseURL, shareAction, prepared, encrypted) {
  const response = await page.request.post(new URL(`${shareAction}/publish`, baseURL).href, {
    form: {
      token: prepared.token,
      format_version: '1',
      nonce: encrypted.nonce,
      ciphertext: encrypted.ciphertext,
    },
    headers: { 'HX-Request': 'true' },
    maxRedirects: 0,
  });
  const html = await response.text();
  assert.equal(response.status(), 200, `publish fixture returned ${response.status()}: ${html}`);
  const sharePath = /data-share-url="(\/s\/[A-Za-z0-9_-]{43})"/.exec(html)?.[1];
  assert.ok(sharePath, 'publish fixture must return a bare canonical share path');
  assert.ok(!html.includes('#key='), 'publish fixture response must not contain the fragment key');
  assert.equal(sharePath, `/s/${prepared.token}`, 'publish fixture must retain the prepared token');
  return {
    sharePath,
    shareURL: `${new URL(sharePath, baseURL).href}#key=${encrypted.key}`,
    html,
  };
}

async function waitForShareState(page, expected, label) {
  await waitUntil(async () => (
    (await page.locator(shareUI.rootSelector).getAttribute(shareUI.stateAttribute).catch(() => null)) === expected
  ), {
    timeout: 15000,
    label,
  });
}

async function waitForSharedFrame(page, label) {
  const frameElement = page.locator(shareUI.frameSelector);
  await frameElement.waitFor({ state: 'visible', timeout: 15000 });
  const frame = page.frameLocator(shareUI.frameSelector);
  await frame.locator('.shared-page').waitFor({ state: 'visible', timeout: 15000 });
  return { frameElement, frame };
}

async function assertFrameSandbox(frameElement, label) {
  const sandbox = await frameElement.getAttribute('sandbox');
  assert.ok(sandbox !== null, `${label} iframe must declare a sandbox`);
  const tokens = sandbox.split(/\s+/).filter(Boolean);
  assert.ok(tokens.includes('allow-popups'), `${label} sandbox must allow popups`);
  assert.ok(tokens.includes('allow-popups-to-escape-sandbox'), `${label} sandbox must let popups escape the sandbox`);
  assert.ok(!tokens.includes('allow-scripts'), `${label} sandbox must not allow scripts`);
  assert.ok(!tokens.includes('allow-same-origin'), `${label} sandbox must not allow same-origin access`);
}

async function assertOpaqueFrameOrigin(page, frameElement, label) {
  const parent = await page.evaluate((selector) => {
    const frame = document.querySelector(selector);
    let windowAccess = 'none';
    try {
      void frame.contentWindow.document.body;
      windowAccess = 'allowed';
    } catch (error) {
      windowAccess = error.name;
    }
    return {
      contentDocument: frame.contentDocument === null ? 'null' : 'non-null',
      windowAccess,
    };
  }, shareUI.frameSelector);
  assert.equal(parent.contentDocument, 'null', `${label} parent must not read iframe.contentDocument`);
  assert.equal(parent.windowAccess, 'SecurityError', `${label} parent contentWindow.document must throw SecurityError`);

  const handle = await frameElement.elementHandle();
  const contentFrame = await handle.contentFrame();
  assert.ok(contentFrame, `${label} iframe content frame must exist`);
  const child = await contentFrame.evaluate(() => {
    let parentAccess = 'none';
    let topAccess = 'none';
    try {
      void parent.document.body;
      parentAccess = 'allowed';
    } catch (error) {
      parentAccess = error.name;
    }
    try {
      void top.document.body;
      topAccess = 'allowed';
    } catch (error) {
      topAccess = error.name;
    }
    return {
      origin: location.origin,
      parentAccess,
      topAccess,
    };
  });
  assert.equal(child.parentAccess, 'SecurityError', `${label} iframe must not read parent.document`);
  assert.equal(child.topAccess, 'SecurityError', `${label} iframe must not read top.document`);
}

async function assertShareFails(page, url, markers, label) {
  await page.goto(url, { waitUntil: 'load' });
  await waitForShareState(page, 'error', `${label} to report a decryption error`);
  const status = page.locator(shareUI.statusSelector);
  await status.waitFor({ state: 'visible', timeout: 5000 });
  assert.ok((await status.innerText()).trim().length > 0, `${label} must display a decryption failure message`);
  const frame = page.frameLocator(shareUI.frameSelector);
  assert.equal(await frame.locator('.shared-page').count(), 0, `${label} must not render decrypted plaintext`);
  assertNoPlaintext(await page.content(), markers, label);
}

function tamperShareBootstrap(html, label) {
  const bootstrap = parseShareBootstrapHTML(html, label);
  const bytes = Buffer.from(bootstrap.ciphertext, 'base64url');
  bytes[0] ^= 0x01;
  const tamperedCiphertext = bytes.toString('base64url');
  assert.notEqual(tamperedCiphertext, bootstrap.ciphertext, `${label} tampered ciphertext must differ`);
  const parts = html.split(bootstrap.ciphertext);
  assert.equal(parts.length, 2, `${label} ciphertext must occur exactly once in the bootstrap`);
  return {
    bootstrap,
    tamperedCiphertext,
    tamperedHTML: parts.join(tamperedCiphertext),
  };
}

async function assertIconOnlyShareButton(button, label) {
  assert.equal((await button.innerText()).trim(), '', `${label} must not show visible text`);
  assert.equal(await button.locator('svg').count(), 1, `${label} must render exactly one SVG icon`);
  assert.ok((await button.locator('svg path').count()) > 0, `${label} SVG must render icon paths`);
}

async function waitForSharedResponse(actions, baseURL) {
  const copyButton = actions.locator('.message-share-copy-button');
  await waitUntil(async () => (await copyButton.count()) === 1 && (await copyButton.isVisible()), {
    timeout: 15000,
    label: 'share copy action to appear after creating the public snapshot',
  });
  assert.equal(await actions.locator('button').count(), 1, 'sharing must render exactly one icon button');
  assert.equal(await actions.locator('a').count(), 0, 'sharing must not render links');
  assert.equal(await actions.locator('.message-share-link').count(), 0, 'sharing must not render a public link');
  for (const label of ['Open link', 'Copy link']) {
    assert.equal(
      await actions.getByText(label, { exact: true }).count(),
      0,
      `sharing must not render visible ${label} text`,
    );
  }
  assert.equal(await copyButton.getAttribute('type'), 'button', 'copy action must be a non-submitting button');
  const classes = (await copyButton.getAttribute('class') ?? '').split(/\s+/);
  assert.ok(classes.includes('message-share-button'), 'copy action must retain the share button class');
  assert.ok(classes.includes('message-share-copy-button'), 'copy action must expose its copy class');
  assert.equal(await copyButton.getAttribute('aria-label'), 'Copy share link');
  await assertIconOnlyShareButton(copyButton, 'copy share link');
  const link = parseShareLink(await copyButton.getAttribute('data-share-url'), baseURL, 'copy share link');
  assert.equal(new URL(link.shareURL).origin, new URL(baseURL).origin, 'shared response must use the application origin');
  return {
    copyButton,
    ...link,
  };
}

async function waitForShareStatus(page, status, expected, { timeout = 6000, label } = {}) {
  const element = await status.elementHandle();
  assert.ok(element, 'share status element is missing');
  await page.waitForFunction(
    ({ element: statusElement, expected: expectedText }) => (
      (statusElement.textContent ?? '').trim() === expectedText
    ),
    { element, expected },
    { timeout },
  ).catch((error) => {
    throw new Error(`Timed out waiting for ${label ?? `share status ${JSON.stringify(expected)}`}: ${error.message}`);
  });
}

async function assertShareStatusClears(page, status, copiedAt, label) {
  await waitForShareStatus(page, status, '', {
    timeout: 6000,
    label: `${label} to clear`,
  });
  const elapsed = Date.now() - copiedAt;
  assert.ok(elapsed >= 3800, `${label} cleared after ${elapsed}ms, before the four-second feedback window`);
  assert.ok(elapsed <= 5500, `${label} cleared after ${elapsed}ms, later than the tolerated feedback window`);
  return elapsed;
}

async function assertNoShareActions(scope, label) {
  assert.equal(await scope.locator('.message-share-actions').count(), 0, `${label} must not render share actions`);
  assert.equal(
    await scope.getByRole('button', { name: 'Share response' }).count(),
    0,
    `${label} must not render a Share response button`,
  );
}

async function shortTableWordLineCounts(frame) {
  return frame.locator('.shared-page').evaluate(() => {
    const lineCount = (element) => {
      const range = document.createRange();
      range.selectNodeContents(element);
      const tops = [];
      for (const rect of range.getClientRects()) {
        if (rect.width <= 0 || rect.height <= 0) {
          continue;
        }
        if (!tops.some((top) => Math.abs(top - rect.top) < 0.5)) {
          tops.push(rect.top);
        }
      }
      return tops.length;
    };
    const header = document.querySelector('.shared-article table th');
    const firstDataCell = document.querySelector('.shared-article table td');
    return {
      headerText: header?.textContent.trim() ?? '',
      headerLines: header ? lineCount(header) : 0,
      firstDataText: firstDataCell?.textContent.trim() ?? '',
      firstDataLines: firstDataCell ? lineCount(firstDataCell) : 0,
    };
  });
}

function assertShortTableWordsOneLine(counts, label) {
  assert.deepEqual(
    counts,
    {
      headerText: 'Item',
      headerLines: 1,
      firstDataText: 'Alpha',
      firstDataLines: 1,
    },
    `${label} short table words must each render on one line: ${JSON.stringify(counts)}`,
  );
}

function assertTitleGenerationRequest(call, { model, prompt }) {
  assert.equal(call.model, model, 'title request did not use the configured title model');
  assert.deepEqual(
    call.messages.map((message) => message.role),
    ['system', 'user'],
    'title request must contain only the title system prompt and the first user message',
  );
  assert.equal(call.messages[0].content, titleGenerationSystemPrompt, 'title request system prompt changed');
  assert.equal(call.messages[1].content, prompt, 'title request must use the first user message verbatim');
  assert.equal(call.tools.length, 0, 'title request must not send any tools');
}

function currentHistoryTitleState(page) {
  return page.locator('#history-entries .history-entry.current').evaluate((entry) => {
    const text = entry.querySelector('.history-title-link .history-title-text');
    const spinner = entry.querySelector('.history-title-link .history-title-spinner');
    const braille = entry.querySelector('.history-title-link .history-title-spinner .braille-spinner');
    return {
      text: text?.textContent?.trim() ?? '',
      textVisible: Boolean(text?.getClientRects().length),
      spinnerVisible: Boolean(spinner?.getClientRects().length),
      animationName: braille ? getComputedStyle(braille, '::before').animationName : '',
    };
  });
}

async function assertNoGlobalRequestOverlay(page, label) {
  const overlay = page.locator('#request-overlay');
  await waitUntil(async () => {
    return overlay.evaluate((element) => !element.classList.contains('htmx-request')).catch(() => false);
  }, {
    timeout: 10000,
    label: `${label}: global request overlay must not be active for the independent title request`,
  });
  assert.equal(await overlay.isVisible(), false, `${label}: global request overlay must stay hidden while the title is pending`);
}

async function assertHistoryTitlePending(page, fallbackTitle, label) {
  await waitUntil(async () => {
    const state = await currentHistoryTitleState(page).catch(() => null);
    return state && state.text === fallbackTitle && !state.textVisible &&
      state.spinnerVisible && state.animationName === 'braille-spinner' ? state : false;
  }, {
    timeout: 10000,
    label: `${label}: current History title to replace ${JSON.stringify(fallbackTitle)} with the braille pending spinner`,
  });
  await assertNoGlobalRequestOverlay(page, label);
}

async function assertHistoryTitleResolved(page, expectedTitle, label) {
  await waitUntil(async () => {
    const state = await currentHistoryTitleState(page).catch(() => null);
    return state && state.text === expectedTitle && state.textVisible && !state.spinnerVisible ? state : false;
  }, {
    timeout: 10000,
    label: `${label}: current History title to show ${JSON.stringify(expectedTitle)} after the title request`,
  });
  const strongText = (
    await page.locator('#history-entries .history-entry.current .history-title-link strong').first().innerText()
  ).trim();
  assert.equal(strongText, expectedTitle, `${label}: resolved History title strong innerText changed`);
  await assertNoGlobalRequestOverlay(page, label);
}

test('chat title generation updates open History without reopening the panel', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'chat title generation history refresh',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const firstPrompt = 'Help me plan a vegetable garden this spring';
      const assistantReply = 'Here is your garden plan.';
      const generatedTitle = 'Spring vegetable garden plan';

      await signup(page, username('titlehist'), password);
      const { chatID } = await openChat(page);
      const configuredTitleModel = await enableChatTitleGeneration(page, chatID);

      const submittedAt = Date.now();
      await submitPrompt(page, firstPrompt);
      await waitCompletionActive(page);
      const concurrent = await waitForConcurrentCompletionCalls(provider);
      const bothRequestsElapsed = Date.now() - submittedAt;
      const titleRequestOffset = concurrent.title.receivedAt - concurrent.primary.receivedAt;

      assert.equal(
        provider.calls.length,
        2,
        `expected the primary and title requests only, saw ${provider.calls.length} provider calls`,
      );
      assert.notEqual(concurrent.primary.index, concurrent.title.index, 'primary and title calls must be distinct');
      assert.equal(
        concurrent.primary.released,
        false,
        'primary response must remain gated when both concurrent requests are observed',
      );
      assert.equal(concurrent.primary.done, false, 'primary response must not have completed before release');
      assert.equal(concurrent.title.released, false, 'title response must remain gated for the History assertions');
      assert.equal(lastUser(concurrent.primary), firstPrompt, 'primary request must use the submitted first prompt');
      assertTitleGenerationRequest(concurrent.title, { model: configuredTitleModel, prompt: firstPrompt });

      const historyRequests = [];
      page.on('request', (request) => {
        if (request.method() !== 'GET') {
          return;
        }
        const path = new URL(request.url()).pathname;
        if (path === '/history') {
          historyRequests.push(request.url());
        }
      });

      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      const currentHistoryTitle = page.locator('#history-entries .history-entry.current .history-title-link strong');
      await currentHistoryTitle.first().waitFor({ state: 'visible', timeout: 10000 });
      await assertHistoryTitlePending(page, firstPrompt, 'open History title gated by the independent title request');
      assert.equal(historyRequests.length, 1, `opening History should request /history once, saw ${historyRequests.length}`);
      assert.equal(
        concurrent.primary.done,
        false,
        'primary provider response must still be gated while History shows the fallback title',
      );
      assert.equal(
        concurrent.title.done,
        false,
        'title provider response must still be gated while History shows the fallback title',
      );
      const requestsWhileTitleGated = historyRequests.length;
      const gatedScreenshot = await env.screenshot('chat-title-history-gated');
      t.diagnostic(`Title-gated History screenshot: ${gatedScreenshot}`);
      console.log(`Title-gated History screenshot: ${gatedScreenshot}`);

      const titleResponseStartedAt = Date.now();
      await provider.release(concurrent.title.index, finalResponse(generatedTitle));
      const titleUpdate = await waitUntil(async () => {
        const text = (await currentHistoryTitle.first().innerText().catch(() => '')).trim();
        return text === generatedTitle ? { elapsed: Date.now() - titleResponseStartedAt, text } : false;
      }, {
        timeout: 5000,
        label: 'open History title to become the generated title before the primary response completes',
      });
      await assertHistoryTitleResolved(page, generatedTitle, 'open History title after the independent title release');

      assert.equal(
        historyRequests.length,
        requestsWhileTitleGated,
        'an independent title HTTP refresh must not refetch /history',
      );
      assert.equal(await page.locator('#history-page').isVisible(), true, 'History panel must remain open during the title HTTP refresh');
      assert.ok(titleUpdate.elapsed <= 5000, `generated title reached open History after ${titleUpdate.elapsed}ms`);
      assert.equal(
        concurrent.primary.released,
        false,
        'primary provider response must remain gated after the title update',
      );
      assert.equal(
        concurrent.primary.done,
        false,
        'primary provider response must not complete while the title is applied',
      );
      assert.equal(provider.calls.length, 2, `title refresh must not start another provider call, saw ${provider.calls.length}`);
      assert.equal(
        await page.locator('.loading-message.completion-active:not(.completion-failed)').count(),
        1,
        'completion must remain active while the generated title is shown',
      );

      const summaryResponse = await page.request.get(`${env.baseURL}/chats/${chatID}/summary`);
      const summaryHTML = await summaryResponse.text();
      assert.equal(summaryResponse.status(), 200, `chat summary returned ${summaryResponse.status()}`);
      assert.ok(
        summaryHTML.includes(generatedTitle),
        `stored chat summary did not contain the generated title: ${summaryHTML}`,
      );

      const updatedScreenshot = await env.screenshot('chat-title-history-updated');
      t.diagnostic(
        `Title concurrency before primary release: both requests observed ${bothRequestsElapsed}ms after submit, ` +
        `title request offset ${titleRequestOffset}ms; title DOM update ${titleUpdate.elapsed}ms after release; ` +
        `/history requests while open ${historyRequests.length}; summary contains ${generatedTitle}.`,
      );
      t.diagnostic(`Updated History screenshot: ${updatedScreenshot}`);
      console.log(
        `Title concurrency before primary release: both requests observed ${bothRequestsElapsed}ms after submit, ` +
        `title request offset ${titleRequestOffset}ms; title DOM update ${titleUpdate.elapsed}ms after release.`,
      );
      console.log(`Updated History screenshot: ${updatedScreenshot}`);

      const primaryReleaseStartedAt = Date.now();
      await provider.release(concurrent.primary.index, finalResponse(assistantReply));
      await waitCompletionDone(page);
      const completionElapsed = Date.now() - primaryReleaseStartedAt;
      assert.equal(
        await page.locator('.loading-message.completion-active').count(),
        0,
        'completion spinner must disappear after the primary response finishes',
      );

      await page.locator('#history-button').click();
      await waitForPanels(page, null);
      assert.equal(
        await page.locator('#message-history .message').filter({ hasText: assistantReply }).count(),
        1,
        'final assistant response must remain visible exactly once after title generation',
      );

      await reloadChat(page, chatID);
      await waitForHistoryText(page, assistantReply);
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => {
        const text = (await page.locator('#history-entries .history-entry.current .history-title-link strong').first().innerText()).trim();
        return text === generatedTitle;
      }, {
        timeout: 10000,
        label: 'generated title to persist after reload',
      });

      assert.equal(provider.calls.length, 2, `reload and History reopen must not start another provider call, saw ${provider.calls.length}`);
      const totalElapsed = Date.now() - submittedAt;
      t.diagnostic(
        `Title completion: primary settled ${completionElapsed}ms after release; total ${totalElapsed}ms; ` +
        `provider calls ${provider.calls.length}.`,
      );
      console.log(
        `Title completion: primary settled ${completionElapsed}ms after release; total ${totalElapsed}ms; ` +
        `provider calls ${provider.calls.length}.`,
      );
    },
  });
});

test('chat title generation survives a failed first response', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'chat title generation after failed response',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const firstPrompt = 'Build a checklist for a rainy weekend';
      const generatedTitle = 'Rainy weekend checklist';

      await signup(page, username('titlefail'), password);
      const { chatID } = await openChat(page);
      const configuredTitleModel = await enableChatTitleGeneration(page, chatID);
      const titleHTTP = trackChatTitleHTTPRequests(page);

      const submittedAt = Date.now();
      await submitPrompt(page, firstPrompt);
      await waitCompletionActive(page);
      const concurrent = await waitForConcurrentCompletionCalls(provider);
      assert.equal(
        provider.calls.length,
        2,
        `expected the primary and title requests only, saw ${provider.calls.length} provider calls`,
      );
      assert.notEqual(concurrent.primary.index, concurrent.title.index, 'primary and title calls must be distinct');
      assert.equal(
        concurrent.primary.released,
        false,
        'primary response must remain gated when both concurrent requests are observed',
      );
      assert.equal(concurrent.primary.done, false, 'primary response must not have completed before release');
      assert.equal(concurrent.title.released, false, 'title response must remain gated for the History assertions');
      assert.equal(lastUser(concurrent.primary), firstPrompt, 'primary request must use the submitted first prompt');
      assertTitleGenerationRequest(concurrent.title, { model: configuredTitleModel, prompt: firstPrompt });
      await waitUntil(() => titleHTTP.started.length === 1, {
        timeout: 10000,
        label: 'independent title HTTP request to start for the initial message',
      });

      const historyRequests = [];
      page.on('request', (request) => {
        if (request.method() !== 'GET') {
          return;
        }
        const path = new URL(request.url()).pathname;
        if (path === '/history') {
          historyRequests.push(request.url());
        }
      });
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      const currentHistoryTitle = page.locator('#history-entries .history-entry.current .history-title-link strong');
      await currentHistoryTitle.first().waitFor({ state: 'visible', timeout: 10000 });
      await assertHistoryTitlePending(page, firstPrompt, 'open History title gated before the primary failure');
      assert.equal(historyRequests.length, 1, `opening History should request /history once, saw ${historyRequests.length}`);
      assert.equal(titleHTTP.finished.length, 0, 'independent title HTTP request must still be active before the primary failure');
      assert.equal(titleHTTP.failed.length, 0, 'independent title HTTP request must not fail while the primary is gated');
      const requestsWhileTitleGated = historyRequests.length;
      const gatedScreenshot = await env.screenshot('chat-title-failure-history-gated');
      t.diagnostic(`Title-gated failure History screenshot: ${gatedScreenshot}`);
      console.log(`Title-gated failure History screenshot: ${gatedScreenshot}`);

      const failureReleaseStartedAt = Date.now();
      await provider.release(concurrent.primary.index, errorResponse(400, 'fixture first response failure'));
      const failureSettle = await waitUntil(async () => {
        const errors = await page.locator('.completion-error, .loading-message.completion-failed').count();
        return errors === 1 ? { elapsed: Date.now() - failureReleaseStartedAt, errors } : false;
      }, {
        timeout: 5000,
        label: 'completion error to render while the independent title request remains gated',
      });
      assert.ok(
        failureSettle.elapsed <= 3000,
        `completion error rendered ${failureSettle.elapsed}ms after the primary failure release`,
      );
      assert.equal(concurrent.primary.status, 400, `primary provider call finished with status ${concurrent.primary.status}`);
      assert.equal(provider.calls.length, 2, 'the failed primary response must not trigger protocol fallback or another provider call');
      assert.equal(concurrent.title.released, false, 'title response must remain gated after the primary failure');
      assert.equal(concurrent.title.done, false, 'title provider response must not complete after the primary failure');
      assert.equal(titleHTTP.finished.length, 0, 'independent title HTTP request must remain active after the primary failure');
      assert.equal(
        await page.locator('.loading-message.completion-active').count(),
        0,
        'completion must not stay active after the primary failure while the title is gated',
      );
      const failedSummary = await chatSummaryHTML(env, chatID, 'failed completion');
      assert.ok(failedSummary.includes(firstPrompt), `failed completion summary lost the fallback title: ${failedSummary}`);
      assert.ok(!failedSummary.includes(generatedTitle), `failed completion summary unexpectedly contained the title: ${failedSummary}`);

      const failureScreenshot = await env.screenshot('chat-title-failure-history-failed');
      t.diagnostic(
        `Primary error settled ${failureSettle.elapsed}ms after release while the title provider remained gated; ` +
        `summary still showed the fallback title.`,
      );
      t.diagnostic(`Failed-completion History screenshot: ${failureScreenshot}`);
      console.log(
        `Primary error settled ${failureSettle.elapsed}ms after release while the title provider remained gated; ` +
        `summary still showed the fallback title.`,
      );
      console.log(`Failed-completion History screenshot: ${failureScreenshot}`);

      const titleResponseStartedAt = Date.now();
      await provider.release(concurrent.title.index, finalResponse(generatedTitle));
      const titleUpdate = await waitUntil(async () => {
        const text = (await currentHistoryTitle.first().innerText().catch(() => '')).trim();
        return text === generatedTitle ? { elapsed: Date.now() - titleResponseStartedAt, text } : false;
      }, {
        timeout: 5000,
        label: 'open History title to become the generated title after the primary failure',
      });
      await assertHistoryTitleResolved(page, generatedTitle, 'open History title after release despite the primary failure');
      assert.equal(historyRequests.length, requestsWhileTitleGated, 'an independent title HTTP refresh must not refetch /history');
      assert.equal(await page.locator('#history-page').isVisible(), true, 'History panel must remain open during the title HTTP refresh');
      assert.ok(titleUpdate.elapsed <= 5000, `generated title reached open History after ${titleUpdate.elapsed}ms`);
      await waitUntil(() => titleHTTP.finished.length === 1, {
        timeout: 5000,
        label: 'independent title HTTP request to finish after release',
      });
      assert.equal(titleHTTP.failed.length, 0, 'independent title HTTP request must not fail after release');
      assert.equal(provider.calls.length, 2, `title HTTP refresh must not start another provider call, saw ${provider.calls.length}`);
      const titleSummary = await chatSummaryHTML(env, chatID, 'completed title after failure');
      assert.ok(titleSummary.includes(generatedTitle), `failed first response did not persist the generated title: ${titleSummary}`);

      const updatedScreenshot = await env.screenshot('chat-title-failure-history-updated');
      t.diagnostic(
        `Title after primary failure: request offset ${concurrent.title.receivedAt - concurrent.primary.receivedAt}ms, ` +
        `DOM update ${titleUpdate.elapsed}ms after title release; /history requests while open ${historyRequests.length}.`,
      );
      t.diagnostic(`Updated failure History screenshot: ${updatedScreenshot}`);
      console.log(
        `Title after primary failure: request offset ${concurrent.title.receivedAt - concurrent.primary.receivedAt}ms, ` +
        `DOM update ${titleUpdate.elapsed}ms after title release.`,
      );
      console.log(`Updated failure History screenshot: ${updatedScreenshot}`);

      await page.locator('#history-button').click();
      await waitForPanels(page, null);
      await waitCompletionError(page);
      assert.equal(
        await page.locator('.completion-error, .loading-message.completion-failed').count(),
        1,
        'failed primary response must render exactly one completion error',
      );

      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => (await currentHistoryTitle.first().innerText().catch(() => '')).trim() === generatedTitle, {
        timeout: 5000,
        label: 'generated title to remain in History while the completion error is rendered',
      });

      await reloadChat(page, chatID);
      await waitCompletionError(page);
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => {
        const text = (await page.locator('#history-entries .history-entry.current .history-title-link strong').first().innerText()).trim();
        return text === generatedTitle;
      }, {
        timeout: 10000,
        label: 'generated title to survive a failed first response and reload',
      });
      assert.equal(provider.calls.length, 2, `reload after the failed response must not start another provider call, saw ${provider.calls.length}`);
      assert.equal(
        provider.calls.filter(isTitleGenerationCall).length,
        1,
        'the failed completion and reload must not issue another title request',
      );
      assert.equal(titleHTTP.started.length, 1, 'reload must not issue another independent title HTTP request');
      const totalElapsed = Date.now() - submittedAt;
      t.diagnostic(
        `Failure path: primary error settled ${failureSettle.elapsed}ms after release; title DOM update ` +
        `${titleUpdate.elapsed}ms after independent title release; total ${totalElapsed}ms; ` +
        `provider calls ${provider.calls.length}.`,
      );
      console.log(
        `Failure path: primary error settled ${failureSettle.elapsed}ms after release; title DOM update ` +
        `${titleUpdate.elapsed}ms after independent title release; total ${totalElapsed}ms; ` +
        `provider calls ${provider.calls.length}.`,
      );
    },
  });
});

test('chat completion finishes while title generation remains pending', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'completion independent of pending title',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const firstPrompt = 'Plan a weeknight dinner rotation for two';
      const assistantReply = 'Here is your weeknight dinner rotation.';
      const generatedTitle = 'Weeknight dinner rotation';
      const followupPrompt = 'Add one vegetarian option';
      const followupReply = 'Added a vegetarian option.';

      await signup(page, username('titleind'), password);
      const { chatID } = await openChat(page);
      const configuredTitleModel = await enableChatTitleGeneration(page, chatID);
      const titleHTTP = trackChatTitleHTTPRequests(page);

      const submittedAt = Date.now();
      await submitPrompt(page, firstPrompt);
      await waitCompletionActive(page);
      const concurrent = await waitForConcurrentCompletionCalls(provider);
      assert.equal(
        provider.calls.length,
        2,
        `expected the primary and title requests only, saw ${provider.calls.length} provider calls`,
      );
      assert.notEqual(concurrent.primary.index, concurrent.title.index, 'primary and title calls must be distinct');
      assert.equal(concurrent.primary.released, false, 'primary response must remain gated when the title is pending');
      assert.equal(concurrent.primary.done, false, 'primary response must not have completed before release');
      assert.equal(concurrent.title.released, false, 'title response must remain gated for the completion regression');
      assert.equal(concurrent.title.done, false, 'title provider response must not complete before release');
      assert.equal(lastUser(concurrent.primary), firstPrompt, 'primary request must use the submitted first prompt');
      assertTitleGenerationRequest(concurrent.title, { model: configuredTitleModel, prompt: firstPrompt });
      await waitUntil(() => titleHTTP.started.length === 1, {
        timeout: 10000,
        label: 'independent title HTTP request to start for the initial message',
      });
      assert.equal(titleHTTP.finished.length, 0, 'independent title HTTP request must still be active');
      assert.equal(titleHTTP.failed.length, 0, 'independent title HTTP request must not fail while gated');

      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      const currentHistoryTitle = page.locator('#history-entries .history-entry.current .history-title-link strong');
      await currentHistoryTitle.first().waitFor({ state: 'visible', timeout: 10000 });
      await assertHistoryTitlePending(page, firstPrompt, 'open History title gated during the main completion');
      const fallbackSummary = await chatSummaryHTML(env, chatID, 'gated title');
      assert.ok(fallbackSummary.includes(firstPrompt), `gated summary lost the fallback title: ${fallbackSummary}`);
      assert.ok(!fallbackSummary.includes(generatedTitle), `gated summary unexpectedly contained the title: ${fallbackSummary}`);
      const gatedScreenshot = await env.screenshot('chat-title-independent-completion-gated');
      t.diagnostic(`Title-gated independent completion screenshot: ${gatedScreenshot}`);
      console.log(`Title-gated independent completion screenshot: ${gatedScreenshot}`);

      const primaryReleaseStartedAt = Date.now();
      await provider.release(concurrent.primary.index, finalResponse(assistantReply));
      await waitUntil(async () => (await page.locator('.loading-message.completion-active').count()) === 0, {
        timeout: 2000,
        label: 'completion spinner to disappear within 2s while the title request remains gated',
      });
      const completionElapsed = Date.now() - primaryReleaseStartedAt;
      assert.ok(completionElapsed <= 2000, `completion settled ${completionElapsed}ms after primary release`);
      assert.equal(
        await page.locator('.loading-message.completion-active').count(),
        0,
        'completion spinner must disappear after the primary response without waiting for the title',
      );
      assert.equal(
        await page.locator('#message-history .message').filter({ hasText: assistantReply }).count(),
        1,
        'final assistant response must be visible exactly once',
      );
      const transcript = await page.locator('#message-history').textContent();
      assert.equal(
        transcript.split(assistantReply).length - 1,
        1,
        'final assistant response text must appear exactly once in the transcript',
      );
      assert.equal(concurrent.title.released, false, 'title provider response must remain gated after completion');
      assert.equal(concurrent.title.done, false, 'title provider response must not complete with the primary');
      await assertHistoryTitlePending(page, firstPrompt, 'History title to remain pending after the main completion');
      assert.equal(titleHTTP.finished.length, 0, 'independent title HTTP request must remain active after completion');
      assert.equal(titleHTTP.failed.length, 0, 'independent title HTTP request must not fail after completion');
      const summaryAfterMain = await chatSummaryHTML(env, chatID, 'completed main with pending title');
      assert.ok(summaryAfterMain.includes(firstPrompt), `completion summary lost the fallback title: ${summaryAfterMain}`);
      assert.ok(!summaryAfterMain.includes(generatedTitle), `completion summary unexpectedly contained the title: ${summaryAfterMain}`);
      const completedScreenshot = await env.screenshot('chat-title-independent-completion-finished');
      t.diagnostic(
        `Main completion settled ${completionElapsed}ms after primary release with the title provider still gated; ` +
        `independent title HTTP request remained active and the summary kept the fallback title.`,
      );
      t.diagnostic(`Completed-main pending-title screenshot: ${completedScreenshot}`);
      console.log(
        `Main completion settled ${completionElapsed}ms after primary release with the title provider still gated; ` +
        `independent title HTTP request remained active and the summary kept the fallback title.`,
      );
      console.log(`Completed-main pending-title screenshot: ${completedScreenshot}`);

      const titleReleaseStartedAt = Date.now();
      await provider.release(concurrent.title.index, finalResponse(generatedTitle));
      const titleUpdate = await waitUntil(async () => {
        const text = (await currentHistoryTitle.first().innerText().catch(() => '')).trim();
        return text === generatedTitle ? { elapsed: Date.now() - titleReleaseStartedAt, text } : false;
      }, {
        timeout: 5000,
        label: 'open History title to update after the main completion already finished',
      });
      await assertHistoryTitleResolved(page, generatedTitle, 'open History title after release following the main completion');
      await waitUntil(() => titleHTTP.finished.length === 1, {
        timeout: 5000,
        label: 'independent title HTTP request to finish after release',
      });
      assert.equal(titleHTTP.failed.length, 0, 'independent title HTTP request must not fail after release');
      assert.equal(await page.locator('#history-page').isVisible(), true, 'History must remain open for the independent title update');
      assert.ok(titleUpdate.elapsed <= 5000, `generated title reached open History after ${titleUpdate.elapsed}ms`);
      assert.equal(provider.calls.length, 2, `the independent title refresh must not start another provider call, saw ${provider.calls.length}`);
      const titleSummary = await chatSummaryHTML(env, chatID, 'title after main completion');
      assert.ok(titleSummary.includes(generatedTitle), `independent title was not persisted after main completion: ${titleSummary}`);

      const updatedScreenshot = await env.screenshot('chat-title-independent-completion-updated');
      t.diagnostic(
        `Title DOM updated ${titleUpdate.elapsed}ms after title release, after the main completion had already finished; ` +
        `title HTTP started ${concurrent.title.receivedAt - submittedAt}ms after submit.`,
      );
      t.diagnostic(`Updated independent-title screenshot: ${updatedScreenshot}`);
      console.log(
        `Title DOM updated ${titleUpdate.elapsed}ms after title release, after the main completion had already finished; ` +
        `title HTTP started ${concurrent.title.receivedAt - submittedAt}ms after submit.`,
      );
      console.log(`Updated independent-title screenshot: ${updatedScreenshot}`);

      await reloadChat(page, chatID);
      await waitForHistoryText(page, assistantReply);
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => {
        const text = (await page.locator('#history-entries .history-entry.current .history-title-link strong').first().innerText()).trim();
        return text === generatedTitle;
      }, {
        timeout: 10000,
        label: 'generated title to persist after reload following main completion',
      });
      assert.equal(provider.calls.length, 2, `reload after completion must not start another provider call, saw ${provider.calls.length}`);
      assert.equal(provider.calls.filter(isTitleGenerationCall).length, 1, 'reload must not issue another title request');

      await page.locator('#history-button').click();
      await waitForPanels(page, null);
      await submitPrompt(page, followupPrompt);
      await waitCompletionActive(page);
      const followup = await waitUntil(() => {
        return provider.calls.find((call) => !isTitleGenerationCall(call) && lastUser(call) === followupPrompt) ?? false;
      }, {
        timeout: 15000,
        label: 'follow-up completion provider request',
      });
      await provider.release(followup.index, finalResponse(followupReply));
      await waitCompletionDone(page);
      await waitForHistoryText(page, followupReply);
      assert.equal(provider.calls.length, 3, `follow-up completion should make the third provider call, saw ${provider.calls.length}`);
      assert.equal(
        provider.calls.filter(isTitleGenerationCall).length,
        1,
        'the follow-up message must not issue another title provider request',
      );
      assert.equal(titleHTTP.started.length, 1, 'the follow-up message must not issue another title HTTP request');
      assert.equal(titleHTTP.finished.length, 1, 'the initial independent title HTTP request must be the only one');

      const totalElapsed = Date.now() - submittedAt;
      t.diagnostic(
        `Independent title: primary completion settled ${completionElapsed}ms after release with title pending; ` +
        `title DOM updated ${titleUpdate.elapsed}ms after later title release; follow-up completion added only ` +
        `a primary provider call; total ${totalElapsed}ms.`,
      );
      console.log(
        `Independent title: primary completion settled ${completionElapsed}ms after release with title pending; ` +
        `title DOM updated ${titleUpdate.elapsed}ms after later title release; follow-up completion added only ` +
        `a primary provider call; total ${totalElapsed}ms.`,
      );
    },
  });
});

before(async () => {
  build = await buildApp();
  browser = await chromium.launch({
    executablePath,
    headless: true,
    args: ['--no-sandbox'],
  });
});

after(async () => {
  if (browser) {
    await browser.close();
  }
  if (build) {
    await cleanupBuild(build);
  }
});

test('final-response steering continues in a second provider call and persists', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'final-response steering',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('steer'), password);
      const { chatID } = await openChat(page);

      await submitPrompt(page, 'primary final turn');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);

      await waitUntil(async () => (await page.locator('#send-button').getAttribute('aria-label')) === 'Steer', {
        timeout: 10000,
        label: 'Send button to become Steer while the primary completion runs',
      });
      await page.locator('#queue-button').waitFor({ state: 'visible', timeout: 10000 });

      await submitPrompt(page, 'steer during final', { delivery: 'steer', expectInbox: true });
      assert.equal(provider.calls.length, 1, 'steering admission must not call the provider by itself');

      await provider.release(0, finalResponse('first final answer'));
      const continuation = await provider.waitForCallCount(2);
      assert.equal(lastUser(continuation), 'steer during final');
      assert.ok(
        continuation.messages.some((message) => message.role === 'assistant' && message.content === 'first final answer'),
        'second provider call must include the persisted first assistant response',
      );

      await provider.release(1, finalResponse('steered answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'primary final turn');
      await waitForHistoryText(page, 'first final answer');
      await waitForHistoryText(page, 'steer during final');
      await waitForHistoryText(page, 'steered answer');
      await waitForInboxCount(page, 0);
      await waitForNoCompletionControl(page);
      assertNoCallOverride(provider.calls, 2);

      await reloadChat(page, chatID);
      await waitForHistoryText(page, 'steered answer');
      await waitForHistoryText(page, 'steer during final');
      assertNoCallOverride(provider.calls, 2);
    },
  });
});

test('queued prompts run FIFO automatically without browser completion clicks', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'queued prompts FIFO',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('fifo'), password);
      await openChat(page);

      await submitPrompt(page, 'queue primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);

      await submitPrompt(page, 'queued B', { delivery: 'queue', expectInbox: true });
      await submitPrompt(page, 'queued C', { delivery: 'queue', expectInbox: true });
      await waitForInboxCount(page, 2);
      assert.deepEqual(await inboxTexts(page), ['queued B', 'queued C']);
      const screenshot = await env.screenshot('active-queue-fifo');
      t.diagnostic(`Representative active queue screenshot: ${screenshot}`);
      console.log(`Representative active queue screenshot: ${screenshot}`);
      assert.equal(provider.calls.length, 1);

      await provider.release(0, finalResponse('reply to primary'));
      const second = await provider.waitForCallCount(2);
      assert.equal(lastUser(second), 'queued B');
      assert.ok(!userContents(second).includes('queued C'), 'queued C must wait for queued B');

      await provider.release(1, finalResponse('reply to B'));
      const third = await provider.waitForCallCount(3);
      assert.equal(lastUser(third), 'queued C');

      await provider.release(2, finalResponse('reply to C'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'reply to primary');
      await waitForHistoryText(page, 'reply to B');
      await waitForHistoryText(page, 'reply to C');
      await waitForHistoryText(page, 'queued C');
      await waitForInboxCount(page, 0);
      await waitForNoCompletionControl(page);
      assertNoCallOverride(provider.calls, 3);
      assert.equal(await page.locator('.completion-retry-button').count(), 0);
    },
  });
});

test('tool round proves queued input waits while steering input reaches the next continuation', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'tool-round steering versus queue',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('tool'), password);
      await openChat(page);
      await selectWebfetch(page);

      await submitPrompt(page, 'tool primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);

      await submitPrompt(page, 'tool queued', { delivery: 'queue', expectInbox: true });
      await submitPrompt(page, 'tool steer', { delivery: 'steer', expectInbox: true });
      await waitForInboxCount(page, 2);

      await provider.release(0, toolCallResponse({
        id: 'call_fixture_1',
        name: 'webfetch',
        argumentsValue: { url: provider.fixtureURL, limit: 800 },
      }));
      const continuation = await provider.waitForCallCount(2);
      assert.ok(
        continuation.messages.some((message) => message.role === 'tool' && message.tool_call_id === 'call_fixture_1'),
        'tool continuation must include the matching tool result',
      );
      assert.equal(lastUser(continuation), 'tool steer');
      assert.ok(!userContents(continuation).includes('tool queued'), 'queued input must not enter the tool continuation');

      await provider.release(1, finalResponse('answer after tool and steer'));
      const afterFinal = await provider.waitForCallCount(3);
      assert.equal(lastUser(afterFinal), 'tool queued');

      await provider.release(2, finalResponse('answer after queued tool turn'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'tool primary');
      await waitForHistoryText(page, 'tool steer');
      await waitForHistoryText(page, 'tool queued');
      await waitForHistoryText(page, 'answer after tool and steer');
      await waitForHistoryText(page, 'answer after queued tool turn');
      await waitForInboxCount(page, 0);
      assertNoCallOverride(provider.calls, 3);
    },
  });
});

test('pending inputs survive reload and support mode changes and removal', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'pending reload mutation',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('reload'), password);
      const { chatID } = await openChat(page);

      await submitPrompt(page, 'reload primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);

      await submitPrompt(page, 'reload keep', { delivery: 'queue', expectInbox: true });
      await submitPrompt(page, 'reload remove', { delivery: 'queue', expectInbox: true });
      await waitForInboxCount(page, 2);

      await reloadChat(page, chatID);
      await waitCompletionActive(page);
      await waitForInboxText(page, 'reload keep');
      await waitForInboxText(page, 'reload remove');
      assert.equal(provider.calls.length, 1, 'reload must not start an extra provider call');

      await changeInboxDelivery(page, 'reload keep', 'steer');
      await removeInboxItem(page, 'reload remove');
      await waitForInboxCount(page, 1);

      await provider.release(0, finalResponse('reload final answer'));
      const continuation = await provider.waitForCallCount(2);
      assert.equal(lastUser(continuation), 'reload keep');
      assert.ok(!userContents(continuation).includes('reload remove'));

      await provider.release(1, finalResponse('reload kept answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'reload final answer');
      await waitForHistoryText(page, 'reload keep');
      await waitForHistoryText(page, 'reload kept answer');
      const history = await page.locator('#message-history').innerText();
      assert.ok(!history.includes('reload remove'), 'removed pending input must not reach the transcript');
      await waitForInboxCount(page, 0);
      assertNoCallOverride(provider.calls, 2);
    },
  });
});

test('failed provider parks the queue and retry drains it automatically', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'failed provider retry',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('retry'), password);
      await openChat(page);

      await submitPrompt(page, 'failure primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await submitPrompt(page, 'retry queued', { delivery: 'queue', expectInbox: true });

      await provider.release(0, errorResponse(503, 'fixture provider failure'));
      await waitCompletionError(page);
      await waitForInboxText(page, 'retry queued');
      assertNoCallOverride(provider.calls, 1);

      await clickRetry(page);
      const retryCall = await provider.waitForCallCount(2);
      assert.equal(lastUser(retryCall), 'failure primary');
      assert.ok(!userContents(retryCall).includes('retry queued'), 'queued input must still wait for a final response');
      await provider.release(1, finalResponse('retry recovered answer'));

      const queuedCall = await provider.waitForCallCount(3);
      assert.equal(lastUser(queuedCall), 'retry queued');
      await provider.release(2, finalResponse('retry queued answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'retry recovered answer');
      await waitForHistoryText(page, 'retry queued');
      await waitForHistoryText(page, 'retry queued answer');
      await waitForInboxCount(page, 0);
      assertNoCallOverride(provider.calls, 3);
    },
  });
});

test('idle Steer now after provider failure recovers, clears stale retry UI, and preserves the draft', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'idle steer now after failure',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('idle'), password);
      await openChat(page);

      await submitPrompt(page, 'idle failure primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await submitPrompt(page, 'idle steer now', { delivery: 'queue', expectInbox: true });

      await provider.release(0, errorResponse(503, 'fixture idle failure'));
      await waitCompletionError(page);
      await waitForInboxText(page, 'idle steer now');
      assert.equal(await page.locator('.completion-error').count(), 1);
      assert.equal(await page.locator('.completion-retry-button').count(), 1);

      await clickInboxSteerNow(page, 'idle steer now');
      await waitCompletionActive(page);
      await assertNoStaleCompletionFailure(page, { includeRetry: false });
      const draft = 'draft survives idle steer now recovery';
      await page.locator('#message').fill(draft);

      const steered = await provider.waitForCallCount(2);
      assert.equal(lastUser(steered), 'idle steer now');
      await provider.release(1, finalResponse('recovery answer after idle wake'));

      await waitCompletionDone(page);
      await waitForHistoryText(page, 'recovery answer after idle wake');
      const transcript = await page.locator('#message-history').innerText();
      const occurrences = transcript.split('idle steer now').length - 1;
      assert.equal(occurrences, 1, 'idle Steer now input must be persisted exactly once');
      await waitForInboxCount(page, 0);
      await waitForNoCompletionControl(page);
      await assertNoStaleCompletionFailure(page);
      assert.equal(await page.locator('#message').inputValue(), draft);
      assertNoCallOverride(provider.calls, 2);
    },
  });
});

test('app restart retains the inbox until explicit Retry', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'app restart retry',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('restart'), password);
      const { chatID } = await openChat(page);

      await submitPrompt(page, 'restart primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await submitPrompt(page, 'restart queued', { delivery: 'queue', expectInbox: true });
      await waitForInboxText(page, 'restart queued');

      await env.restartApp({ signal: 'SIGKILL' });
      await waitUntil(async () => provider.calls[0]?.aborted === true, {
        timeout: 10000,
        label: 'the killed app provider request to abort',
      });

      await reloadChat(page, chatID);
      await waitCompletionError(page);
      await waitForInboxText(page, 'restart queued');
      await assertNoResumeControl(page);
      assert.equal(provider.calls.length, 1, 'restart must not automatically resume work');

      await clickRetry(page);
      const resumed = await provider.waitForCallCount(2);
      assert.equal(lastUser(resumed), 'restart primary');
      assert.ok(!userContents(resumed).includes('restart queued'));
      await provider.release(1, finalResponse('restart recovered answer'));

      const queued = await provider.waitForCallCount(3);
      assert.equal(lastUser(queued), 'restart queued');
      await provider.release(2, finalResponse('restart queued answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'restart recovered answer');
      await waitForHistoryText(page, 'restart queued');
      await waitForHistoryText(page, 'restart queued answer');
      await waitForInboxCount(page, 0);
      await assertNoStaleCompletionFailure(page);
      assertNoCallOverride(provider.calls, 3);
    },
  });
});

test('composer draft typed after admission survives the completion SSE swap', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'draft survives completion',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('draft'), password);
      await openChat(page);

      await submitPrompt(page, 'draft primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);

      const draft = 'unsent draft survives terminal SSE';
      await page.locator('#message').fill(draft);
      assert.equal(await page.locator('#message').inputValue(), draft);

      await provider.release(0, finalResponse('draft completion answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'draft completion answer');
      await waitForNoCompletionControl(page);
      assert.equal(await page.locator('#message').inputValue(), draft);
      assertNoCallOverride(provider.calls, 1);
    },
  });
});

test('tools generated after steering stay live until the final response and survive reload', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'post-steer live tool chain',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('livetools'), password);
      const { chatID } = await openChat(page);
      await selectWebfetch(page);

      await submitPrompt(page, 'live tool primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await provider.release(0, toolCallResponse({
        id: 'call_live_old',
        name: 'webfetch',
        argumentsValue: { url: provider.fixtureURL, limit: 800 },
      }));

      const continuation = await provider.waitForCallCount(2);
      assert.ok(
        continuation.messages.some((message) => message.role === 'tool' && message.tool_call_id === 'call_live_old'),
        'the first continuation must include the matching pre-steer tool result',
      );

      await submitPrompt(page, 'live tool steer', { delivery: 'steer', expectInbox: true });
      assert.equal(provider.calls.length, 2, 'steering must not call the provider while the continuation is gated');

      await provider.release(1, finalResponse('live tool pre-steer answer'));
      const steered = await provider.waitForCallCount(3);
      assert.equal(lastUser(steered), 'live tool steer');
      assert.ok(
        steered.messages.some((message) => message.role === 'tool' && message.tool_call_id === 'call_live_old'),
        'the request after steering must include the older pre-steer tool result',
      );
      assert.ok(
        steered.messages.some((message) => message.role === 'assistant' && message.content === 'live tool pre-steer answer'),
        'the request after steering must include the pre-steer final answer',
      );
      await waitForInboxCount(page, 0);

      const liveTools = page.locator('.loading-message .tool-call-list [data-tool-call-id]');
      const completedDetails = page.locator('#message-history .completed-tool-calls');
      await waitForToolCallIDs(liveTools, [], 'steering to archive pre-steer tools from the live list');
      assert.equal(await completedDetails.count(), 1, 'pre-steer tool calls must render in one completed details block');
      assert.equal(await completedDetails.first().evaluate((element) => element.open), false,
        'pre-steer tool details must stay collapsed');
      await waitForToolCallIDs(completedDetails.locator('[data-tool-call-id]'), ['call_live_old'],
        'pre-steer completed tool block to contain only the older tool');

      await provider.release(2, toolCallResponse({
        id: 'call_live_after',
        name: 'webfetch',
        argumentsValue: { url: provider.fixtureURL, limit: 400 },
      }));
      const steeredTool = await provider.waitForCallCount(4);
      assert.equal(lastUser(steeredTool), 'live tool steer');
      assert.ok(
        steeredTool.messages.some((message) => message.role === 'tool' && message.tool_call_id === 'call_live_after'),
        'the continuation after the steered tool batch must include its matching tool result',
      );
      await waitForToolCallIDs(liveTools, ['call_live_after'], 'post-steer live tool list to contain the newer tool');
      assert.equal(await completedDetails.count(), 1, 'completed history must keep one pre-steer details block');
      assert.equal(await completedDetails.first().evaluate((element) => element.open), false,
        'completed history must remain collapsed while the current tool runs');
      await waitForToolCallIDs(completedDetails.locator('[data-tool-call-id]'), ['call_live_old'],
        'completed history to keep only pre-steer tools');
      assert.equal(
        await page.locator('.completed-tool-calls [data-tool-call-id="call_live_after"]').count(),
        0,
        'the post-steer tool must not be folded into completed history while completion is active',
      );

      await provider.release(3, toolCallResponse({
        id: 'call_live_round2',
        name: 'webfetch',
        argumentsValue: { url: provider.fixtureURL, limit: 200 },
      }));
      const nextContinuation = await provider.waitForCallCount(5);
      assert.equal(lastUser(nextContinuation), 'live tool steer');
      assert.ok(
        nextContinuation.messages.some((message) => message.role === 'tool' && message.tool_call_id === 'call_live_after'),
        'the next tool round must include the post-steer tool result',
      );
      await waitForToolCallIDs(
        liveTools,
        ['call_live_after', 'call_live_round2'],
        'another post-steer tool round to append to the existing live list',
      );
      assert.equal(await completedDetails.count(), 1, 'completed history must keep one pre-steer details block');
      assert.equal(await completedDetails.first().evaluate((element) => element.open), false,
        'completed history must remain collapsed while later tool rounds run');
      await waitForToolCallIDs(completedDetails.locator('[data-tool-call-id]'), ['call_live_old'],
        'completed history to keep only pre-steer tools');

      const screenshot = await env.screenshot('active-post-steer-tool-chain');
      t.diagnostic(`Representative active post-steer screenshot: ${screenshot}`);
      console.log(`Representative active post-steer screenshot: ${screenshot}`);

      await reloadChat(page, chatID);
      await waitCompletionActive(page);
      await waitForToolCallIDs(
        page.locator('.loading-message .tool-call-list [data-tool-call-id]'),
        ['call_live_after', 'call_live_round2'],
        'reloaded active completion to restore the current-turn live tools',
      );
      const reloadedDetails = page.locator('#message-history .completed-tool-calls');
      assert.equal(await reloadedDetails.count(), 1, 'reload must keep one collapsed pre-steer details block');
      assert.equal(await reloadedDetails.first().evaluate((element) => element.open), false,
        'reload must keep pre-steer tool details collapsed');
      await waitForToolCallIDs(reloadedDetails.locator('[data-tool-call-id]'), ['call_live_old'],
        'reload must not move current-turn tools into completed history');

      await provider.release(4, finalResponse('live tool final answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'live tool final answer');
      await waitForInboxCount(page, 0);
      assert.equal(await page.locator('.loading-message').count(), 0, 'active completion must disappear after the final answer');
      const finalDetails = page.locator('#message-history .completed-tool-calls');
      assert.equal(await finalDetails.count(), 2, 'pre-steer and post-steer tool rounds must collapse into history blocks');
      assert.deepEqual(await finalDetails.evaluateAll((elements) => elements.map((element) => element.open)), [false, false],
        'historical tool details must be collapsed after completion');
      const finalIDs = await toolCallIDs(page.locator('#message-history [data-tool-call-id]'));
      assert.deepEqual(finalIDs, ['call_live_old', 'call_live_after', 'call_live_round2'],
        'the transcript must contain each tool call exactly once and in order');
      assertNoCallOverride(provider.calls, 5);
    },
  });
});

test('tool-result budget elision mutes consumed live tool names before the next response and preserves flags through collapse and reload', { timeout: 240000 }, async (t) => {
  const search = new SearchFixture();
  await search.start();
  try {
    await runIsolatedCase(t, {
      name: 'tool result budget elision',
      browser,
      build,
      appEnv: { SEARXNG_URL: search.url },
      async run(env) {
        const { page, provider } = env;
        await signup(page, username('elidebudget'), password);
        const { chatID } = await openChat(page);
        await selectChatTool(page, 'websearch');
        await configureToolResultElision(page, { mode: 'tool_result_budget', tokenBudget: 1 });
        const colors = await expectedToolNameColors(page);

        const batchA = [
          {
            id: 'call_elide_a_ok',
            name: 'websearch',
            argumentsValue: { query: 'budget success long result', max_results: 2 },
          },
          {
            id: 'call_elide_a_err',
            name: 'websearch',
            argumentsValue: { query: 'empty budget failure', max_results: 2 },
          },
        ];
        const batchB = [
          {
            id: 'call_elide_b_ok',
            name: 'websearch',
            argumentsValue: { query: 'fresh success long result', max_results: 2 },
          },
          {
            id: 'call_elide_b_err',
            name: 'websearch',
            argumentsValue: { query: 'empty fresh failure', max_results: 2 },
          },
        ];
        const expectedLive = [
          { id: batchA[0].id, name: 'websearch', elided: true, errored: false },
          { id: batchA[1].id, name: 'websearch', elided: true, errored: true },
          { id: batchB[0].id, name: 'websearch', elided: false, errored: false },
          { id: batchB[1].id, name: 'websearch', elided: false, errored: true },
        ];

        await submitPrompt(page, 'budget elision primary');
        await waitCompletionActive(page);
        await provider.waitForCallCount(1);
        await provider.release(0, multiToolCallResponse(batchA));

        const afterFirstBatch = await provider.waitForCallCount(2);
        assert.ok(
          toolResultContent(afterFirstBatch, batchA[0].id).includes(elisionResultMarker),
          'the first continuation must receive the full fresh successful result',
        );
        assert.equal(
          toolResultContent(afterFirstBatch, batchA[1].id),
          `Tool error: ${elisionErrorText}`,
          'the first continuation must receive the original error result',
        );

        await provider.release(1, multiToolCallResponse(batchB));
        const gated = await provider.waitForCallCount(3);
        assert.equal(
          toolResultContent(gated, batchA[0].id),
          elisionMarker,
          'the consumed successful result must be elided before the gated provider request',
        );
        assert.equal(
          toolResultContent(gated, batchA[1].id),
          elisionMarker,
          'the consumed errored result must be elided before the gated provider request',
        );
        assert.ok(
          toolResultContent(gated, batchB[0].id).includes(elisionResultMarker),
          'the fresh successful result must remain intact in the gated provider request',
        );
        assert.equal(
          toolResultContent(gated, batchB[1].id),
          `Tool error: ${elisionErrorText}`,
          'the fresh errored result must remain intact in the gated provider request',
        );

        await waitCompletionActive(page);
        const liveTools = page.locator('.loading-message .tool-call-list');
        await assertToolCallAppearance(page, liveTools, expectedLive, colors, 'gated live tool list');
        assert.equal(
          await page.locator('.loading-message.completion-active').count(),
          1,
          'the completion must remain active while elided names are published',
        );
        await assertToolResultBodiesHidden(page, 'active budget completion');

        await provider.release(2, finalResponse('budget elision final answer'));
        await waitCompletionDone(page);
        await waitForHistoryText(page, 'budget elision final answer');

        const completed = await completedToolDetails(page, expectedLive, colors, 'completed budget turn');
        await assertVisibleToolError(completed, batchA[1].id, 'expanded budget turn', colors);
        await assertToolResultBodiesHidden(page, 'completed budget turn');

        await reloadChat(page, chatID);
        await waitForHistoryText(page, 'budget elision final answer');
        const reloadedColors = await expectedToolNameColors(page);
        const reloaded = await completedToolDetails(page, expectedLive, reloadedColors, 'reloaded budget turn');
        await assertVisibleToolError(reloaded, batchA[1].id, 'reloaded budget turn', reloadedColors);
        await assertToolResultBodiesHidden(page, 'reloaded budget turn');
        assertNoCallOverride(provider.calls, 3);
      },
    });
  } finally {
    await search.close();
  }
});

test('current-turn elision updates archived tool names before the next response and none mode clears them when full results return', { timeout: 240000 }, async (t) => {
  const search = new SearchFixture();
  await search.start();
  try {
    await runIsolatedCase(t, {
      name: 'archived tool result elision reset',
      browser,
      build,
      appEnv: { SEARXNG_URL: search.url },
      async run(env) {
        const { page, provider } = env;
        await signup(page, username('elidearchive'), password);
        const { chatID } = await openChat(page);
        await selectChatTool(page, 'websearch');
        await configureToolResultElision(page, { mode: 'current_turn' });
        const colors = await expectedToolNameColors(page);

        const seedCalls = [
          {
            id: 'call_elide_seed_ok',
            name: 'websearch',
            argumentsValue: { query: 'current turn success long result', max_results: 2 },
          },
          {
            id: 'call_elide_seed_err',
            name: 'websearch',
            argumentsValue: { query: 'empty current turn failure', max_results: 2 },
          },
        ];
        const seedStates = [
          { id: seedCalls[0].id, name: 'websearch', elided: false, errored: false },
          { id: seedCalls[1].id, name: 'websearch', elided: false, errored: true },
        ];
        const elidedSeedStates = seedStates.map((state) => ({ ...state, elided: true }));

        await submitPrompt(page, 'current turn seed');
        await waitCompletionActive(page);
        await provider.waitForCallCount(1);
        await provider.release(0, multiToolCallResponse(seedCalls));
        await provider.waitForCallCount(2);
        await provider.release(1, finalResponse('current turn seed answer'));
        await waitCompletionDone(page);
        await waitForHistoryText(page, 'current turn seed answer');
        const seedBlock = await completedToolDetails(page, seedStates, colors, 'seed turn');
        await assertVisibleToolError(seedBlock, seedCalls[1].id, 'seed turn', colors);
        await assertToolResultBodiesHidden(page, 'seed turn');

        await submitPrompt(page, 'current turn follow-up');
        await waitCompletionActive(page);
        const followUp = await provider.waitForCallCount(3);
        assert.equal(
          toolResultContent(followUp, seedCalls[0].id),
          elisionMarker,
          'current-turn mode must elide the prior successful result in the follow-up request',
        );
        assert.equal(
          toolResultContent(followUp, seedCalls[1].id),
          elisionMarker,
          'current-turn mode must elide the prior errored result in the follow-up request',
        );

        const history = page.locator('#message-history');
        await assertToolCallAppearance(page, history, elidedSeedStates, colors, 'archived tools while follow-up is gated');
        assert.equal(
          await page.locator('.loading-message.completion-active').count(),
          1,
          'the follow-up completion must remain active while archived flags update',
        );
        await assertToolResultBodiesHidden(page, 'gated current-turn follow-up');

        await provider.release(2, finalResponse('current turn follow-up answer'));
        await waitCompletionDone(page);
        await waitForHistoryText(page, 'current turn follow-up answer');

        await configureToolResultElision(page, { mode: 'none' });
        await submitPrompt(page, 'none reset follow-up');
        await waitCompletionActive(page);
        const reset = await provider.waitForCallCount(4);
        assert.ok(
          toolResultContent(reset, seedCalls[0].id).includes(elisionResultMarker),
          'none mode must resend the full prior successful result',
        );
        assert.equal(
          toolResultContent(reset, seedCalls[1].id),
          `Tool error: ${elisionErrorText}`,
          'none mode must resend the full prior error result',
        );
        assert.equal(
          toolResultContent(reset, seedCalls[0].id).includes(elisionMarker),
          false,
          'none mode must not send the internal elision marker',
        );

        await assertToolCallAppearance(page, page.locator('#message-history'), seedStates, colors, 'archived tools after none reset while gated');
        assert.equal(
          await page.locator('.loading-message.completion-active').count(),
          1,
          'the reset completion must remain active while archived flags clear',
        );

        await provider.release(3, finalResponse('none reset answer'));
        await waitCompletionDone(page);
        await waitForHistoryText(page, 'none reset answer');
        await reloadChat(page, chatID);
        await waitForHistoryText(page, 'none reset answer');
        const reloadedColors = await expectedToolNameColors(page);
        const reloaded = await completedToolDetails(page, seedStates, reloadedColors, 'reloaded none reset turn');
        await assertVisibleToolError(reloaded, seedCalls[1].id, 'reloaded none reset turn', reloadedColors);
        await assertToolResultBodiesHidden(page, 'reloaded none reset turn');
        assertNoCallOverride(provider.calls, 4);
      },
    });
  } finally {
    await search.close();
  }
});

test('pending inbox sits above the toolbar without visible heading, counter, or Resume', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'pending inbox layout chrome',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('layout'), password);
      await openChat(page);

      await submitPrompt(page, 'layout primary');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await submitPrompt(page, 'layout queued item', { delivery: 'queue', expectInbox: true });
      await submitPrompt(page, 'layout steer item', { delivery: 'steer', expectInbox: true });
      await waitForInboxCount(page, 2);

      await assertNoResumeControl(page);
      await assertInboxChromeRemoved(page);
      const badges = page.locator('#prompt-inbox .prompt-inbox-delivery');
      await waitUntil(async () => {
        if ((await badges.count()) !== 2) {
          return false;
        }
        return (await badges.nth(0).isVisible()) && (await badges.nth(1).isVisible());
      }, {
        timeout: 10000,
        label: 'both pending inbox delivery badges to be visible',
      });
      const badgeLabels = (await badges.allTextContents()).map((label) => label.trim()).sort();
      assert.deepEqual(badgeLabels, ['Queued', 'Steer'], 'pending inbox must retain its delivery badges');
      const models = page.locator('#prompt-inbox .prompt-inbox-model');
      await waitUntil(async () => (await models.count()) >= 1 && (await models.first().isVisible()), {
        timeout: 10000,
        label: 'queued pending input model badge',
      });
      assert.equal((await models.first().innerText()).trim(), 'fake-model');

      await assertInboxAboveToolbar(page);

      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForFunction(() => window.innerWidth === 390);
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const screenshot = await env.screenshot('pending-inbox-mobile-layout');
      t.diagnostic(`Representative mobile pending inbox layout screenshot: ${screenshot}`);
      console.log(`Representative mobile pending inbox layout screenshot: ${screenshot}`);

      await assertNoResumeControl(page);
      await assertInboxChromeRemoved(page);
      await assertInboxAboveToolbar(page);
    },
  });
});

test('settings Save all and Log out actions share geometry with distinct success and danger colors', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'settings action consistency',
    browser,
    build,
    async run(env) {
      const { page } = env;
      await signup(page, username('actions'), password);
      await openChat(page);

      await page.locator('#settings-button').click();
      await page.locator('#settings-page').waitFor({ state: 'visible' });

      const save = page.locator('#settings-page .settings-save');
      const logout = page.locator('#settings-page .settings-logout-button');
      assert.equal((await save.innerText()).trim(), 'Save all');
      assert.equal((await logout.innerText()).trim(), 'Log out');

      for (const viewport of [
        { label: 'desktop', width: 1280, height: 900 },
        { label: 'mobile', width: 390, height: 844 },
      ]) {
        await useViewport(page, viewport.width, viewport.height);
        await save.waitFor({ state: 'visible' });
        await logout.waitFor({ state: 'visible' });
        await assertEqualActionChrome(save, logout, viewport.label);

        await page.locator('#settings-page').evaluate((panel) => {
          panel.scrollTop = panel.scrollHeight;
        });
        await waitForStableLayout(page);
        const screenshot = await env.screenshot(`settings-actions-${viewport.label}`);
        t.diagnostic(`Settings action ${viewport.label} screenshot: ${screenshot}`);
        console.log(`Settings action ${viewport.label} screenshot: ${screenshot}`);
      }
    },
  });
});

test('bottom navigation and model options share borders while panels switch without leaving the chat', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'bottom navigation boundaries',
    browser,
    build,
    async run(env) {
      const { page } = env;
      await signup(page, username('navbounds'), password);
      await openChat(page);
      const formAction = await page.locator('#message-form').getAttribute('hx-post');

      await assertBottomNavigationGeometry(page, 'desktop closed');
      await exercisePanelAndModelNavigation(page, formAction, 'desktop');
      const desktopScreenshot = await env.screenshot('bottom-navigation-model-desktop');
      t.diagnostic(`Desktop bottom navigation/model screenshot: ${desktopScreenshot}`);
      console.log(`Desktop bottom navigation/model screenshot: ${desktopScreenshot}`);

      await useViewport(page, 390, 844);
      await assertBottomNavigationGeometry(page, 'mobile closed');
      await exercisePanelAndModelNavigation(page, formAction, 'mobile');
      const mobileScreenshot = await env.screenshot('bottom-navigation-model-mobile');
      t.diagnostic(`Mobile bottom navigation/model screenshot: ${mobileScreenshot}`);
      console.log(`Mobile bottom navigation/model screenshot: ${mobileScreenshot}`);
    },
  });
});

test('history rename checkmark and X align and submit by click and Enter at desktop and mobile widths', { timeout: 180000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'history rename actions',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('historyact'), password);
      await openChat(page);

      await submitPrompt(page, 'history seed message');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await provider.release(0, finalResponse('history seed answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'history seed answer');

      await page.locator('#history-button').click();
      await page.locator('#history-page').waitFor({ state: 'visible' });
      await page.locator('#history-entries .history-entry.current').first().waitFor({ state: 'visible', timeout: 10000 });

      for (const viewport of [
        { label: 'desktop', width: 1280, height: 900 },
        { label: 'mobile', width: 390, height: 844 },
      ]) {
        await useViewport(page, viewport.width, viewport.height);
        const clickTitle = `History click ${viewport.label}`;
        const enterTitle = `History enter ${viewport.label}`;

        let entry = await openHistoryEditor(page);
        await assertHistoryRenameActions(entry, viewport.label);
        const screenshot = await env.screenshot(`history-rename-actions-${viewport.label}`);
        t.diagnostic(`History rename ${viewport.label} screenshot: ${screenshot}`);
        console.log(`History rename ${viewport.label} screenshot: ${screenshot}`);
        await renameHistoryWith(page, entry, clickTitle, 'click', `${viewport.label} click`);

        entry = await openHistoryEditor(page);
        await renameHistoryWith(page, entry, enterTitle, 'enter', `${viewport.label} Enter`);
      }
    },
  });
});

test('history delete keeps its own pending spinner active without the global overlay until the gated DELETE completes', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'history delete pending indicator',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('historydel'), password);
      const target = await createPersistedChat(page, provider, 'Delete this history chat', 'Deleted chat answer');
      const survivor = await createPersistedChat(page, provider, 'Keep this history chat', 'Surviving chat answer');

      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      const targetRow = historyRowForChat(page, target.chatID);
      const survivorRow = historyRowForChat(page, survivor.chatID);
      await targetRow.waitFor({ state: 'visible', timeout: 10000 });
      await survivorRow.waitFor({ state: 'visible', timeout: 10000 });
      await waitUntil(async () => {
        return (await page.locator('#history-entries .history-entry').count()) === 2;
      }, {
        timeout: 10000,
        label: 'two persisted chats in History',
      });

      const deleteButton = targetRow.locator('.history-delete');
      assert.equal(await deleteButton.getAttribute('hx-indicator'), 'this', 'delete must use its own pending indicator');
      const idleState = await historyDeleteState(targetRow);
      assert.equal(idleState.pending, false, 'idle delete button must not carry htmx-request');
      assert.equal(idleState.svgCount, 1, 'idle delete button must render one trash icon');
      assert.equal(idleState.svgVisible, true, 'idle delete trash icon must be visible');
      assert.equal(idleState.spinnerCount, 1, 'delete button must render one pending spinner');
      assert.equal(idleState.spinnerAriaHidden, 'true', 'delete spinner must stay hidden from assistive technology');
      assert.equal(idleState.spinnerVisible, false, 'idle delete pending spinner must stay hidden');
      const idleMetrics = await elementMetrics(deleteButton);

      const deleteRequests = [];
      const trackRequest = (request) => {
        const url = new URL(request.url());
        if (request.method() === 'DELETE' && /^\/chats\/\d+$/.test(url.pathname)) {
          deleteRequests.push(url.href);
        }
      };
      page.on('request', trackRequest);

      let cancelledMessage = '';
      const cancelledDialog = new Promise((resolve, reject) => {
        page.once('dialog', async (dialog) => {
          try {
            cancelledMessage = dialog.message();
            await dialog.dismiss();
            resolve();
          } catch (error) {
            reject(error);
          }
        });
      });
      await deleteButton.click();
      await cancelledDialog;
      assert.match(cancelledMessage, /^Permanently delete .+\?$/, 'delete confirmation message changed');
      await page.waitForTimeout(100);
      assert.equal(deleteRequests.length, 0, 'cancelled confirmation must not issue DELETE');
      const cancelledState = await historyDeleteState(targetRow);
      assert.equal(cancelledState.pending, false, 'cancelled delete must not keep htmx-request');
      assert.equal(cancelledState.svgVisible, true, 'cancelled delete must restore the trash icon');
      assert.equal(cancelledState.spinnerVisible, false, 'cancelled delete must keep the pending spinner hidden');
      await assertGlobalRequestOverlayInactive(page, 'cancelled history delete');

      const deleteGate = deferredGate();
      let routedDelete = false;
      const deleteRoute = (url) => /^\/chats\/\d+$/.test(url.pathname);
      const deleteRouteHandler = async (route) => {
        if (route.request().method() !== 'DELETE') {
          await route.continue();
          return;
        }
        routedDelete = true;
        await deleteGate.promise;
        await route.continue();
      };
      await page.route(deleteRoute, deleteRouteHandler);
      try {
        let acceptedMessage = '';
        const acceptedDialog = new Promise((resolve, reject) => {
          page.once('dialog', async (dialog) => {
            try {
              acceptedMessage = dialog.message();
              await dialog.accept();
              resolve();
            } catch (error) {
              reject(error);
            }
          });
        });
        await deleteButton.click();
        await acceptedDialog;
        assert.match(acceptedMessage, /^Permanently delete .+\?$/, 'accepted delete confirmation message changed');
        await waitUntil(() => routedDelete, {
          timeout: 10000,
          label: 'gated history DELETE request to start',
        });

        const pendingState = await waitUntil(async () => {
          const state = await historyDeleteState(targetRow);
          return state.pending && state.spinnerVisible && state.spinnerAnimation === 'braille-spinner' &&
            !state.svgVisible ? state : false;
        }, {
          timeout: 10000,
          label: 'clicked history delete button to show its braille spinner',
        });
        assert.equal(pendingState.indicator, 'this', 'pending delete must retain its own indicator');
        assert.equal(pendingState.spinnerCount, 1, 'pending delete must keep one spinner element');
        assert.equal(pendingState.spinnerAriaHidden, 'true', 'pending delete spinner must stay aria-hidden');
        assert.equal(pendingState.svgDisplay, 'none', 'pending delete must hide the trash SVG');
        assertClose(pendingState.width, idleMetrics.width, 0.5, 'pending history delete width');
        assertClose(pendingState.height, idleMetrics.height, 0.5, 'pending history delete height');

        const survivorState = await historyDeleteState(survivorRow);
        assert.equal(survivorState.pending, false, 'unclicked delete button must stay idle');
        assert.equal(survivorState.svgVisible, true, 'unclicked delete button must keep its trash icon');
        assert.equal(survivorState.spinnerVisible, false, 'unclicked delete button must keep its spinner hidden');
        await assertGlobalRequestOverlayInactive(page, 'pending history delete');

        const screenshot = await env.screenshot('history-delete-pending');
        t.diagnostic(`History delete pending screenshot: ${screenshot}`);
        console.log(`History delete pending screenshot: ${screenshot}`);

        const responsePromise = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return response.request().method() === 'DELETE' && /^\/chats\/\d+$/.test(url.pathname);
        }, { timeout: 20000 });
        deleteGate.release();
        const response = await responsePromise;
        const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
        assert.ok(
          response.status() >= 200 && response.status() < 300,
          `history DELETE returned ${response.status()}${body ? `: ${body}` : ''}`,
        );

        await waitUntil(async () => {
          return (await historyRowForChat(page, target.chatID).count()) === 0 &&
            (await historyRowForChat(page, survivor.chatID).count()) === 1;
        }, {
          timeout: 10000,
          label: 'deleted history row to disappear while the survivor remains',
        });
        assert.equal(await page.locator('#history-page').isVisible(), true, 'History must remain open after delete');
        await assertGlobalRequestOverlayInactive(page, 'completed history delete');
      } finally {
        deleteGate.release();
        await page.unroute(deleteRoute, deleteRouteHandler).catch(() => {});
        page.off('request', trackRequest);
      }
    },
  });
});

test('history pagination shows its cursor loader spinner without the global overlay while the gated request is pending', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'history pagination pending indicator',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('historypage'), password);
      const chats = [
        await createPersistedChat(page, provider, 'Oldest pagination history chat', 'Oldest pagination answer'),
        await createPersistedChat(page, provider, 'Middle pagination history chat', 'Middle pagination answer'),
        await createPersistedChat(page, provider, 'Newest pagination history chat', 'Newest pagination answer'),
      ];

      const cursorGate = deferredGate();
      let firstHistoryRewritten = false;
      let cursorURL = '';
      const historyRoute = (url) => url.pathname === '/history';
      const historyRouteHandler = async (route) => {
        if (route.request().method() !== 'GET') {
          await route.continue();
          return;
        }
        const url = new URL(route.request().url());
        if (!url.searchParams.has('before') && !firstHistoryRewritten) {
          firstHistoryRewritten = true;
          url.searchParams.set('limit', '1');
          await route.continue({ url: url.href });
          return;
        }
        if (url.searchParams.has('before')) {
          cursorURL = url.href;
          await cursorGate.promise;
        }
        await route.continue();
      };
      await page.route(historyRoute, historyRouteHandler);
      try {
        await page.locator('#history-button').click();
        await waitForPanels(page, 'history-page');
        await waitUntil(() => firstHistoryRewritten, {
          timeout: 10000,
          label: 'initial History request to be limited to one row',
        });
        const entries = page.locator('#history-entries .history-entry');
        await waitUntil(async () => (await entries.count()) === 1, {
          timeout: 10000,
          label: 'first History page to render one chat with a cursor loader',
        });
        const loader = page.locator('#history-entries .history-loader');
        await loader.waitFor({ state: 'visible', timeout: 10000 });
        assert.equal(await loader.getAttribute('hx-indicator'), 'this', 'history loader must use its own pending indicator');

        await page.locator('#history-page').evaluate((element) => {
          element.scrollTop = element.scrollHeight;
        });
        await loader.scrollIntoViewIfNeeded().catch(() => {});

        const pendingState = await waitUntil(async () => {
          const state = await historyLoaderState(loader);
          return state.pending && state.spinnerVisible && state.spinnerAnimation === 'braille-spinner' ? state : false;
        }, {
          timeout: 15000,
          label: 'cursor history loader to show its pending braille spinner',
        });
        assert.ok(cursorURL, 'gated History request must include a before cursor');
        const cursor = new URL(cursorURL);
        assert.equal(cursor.pathname, '/history', 'cursor request must use the History endpoint');
        assert.ok(cursor.searchParams.get('before'), 'cursor request must include the before timestamp');
        assert.ok(cursor.searchParams.get('before_id'), 'cursor request must include the before id');
        assert.equal(cursor.searchParams.get('before_pinned'), '0', 'cursor request must include the pinned cursor');
        assert.equal(pendingState.indicator, 'this', 'pending loader must retain its own indicator');
        assert.equal(pendingState.spinnerCount, 1, 'pending loader must keep one spinner element');
        assertClose(
          pendingState.spinnerFontSize,
          pendingState.loaderFontSize * 3,
          0.5,
          'pending history loader spinner font size',
        );
        await assertGlobalRequestOverlayInactive(page, 'pending history pagination');

        const screenshot = await env.screenshot('history-pagination-pending');
        t.diagnostic(`History pagination pending screenshot: ${screenshot}`);
        console.log(`History pagination pending screenshot: ${screenshot}`);

        const responsePromise = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return response.request().method() === 'GET' && url.pathname === '/history' && url.searchParams.has('before');
        }, { timeout: 20000 });
        cursorGate.release();
        const response = await responsePromise;
        const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
        assert.ok(
          response.status() >= 200 && response.status() < 300,
          `cursor history request returned ${response.status()}${body ? `: ${body}` : ''}`,
        );

        await waitUntil(async () => (await entries.count()) === chats.length, {
          timeout: 10000,
          label: 'older History page rows to replace the loader',
        });
        const hrefs = await page.locator('#history-entries .history-entry .history-title-link').evaluateAll(
          (nodes) => nodes.map((node) => node.getAttribute('href')),
        );
        const expectedHrefs = chats.slice().reverse().map((chat) => `/?chat=${chat.chatID}`);
        assert.deepEqual(hrefs, expectedHrefs, 'History rows must insert once in newest-to-oldest order');
        assert.equal(new Set(hrefs).size, chats.length, 'History pagination must not duplicate rows');
        assert.equal(await page.locator('#history-entries .history-loader').count(), 0,
          'loader must disappear after the final cursor page');
        await assertGlobalRequestOverlayInactive(page, 'completed history pagination');
      } finally {
        cursorGate.release();
        await page.unroute(historyRoute, historyRouteHandler).catch(() => {});
      }
    },
  });
});

test('user message edit checkmark and X match history icons and submit by click and Enter while cancel stays local', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'user message edit actions',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      await signup(page, username('messageact'), password);
      await openChat(page);

      await submitPrompt(page, 'original user message');
      await waitCompletionActive(page);
      await provider.waitForCallCount(1);
      await provider.release(0, finalResponse('initial answer'));
      await waitCompletionDone(page);
      await waitForHistoryText(page, 'initial answer');

      const editRequests = [];
      page.on('request', (request) => {
        if (request.method() !== 'PUT') {
          return;
        }
        const path = new URL(request.url()).pathname;
        if (/^\/chats\/\d+\/messages\/\d+$/.test(path)) {
          editRequests.push(path);
        }
      });
      let currentText = 'original user message';

      for (const viewport of [
        { label: 'desktop', width: 1280, height: 900 },
        { label: 'mobile', width: 390, height: 844 },
      ]) {
        await useViewport(page, viewport.width, viewport.height);

        let message = await openMessageEditor(page);
        await assertMessageEditActions(message, viewport.label);
        const screenshot = await env.screenshot(`message-edit-actions-${viewport.label}`);
        t.diagnostic(`Message edit ${viewport.label} screenshot: ${screenshot}`);
        console.log(`Message edit ${viewport.label} screenshot: ${screenshot}`);

        const requestsBeforeCancel = editRequests.length;
        const callsBeforeCancel = provider.calls.length;
        await message.locator('.message-edit-input').fill(`cancelled edit ${viewport.label}`);
        await message.locator('.message-edit-cancel').click();
        await waitUntil(async () => (await page.locator('#message-history .message.user.editing').count()) === 0, {
          timeout: 5000,
          label: `${viewport.label} message editor to close after cancel`,
        });
        await page.waitForTimeout(300);
        assert.equal(editRequests.length, requestsBeforeCancel, `${viewport.label} cancel must not send an edit request`);
        assert.equal(provider.calls.length, callsBeforeCancel, `${viewport.label} cancel must not start a completion`);
        assert.equal(
          await page.locator('.loading-message.completion-active').count(),
          0,
          `${viewport.label} cancel must not render an active completion`,
        );
        assert.ok(
          (await message.locator('.message-content').innerText()).includes(currentText),
          `${viewport.label} cancel must preserve the stored message text`,
        );

        const clickText = `click edit ${viewport.label}`;
        await submitMessageEdit(page, provider, await openMessageEditor(page), clickText, 'click', `${viewport.label} click`);
        currentText = clickText;

        const enterText = `enter edit ${viewport.label}`;
        await submitMessageEdit(page, provider, await openMessageEditor(page), enterText, 'enter', `${viewport.label} Enter`);
        currentText = enterText;
      }
    },
  });
});

test('public response share copies a frozen standalone article and survives theme and source changes', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'public response share frozen article',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const promptMarker = 'PROMPT-SENTINEL-field-notes-7c31';
      const articleMarker = 'ARTICLE-SENTINEL-reading-surface-9d42';
      const prompt = [
        `${promptMarker} Explain these field notes clearly.`,
        '',
        'Keep **markdown** and <strong>HTML</strong> literal.',
        'Do not execute <script>window.__sharedPromptXss = true</script>.',
      ].join('\n');
      const chatTitle = `Shared field notes ${promptMarker}`;
      const articleReply = [
        '## Overview',
        '',
        `A public reading surface for one finished response. ${articleMarker}`,
        '',
        '| Item | Detail |',
        '| --- | --- |',
        '| Alpha | first value |',
        `| Wide | ${'column value '.repeat(20)} |`,
        '',
        '```text',
        `const frozen = "${'x'.repeat(160)}";`,
        '```',
        '',
        'Read the [reference](https://example.com/reference) for details.',
        '',
        'Attempted <script>window.__sharedXss = true</script> content.',
      ].join('\n');
      const plaintextMarkers = [
        promptMarker,
        articleMarker,
        chatTitle,
        'A public reading surface for one finished response.',
        '--color-background',
      ];
      const extraContexts = [];
      const sharePOSTs = [];
      const ownerRequests = [];
      page.on('request', (request) => {
        ownerRequests.push(request);
        const url = new URL(request.url());
        if (request.method() === 'POST' && /^\/chats\/\d+\/messages\/\d+\/share(?:\/publish)?$/.test(url.pathname)) {
          sharePOSTs.push(url.pathname);
        }
      });

      try {
        await signup(page, username('sharepage'), password);
        const { chatID } = await openChat(page);
        await selectTheme(page, '1975');
        await selectWebfetch(page);

        await submitPrompt(page, prompt);
        await waitCompletionActive(page);
        await assertNoShareActions(page.locator('.loading-message.completion-active'), 'active completion placeholder');
        await assertNoShareActions(page.locator('#message-history .message.user'), 'user message');
        await provider.waitForCallCount(1);
        await provider.release(0, toolCallResponse({
          id: 'call_share_fixture',
          name: 'webfetch',
          argumentsValue: { url: provider.fixtureURL, limit: 400 },
        }));
        await provider.waitForCallCount(2);
        await waitForToolCallIDs(
          page.locator('.loading-message .tool-call-list [data-tool-call-id]'),
          ['call_share_fixture'],
          'live share tool call to render',
        );
        await assertNoShareActions(page.locator('.loading-message .tool-call-list'), 'live tool-call list');
        await provider.release(1, finalResponse(articleReply));
        await waitCompletionDone(page);
        await waitForHistoryText(page, 'A public reading surface for one finished response.');

        await renameChat(page, chatID, chatTitle);
        await reloadChat(page, chatID);
        await waitForHistoryText(page, 'A public reading surface for one finished response.');

        const assistant = page.locator('#message-history article.message')
          .filter({ hasText: 'A public reading surface for one finished response.' })
          .first();
        await assistant.waitFor({ state: 'visible', timeout: 10000 });
        const userMessage = page.locator('#message-history .message.user').filter({ hasText: prompt }).first();
        await assertNoShareActions(userMessage, 'user message after completion');
        assert.equal(await page.locator('#message-history .completed-tool-calls').count(), 1,
          'the shared turn must keep its completed tool-call group');
        await assertNoShareActions(page.locator('#message-history .completed-tool-calls'), 'completed tool-call group');
        const shareActions = assistant.locator('.message-share-actions');
        assert.equal(await shareActions.count(), 1,
          'the final persisted assistant response must render share actions exactly once');
        assert.equal(await shareActions.locator('button').count(), 1,
          'the share actions must render exactly one icon button');
        assert.equal(await shareActions.locator('a').count(), 0,
          'the share actions must not render links before sharing');
        const shareButton = shareActions.getByRole('button', { name: 'Share response' });
        assert.equal(await shareButton.count(), 1, 'final assistant response must expose a Share response button');
        assert.equal(await shareButton.isVisible(), true, 'Share response control must be visible');
        assert.equal(await shareButton.getAttribute('aria-label'), 'Share response',
          'Share response control must expose its accessible label');
        assert.equal(await shareButton.getAttribute('type'), 'submit', 'Share response must submit the share form');
        assert.match(await shareButton.getAttribute('class') ?? '', /\bmessage-share-button\b/,
          'Share response must use the message share button class');
        await assertIconOnlyShareButton(shareButton, 'initial Share response button');
        const shareIcon = await iconPaths(shareButton);

        await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: env.baseURL });
        await page.bringToFront();
        const shareAction = await shareActions.getAttribute('action');
        assert.match(shareAction ?? '', /^\/chats\/\d+\/messages\/\d+\/share$/, 'share form action is missing');

        const creation = await performShare(page, shareButton, shareAction);
        assert.deepEqual(
          sharePOSTs,
          [shareAction, `${shareAction}/publish`],
          'creating one encrypted share must send exactly one prepare and one publish POST',
        );
        const publish = parsePublishedShareRequest(creation.publishRequest, 'first share');
        assert.equal(publish.token, creation.prepare.token, 'publish must use the prepared token');

        const share = await waitForSharedResponse(shareActions, env.baseURL);
        assert.equal(share.token, creation.prepare.token, 'copied URL must use the prepared token');
        const publishedSnapshot = assertPublishedSharePayload(
          publish,
          creation.prepare,
          share.key,
          plaintextMarkers,
          'first share',
        );
        assert.match(publishedSnapshot, /data-theme="1975"/, 'encrypted snapshot must freeze the selected 1975 theme');
        assert.match(publishedSnapshot, /style="[^"]*--color-background/, 'encrypted snapshot must freeze theme CSS variables');
        assert.ok(!publishedSnapshot.includes('data-theme="nord"'), 'encrypted snapshot must not use a later theme');
        assert.ok(!publishedSnapshot.includes('Fixture article: deterministic test content.'),
          'encrypted snapshot must not contain tool output');

        const shareStatus = shareActions.locator('.message-share-status');
        assert.equal(await shareStatus.getAttribute('role'), 'status', 'share feedback must be a status region');
        await waitForShareStatus(page, shareStatus, 'Link copied', {
          label: 'automatic share URL copy to report success',
        });
        const copiedAt = Date.now();
        assert.equal(await page.evaluate(() => navigator.clipboard.readText()), share.shareURL,
          'real clipboard must contain the absolute public share URL with its fragment key');
        assert.match(await page.evaluate(() => navigator.clipboard.readText()), /#key=[A-Za-z0-9_-]{43}$/,
          'clipboard share URL must carry the raw key only in the fragment');
        assert.equal(await share.copyButton.getAttribute('data-share-copy-pending'), null,
          'the one-time automatic copy marker must be consumed');
        assert.deepEqual(await iconPaths(share.copyButton), shareIcon,
          'the successful copy action must keep the same icon as the initial share action');
        const firstClearElapsed = await assertShareStatusClears(
          page,
          shareStatus,
          copiedAt,
          'automatic share copy feedback',
        );

        await share.copyButton.click();
        await waitForShareStatus(page, shareStatus, 'Link copied', {
          label: 'repeated icon action to report success',
        });
        assert.equal(await page.evaluate(() => navigator.clipboard.readText()), share.shareURL,
          'another icon click must copy the same public URL and fragment key');
        assert.equal(sharePOSTs.length, 2, 'copying the existing URL must not create another prepare or publish request');
        assert.equal(await assistant.locator('.message-share-actions').count(), 1,
          'repeated copy must keep one share action group');
        assert.equal(await shareActions.locator('button').count(), 1,
          'repeated copy must keep one icon button');
        await assertIconOnlyShareButton(share.copyButton, 'repeated copy action');
        assertNoSecretInRequests(ownerRequests, [share.key], 'owner page');
        const repeatedAt = Date.now();
        const repeatedClearElapsed = await assertShareStatusClears(
          page,
          shareStatus,
          repeatedAt,
          'repeated share copy feedback',
        );
        t.diagnostic(
          `Share feedback visible ${firstClearElapsed}ms after automatic copy and ${repeatedClearElapsed}ms ` +
          'after the repeated copy; one prepare and one publish created the encrypted snapshot.',
        );

        const publicContext = await browser.newContext({ viewport: { width: 1280, height: 900 } });
        extraContexts.push(publicContext);
        assert.deepEqual(await publicContext.cookies(), [], 'public navigation must start without session cookies');
        const publicPage = await publicContext.newPage();
        publicPage.setDefaultTimeout(15000);
        const baseOrigin = new URL(env.baseURL).origin;
        const forbiddenRequests = [];
        const publicRequests = [];
        publicPage.on('request', (request) => {
          publicRequests.push(request);
          const url = new URL(request.url());
          const type = request.resourceType();
          if (url.origin !== baseOrigin) {
            forbiddenRequests.push(`external ${type} ${url.href}`);
          }
          if (type === 'script' || type === 'stylesheet' || type === 'font') {
            forbiddenRequests.push(`${type} ${url.href}`);
          }
          if (url.pathname.startsWith('/static/')) {
            forbiddenRequests.push(`static asset ${url.href}`);
          }
        });

        const publicResponse = await publicPage.goto(share.shareURL, { waitUntil: 'load' });
        assert.ok(publicResponse, 'anonymous navigation must return a response');
        assert.equal(publicResponse.status(), 200, `anonymous public navigation returned ${publicResponse.status()}`);
        const publicHTML = Buffer.from(await publicResponse.body());
        const publicText = publicHTML.toString('utf8');
        assert.match(publicText, /<title>Encrypted shared response<\/title>/,
          'public bootstrap must use its generic title');
        assert.ok(!publicText.includes(`<title>${chatTitle}</title>`),
          'public bootstrap title must not expose the chat title');
        assertNoPlaintext(publicText, plaintextMarkers, 'public bootstrap');
        const bootstrap = parseShareBootstrapHTML(publicText, 'first public bootstrap');
        assertShareBootstrapMatches(bootstrap, publish, 'first public bootstrap');
        assert.ok(publicText.includes(bootstrap.ciphertext), 'public bootstrap must embed the stored ciphertext');
        assert.ok(!publicText.includes(share.key), 'public bootstrap must not contain the fragment key');
        assertShareCSP(publicResponse.headers(), 'public bootstrap');
        const topDocument = await publicPage.evaluate(() => ({
          scripts: document.scripts.length,
          scriptSources: Array.from(document.scripts)
            .map((script) => script.getAttribute('src') ?? '')
            .filter(Boolean),
          sharedPages: document.querySelectorAll('.shared-page').length,
          prompts: document.querySelectorAll('.shared-prompt').length,
          theme: document.documentElement.dataset.theme ?? '',
          state: document.querySelector('[data-share-root]')?.getAttribute('data-share-state') ?? '',
        }));
        assert.equal(topDocument.scripts, 1, 'public bootstrap must contain exactly one inline script');
        assert.deepEqual(topDocument.scriptSources, [], 'public bootstrap must not load external scripts');
        assert.equal(topDocument.sharedPages, 0, 'public bootstrap must not render shared article markup');
        assert.equal(topDocument.prompts, 0, 'public bootstrap must not render the shared prompt');
        assert.notEqual(topDocument.theme, '1975', 'public bootstrap must not expose the frozen theme outside the sandbox');

        await waitForShareState(publicPage, 'ready', 'public bootstrap to decrypt the first share');
        const { frameElement, frame } = await waitForSharedFrame(publicPage, 'first shared response');
        await assertFrameSandbox(frameElement, 'first shared response');
        await assertOpaqueFrameOrigin(publicPage, frameElement, 'first shared response');

        const publicLayout = await frame.locator('.shared-page').evaluate(() => {
          const pageElement = document.querySelector('.shared-page');
          const article = document.querySelector('.shared-article');
          const title = document.querySelector('.shared-title');
          const date = document.querySelector('.shared-date');
          const eyebrow = document.querySelector('.shared-eyebrow');
          const eyebrowLink = eyebrow?.matches('a') ? eyebrow : eyebrow?.querySelector('a');
          const prompt = document.querySelector('.shared-prompt');
          const promptTextElement = document.querySelector('.shared-prompt-text');
          const footer = document.querySelector('.shared-footer');
          const footerLink = footer?.querySelector('a');
          const bodyStyle = getComputedStyle(document.body);
          const titleStyle = getComputedStyle(title);
          const dateStyle = getComputedStyle(date);
          const footerLinkStyle = footerLink ? getComputedStyle(footerLink) : null;
          const eyebrowLinkStyle = eyebrowLink ? getComputedStyle(eyebrowLink) : null;
          const resourceReferences = Array.from(
            document.querySelectorAll('[src], link[href], source[src], object[data], embed[src]'),
          ).map((element) => element.getAttribute('src') ?? element.getAttribute('href') ?? element.getAttribute('data'));
          return {
            theme: document.documentElement.dataset.theme ?? '',
            themeStyle: document.documentElement.getAttribute('style') ?? '',
            pageWidth: pageElement.getBoundingClientRect().width,
            pageMaxWidth: parseFloat(getComputedStyle(pageElement).maxWidth),
            paddingTop: parseFloat(getComputedStyle(pageElement).paddingTop),
            articleWidth: article.getBoundingClientRect().width,
            titleText: title.textContent.trim(),
            titleAlign: titleStyle.textAlign,
            titleFontSize: parseFloat(titleStyle.fontSize),
            bodyFontSize: parseFloat(bodyStyle.fontSize),
            dateFontSize: parseFloat(dateStyle.fontSize),
            dateColor: dateStyle.color,
            bodyColor: bodyStyle.color,
            bodyFontFamily: bodyStyle.fontFamily,
            eyebrowCount: document.querySelectorAll('.shared-eyebrow').length,
            eyebrowText: eyebrow?.textContent.replace(/\s+/g, ' ').trim() ?? '',
            eyebrowLinkText: eyebrowLink?.textContent.trim() ?? '',
            eyebrowHref: eyebrowLink?.getAttribute('href') ?? '',
            eyebrowTarget: eyebrowLink?.getAttribute('target') ?? '',
            eyebrowRel: eyebrowLink?.getAttribute('rel') ?? '',
            eyebrowUnderlined: eyebrowLinkStyle
              ? eyebrowLinkStyle.textDecorationLine.includes('underline') ||
                (parseFloat(eyebrowLinkStyle.borderBottomWidth) > 0 &&
                  eyebrowLinkStyle.borderBottomStyle !== 'none')
              : false,
            eyebrowDateGap: eyebrow && date
              ? date.getBoundingClientRect().top - eyebrow.getBoundingClientRect().bottom
              : null,
            promptCount: document.querySelectorAll('.shared-prompt').length,
            promptTextCount: document.querySelectorAll('.shared-prompt-text').length,
            promptText: promptTextElement?.textContent.trim() ?? '',
            promptBeforeArticle: Boolean(
              prompt && article &&
              (prompt.compareDocumentPosition(article) & Node.DOCUMENT_POSITION_FOLLOWING),
            ),
            promptMarkupElements: promptTextElement
              ? promptTextElement.querySelectorAll('script, strong, em, kbd, a').length
              : -1,
            promptXssGlobal: typeof window.__sharedPromptXss,
            footerText: footer?.textContent.replace(/\s+/g, ' ').trim() ?? '',
            footerLinkCount: footer?.querySelectorAll('a').length ?? -1,
            footerLinkText: footerLink?.textContent.trim() ?? '',
            footerHref: footerLink?.getAttribute('href') ?? '',
            footerTarget: footerLink?.getAttribute('target') ?? '',
            footerRel: footerLink?.getAttribute('rel') ?? '',
            footerUnderlined: footerLinkStyle
              ? footerLinkStyle.textDecorationLine.includes('underline') ||
                (parseFloat(footerLinkStyle.borderBottomWidth) > 0 &&
                  footerLinkStyle.borderBottomStyle !== 'none')
              : false,
            scripts: document.querySelectorAll('script').length,
            stylesheetLinks: document.querySelectorAll('link[rel~="stylesheet"]').length,
            staticReferences: resourceReferences.filter((value) => value?.includes('/static/')),
            heading: document.querySelector('.shared-article h2')?.textContent.trim() ?? '',
            paragraph: document.querySelector('.shared-article p')?.textContent ?? '',
            code: document.querySelector('.shared-article pre code')?.textContent ?? '',
            tableText: document.querySelector('.shared-article table')?.textContent ?? '',
            linkText: document.querySelector('.shared-article a')?.textContent.trim() ?? '',
            linkHref: document.querySelector('.shared-article a')?.getAttribute('href') ?? '',
            xssGlobal: typeof window.__sharedXss,
          };
        });

        assert.equal(publicLayout.theme, '1975', 'decrypted snapshot must use the frozen 1975 theme');
        assert.match(publicLayout.themeStyle, /--color-background/, 'decrypted snapshot must freeze theme CSS variables');
        assert.equal(publicLayout.titleText, chatTitle);
        assert.equal(publicLayout.titleAlign, 'center', 'public title must be centered');
        assert.ok(publicLayout.titleFontSize > publicLayout.bodyFontSize * 1.5,
          'public title must be visibly larger than body text');
        assert.ok(publicLayout.dateFontSize < publicLayout.bodyFontSize, 'public date must be smaller than body text');
        assert.notEqual(publicLayout.dateColor, publicLayout.bodyColor, 'public date must use the muted text color');
        assert.ok(!/Inter|JetBrains/i.test(publicLayout.bodyFontFamily), 'public page must use system fonts');
        assert.ok(publicLayout.pageMaxWidth <= 875, 'public reading column must cap near 825px');
        assert.ok(publicLayout.articleWidth > 700 && publicLayout.articleWidth <= 830,
          `desktop article width ${publicLayout.articleWidth} must stay in the reading column`);
        assert.ok(publicLayout.paddingTop <= 40,
          `desktop public top padding ${publicLayout.paddingTop}px must stay at or below 40px`);
        assert.equal(publicLayout.eyebrowCount, 1, 'public page must render exactly one Kritui eyebrow');
        assert.equal(publicLayout.eyebrowText, 'Kritui', 'public eyebrow must show the Kritui wordmark');
        assert.equal(publicLayout.eyebrowLinkText, 'Kritui', 'public eyebrow Kritui must be linked');
        assert.equal(new URL(publicLayout.eyebrowHref).href, 'https://kritui.skrittle.net/',
          'public eyebrow must link to the Kritui site');
        assert.equal(publicLayout.eyebrowTarget, '_blank', 'public eyebrow must open in a new tab');
        assert.match(publicLayout.eyebrowRel, /\bnoopener\b/, 'public eyebrow link must use noopener');
        assert.equal(publicLayout.eyebrowUnderlined, true, 'public eyebrow Kritui link must be underlined');
        assert.ok(publicLayout.eyebrowDateGap !== null && publicLayout.eyebrowDateGap >= 0 &&
          publicLayout.eyebrowDateGap <= 16,
        `public eyebrow to date gap ${publicLayout.eyebrowDateGap}px must stay at or below 16px`);
        assert.equal(publicLayout.promptCount, 1, 'public page must render exactly one shared prompt');
        assert.equal(publicLayout.promptTextCount, 1, 'public page must render exactly one shared prompt text');
        assert.equal(publicLayout.promptText, prompt, 'public shared prompt must match the nearest user message exactly');
        assert.equal(publicLayout.promptBeforeArticle, true,
          'public shared prompt must appear before the assistant article');
        assert.equal(publicLayout.promptMarkupElements, 0,
          'public shared prompt must remain escaped plaintext without rendered markup');
        assert.equal(publicLayout.promptXssGlobal, 'undefined',
          'literal script text in the shared prompt must not execute');
        assert.equal(publicLayout.footerText, 'Shared from Kritui', 'public footer must use the shared-from wording');
        assert.equal(publicLayout.footerLinkCount, 1, 'public footer must link only the Kritui word');
        assert.equal(publicLayout.footerLinkText, 'Kritui', 'public footer link must contain only Kritui');
        assert.equal(new URL(publicLayout.footerHref).href, 'https://kritui.skrittle.net/',
          'public footer Kritui link must point to the Kritui site');
        assert.equal(publicLayout.footerTarget, '_blank', 'public footer link must open in a new tab');
        assert.match(publicLayout.footerRel, /\bnoopener\b/, 'public footer link must use noopener');
        assert.equal(publicLayout.footerUnderlined, true, 'public footer Kritui link must be underlined');
        assert.equal(publicLayout.scripts, 0, 'decrypted shared page must contain no executable scripts');
        assert.equal(publicLayout.stylesheetLinks, 0, 'public page must not link external stylesheets');
        assert.deepEqual(publicLayout.staticReferences, [], 'public page must not reference application static assets');
        assert.equal(publicLayout.heading, 'Overview');
        assert.match(publicLayout.paragraph, /A public reading surface/);
        assert.match(publicLayout.code, /const frozen/);
        assert.match(publicLayout.tableText, /Alpha/);
        assert.match(publicLayout.tableText, /Wide/);
        assert.equal(publicLayout.linkText, 'reference');
        assert.equal(publicLayout.linkHref, 'https://example.com/reference');
        assert.equal(publicLayout.xssGlobal, 'undefined', 'public Markdown must remain sanitized');
        assert.deepEqual(forbiddenRequests, [], 'public page must not fetch external CSS, JavaScript, or fonts');
        assertNoSecretInRequests(publicRequests, [share.key], 'public page');
        assertShortTableWordsOneLine(await shortTableWordLineCounts(frame), 'desktop 1280px');
        await capturePublicScreenshot(t, publicPage, 'public-response-desktop');

        const headResponse = await publicContext.request.head(share.rawURL);
        assert.equal(headResponse.status(), 200, `HEAD public response returned ${headResponse.status()}`);
        assert.equal((await headResponse.body()).length, 0, 'HEAD public response must have an empty body');
        assert.match(headResponse.headers()['content-type'] ?? '', /^text\/html/, 'HEAD must advertise HTML');
        assert.equal(Number(headResponse.headers()['content-length'] ?? 0), publicHTML.length,
          'HEAD must retain the ciphertext bootstrap content length');
        assertShareCSP(headResponse.headers(), 'HEAD public bootstrap');

        await useViewport(publicPage, 390, 844);
        await waitForStableLayout(publicPage);
        const mobileLayout = await frame.locator('.shared-page').evaluate(() => {
          const pre = document.querySelector('.shared-article pre');
          const table = document.querySelector('.shared-article table');
          return {
            innerWidth: window.innerWidth,
            documentScrollWidth: document.documentElement.scrollWidth,
            bodyScrollWidth: document.body.scrollWidth,
            pageWidth: document.querySelector('.shared-page').getBoundingClientRect().width,
            paddingTop: parseFloat(getComputedStyle(document.querySelector('.shared-page')).paddingTop),
            preOverflow: getComputedStyle(pre).overflowX,
            preScrolls: pre.scrollWidth > pre.clientWidth,
            tableOverflow: getComputedStyle(table).overflowX,
            tableWidth: table.getBoundingClientRect().width,
          };
        });
        assert.ok(mobileLayout.documentScrollWidth <= mobileLayout.innerWidth + 1,
          `mobile page overflowed: ${mobileLayout.documentScrollWidth}px in ${mobileLayout.innerWidth}px`);
        assert.ok(mobileLayout.bodyScrollWidth <= mobileLayout.innerWidth + 1,
          `mobile body overflowed: ${mobileLayout.bodyScrollWidth}px in ${mobileLayout.innerWidth}px`);
        assert.ok(mobileLayout.pageWidth <= mobileLayout.innerWidth,
          `mobile reading column ${mobileLayout.pageWidth}px must fit ${mobileLayout.innerWidth}px`);
        assert.ok(mobileLayout.paddingTop <= 24,
          `mobile public top padding ${mobileLayout.paddingTop}px must stay at or below 24px`);
        assert.equal(mobileLayout.preOverflow, 'auto', 'wide code blocks must scroll inside the article');
        assert.equal(mobileLayout.preScrolls, true, 'wide code fixture must overflow its scroll container');
        assert.equal(mobileLayout.tableOverflow, 'auto', 'wide tables must stay inside a horizontal scroll container');
        assert.ok(mobileLayout.tableWidth <= mobileLayout.innerWidth + 1,
          `mobile table ${mobileLayout.tableWidth}px must not exceed ${mobileLayout.innerWidth}px`);
        assertShortTableWordsOneLine(await shortTableWordLineCounts(frame), 'mobile 390px');
        await capturePublicScreenshot(t, publicPage, 'public-response-mobile');

        await selectTheme(page, 'nord');
        assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), 'nord',
          'sharer theme change must apply to the private application');
        const afterThemeResponse = await publicContext.request.get(share.rawURL);
        assert.equal(afterThemeResponse.status(), 200);
        assert.deepEqual(Buffer.from(await afterThemeResponse.body()), publicHTML,
          'public ciphertext bootstrap bytes must stay frozen after the sharer changes theme');
        const frozenResponse = await publicPage.reload({ waitUntil: 'load' });
        assert.equal(frozenResponse.status(), 200, 'frozen first share must remain readable after the theme change');
        await waitForShareState(publicPage, 'ready', 'frozen first share to decrypt after the theme change');
        const frozenFrame = await waitForSharedFrame(publicPage, 'frozen first shared response');
        assert.equal(
          await frozenFrame.frame.locator('.shared-page').evaluate(() => document.documentElement.dataset.theme),
          '1975',
          'first encrypted snapshot must stay frozen on the 1975 theme after the sharer changes theme',
        );

        await reloadChat(page, chatID);
        await waitForHistoryText(page, 'A public reading surface for one finished response.');
        assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), 'nord',
          'reloaded owner chat must retain the Nord theme');
        const nordAssistant = page.locator('#message-history article.message')
          .filter({ hasText: 'A public reading surface for one finished response.' })
          .first();
        await nordAssistant.waitFor({ state: 'visible', timeout: 10000 });
        const nordShareActions = nordAssistant.locator('.message-share-actions');
        const nordShareAction = await nordShareActions.getAttribute('action');
        assert.match(nordShareAction ?? '', /^\/chats\/\d+\/messages\/\d+\/share$/, 'Nord share form action is missing');
        const nordCreation = await performShare(
          page,
          nordAssistant.getByRole('button', { name: 'Share response' }),
          nordShareAction,
        );
        const nordPublish = parsePublishedShareRequest(nordCreation.publishRequest, 'Nord share');
        const nordShare = await waitForSharedResponse(nordShareActions, env.baseURL);
        assert.notEqual(nordShare.shareURL, share.shareURL, 'a second share must create a distinct public URL');
        assert.notEqual(nordShare.token, share.token, 'a second share must create a distinct public token');
        const nordSnapshot = assertPublishedSharePayload(
          nordPublish,
          nordCreation.prepare,
          nordShare.key,
          plaintextMarkers,
          'Nord share',
        );
        assert.match(nordSnapshot, /data-theme="nord"/, 'second encrypted snapshot must freeze the Nord theme');
        const nordShareStatus = nordShareActions.locator('.message-share-status');
        await waitForShareStatus(page, nordShareStatus, 'Link copied', {
          label: 'automatic Nord share URL copy to report success',
        });
        assert.equal(await page.evaluate(() => navigator.clipboard.readText()), nordShare.shareURL,
          'real clipboard must contain the new Nord public share URL with its fragment key');

        await useViewport(publicPage, 1280, 900);
        const nordPublicResponse = await publicPage.goto(nordShare.shareURL, { waitUntil: 'load' });
        assert.ok(nordPublicResponse, 'anonymous Nord navigation must return a response');
        assert.equal(nordPublicResponse.status(), 200,
          `anonymous Nord public navigation returned ${nordPublicResponse.status()}`);
        const nordHTML = Buffer.from(await nordPublicResponse.body());
        const nordText = nordHTML.toString('utf8');
        assertNoPlaintext(nordText, plaintextMarkers, 'Nord public bootstrap');
        const nordBootstrap = parseShareBootstrapHTML(nordText, 'Nord public bootstrap');
        assertShareBootstrapMatches(nordBootstrap, nordPublish, 'Nord public bootstrap');
        assertShareCSP(nordPublicResponse.headers(), 'Nord public bootstrap');
        assert.notDeepEqual(nordHTML, publicHTML, 'second encrypted bootstrap must store different ciphertext');
        await waitForShareState(publicPage, 'ready', 'public bootstrap to decrypt the Nord share');
        const nordFrameState = await waitForSharedFrame(publicPage, 'Nord shared response');
        await assertFrameSandbox(nordFrameState.frameElement, 'Nord shared response');
        const nordPublic = await nordFrameState.frame.locator('.shared-page').evaluate(() => {
          const rootStyle = getComputedStyle(document.documentElement);
          const bodyStyle = getComputedStyle(document.body);
          const probe = document.createElement('span');
          probe.style.backgroundColor = 'var(--color-background)';
          probe.style.display = 'none';
          document.body.append(probe);
          const expectedBackground = getComputedStyle(probe).backgroundColor;
          probe.remove();
          return {
            theme: document.documentElement.dataset.theme,
            colorScheme: rootStyle.colorScheme,
            bodyBackground: bodyStyle.backgroundColor,
            expectedBackground,
          };
        });
        assert.equal(nordPublic.theme, 'nord', 'second public snapshot must use the Nord theme');
        assert.match(nordPublic.colorScheme, /dark/, 'public Nord root must declare a dark color scheme');
        assert.equal(nordPublic.bodyBackground, nordPublic.expectedBackground,
          'public Nord body background must match the frozen --color-background');
        await capturePublicScreenshot(t, publicPage, 'public-response-nord-dark');

        await env.restartApp({ signal: 'SIGKILL' });
        await reloadChat(page, chatID);
        await waitForHistoryText(page, 'A public reading surface for one finished response.');
        const restartedContext = await browser.newContext();
        extraContexts.push(restartedContext);
        assert.deepEqual(await restartedContext.cookies(), [], 'post-restart public checks must start without cookies');
        const afterRestartResponse = await restartedContext.request.get(share.rawURL);
        assert.equal(afterRestartResponse.status(), 200);
        assert.deepEqual(Buffer.from(await afterRestartResponse.body()), publicHTML,
          'public ciphertext bootstrap bytes must survive an application restart');

        const undoStatus = await page.evaluate(async (id) => {
          const response = await fetch(`/messages?chat=${id}`, {
            method: 'POST',
            headers: {
              'Content-Type': 'application/x-www-form-urlencoded',
              'HX-Request': 'true',
            },
            body: new URLSearchParams({ message: '/undo' }),
          });
          return response.status;
        }, chatID);
        assert.ok(undoStatus >= 200 && undoStatus < 300, `undo returned ${undoStatus}`);
        await reloadChat(page, chatID);
        await waitUntil(async () => {
          return !(await page.locator('#message-history').innerText()).includes('A public reading surface for one finished response.');
        }, {
          timeout: 10000,
          label: 'undo to remove the shared source from private history',
        });
        const afterUndoResponse = await restartedContext.request.get(share.rawURL);
        assert.equal(afterUndoResponse.status(), 200);
        assert.deepEqual(Buffer.from(await afterUndoResponse.body()), publicHTML,
          'public ciphertext bootstrap bytes must survive removal of the source message');
      } finally {
        for (const context of extraContexts) {
          await context.close().catch(() => {});
        }
      }
    },
  });
});

test('encrypted public response share keeps a usable fragment link and rejects tampering and foreign access', { timeout: 300000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'encrypted public share failure and security',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const promptMarker = 'PROMPT-SENTINEL-copy-failure-4a18';
      const replyMarker = 'REPLY-SENTINEL-copy-failure-8c92';
      const prompt = `${promptMarker} Copy failure source prompt`;
      const reply = `Copy failure answer body. ${replyMarker}`;
      const plaintextMarkers = [promptMarker, replyMarker, prompt, reply, 'Copy failure answer body.'];
      const extraContexts = [];
      const ownerRequests = [];
      const sharePOSTs = [];
      page.on('request', (request) => {
        ownerRequests.push(request);
        const url = new URL(request.url());
        if (request.method() === 'POST' && /^\/chats\/\d+\/messages\/\d+\/share(?:\/publish)?$/.test(url.pathname)) {
          sharePOSTs.push(url.pathname);
        }
      });

      try {
        await signup(page, username('sharecopy'), password);
        const { chatID } = await openChat(page);
        await submitPrompt(page, prompt);
        await waitCompletionActive(page);
        await provider.waitForCallCount(1);
        await provider.release(0, finalResponse(reply));
        await waitCompletionDone(page);
        await waitForHistoryText(page, reply);

        const assistant = page.locator('#message-history article.message').filter({ hasText: reply }).first();
        await assistant.waitFor({ state: 'visible', timeout: 10000 });
        const shareAction = await assistant.locator('.message-share-actions').getAttribute('action');
        assert.match(shareAction ?? '', /^\/chats\/\d+\/messages\/\d+\/share$/, 'share form action is missing');
        const shareActionMatch = /^\/chats\/(\d+)\/messages\/(\d+)\/share$/.exec(shareAction);
        assert.ok(shareActionMatch, 'share action must expose the chat and message IDs');

        const nativePost = await page.request.post(`${env.baseURL}${shareAction}`, {
          form: {},
          maxRedirects: 0,
        });
        assert.equal(nativePost.status(), 400, 'native share preparation without HX-Request must be rejected');
        assert.match(await nativePost.text(), /JavaScript/i,
          'native share preparation rejection must explain that JavaScript is required');

        const preparedFixture = await prepareShareFixture(page, env.baseURL, shareAction);
        const fixtureRawURL = new URL(`/s/${preparedFixture.token}`, env.baseURL).href;
        const beforePublish = await page.request.get(fixtureRawURL, { maxRedirects: 0 });
        assert.equal(beforePublish.status(), 404,
          'preparing a share must not create a public record before publish');

        const overrideApplied = await page.evaluate(() => {
          const original = navigator.clipboard;
          if (!original?.writeText) {
            return false;
          }
          window.__krituiShareClipboard = original;
          window.__krituiShareExecCommand = document.execCommand;
          Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: {
              writeText: () => Promise.reject(new Error('clipboard denied by test')),
              readText: () => original.readText(),
            },
          });
          document.execCommand = () => false;
          return true;
        });
        assert.equal(overrideApplied, true, 'test could not force clipboard denial');

        sharePOSTs.length = 0;
        const creation = await performShare(
          page,
          assistant.getByRole('button', { name: 'Share response' }),
          shareAction,
        );
        assert.deepEqual(
          sharePOSTs,
          [shareAction, `${shareAction}/publish`],
          'one UI share must send exactly one prepare and one publish POST',
        );
        const publish = parsePublishedShareRequest(creation.publishRequest, 'copy-failure share');
        const shareActions = assistant.locator('.message-share-actions');
        const share = await waitForSharedResponse(shareActions, env.baseURL);
        assert.equal(share.token, creation.prepare.token, 'copy URL must use the prepared token');
        const decrypted = assertPublishedSharePayload(
          publish,
          creation.prepare,
          share.key,
          plaintextMarkers,
          'copy-failure share',
        );
        assert.match(decrypted, new RegExp(promptMarker), 'decrypted snapshot must include the preceding prompt');
        assert.match(decrypted, new RegExp(replyMarker), 'decrypted snapshot must include the assistant response');

        const shareStatus = shareActions.locator('.message-share-status');
        await waitForShareStatus(page, shareStatus, 'Copy failed. Try again.', {
          label: 'clipboard denial to render the copy failure message',
        });
        await assertIconOnlyShareButton(share.copyButton, 'failed copy action');

        await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: env.baseURL });
        await page.evaluate(() => {
          Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: window.__krituiShareClipboard,
          });
          document.execCommand = window.__krituiShareExecCommand;
        });
        const postsBeforeRetry = sharePOSTs.length;
        await share.copyButton.click();
        await waitForShareStatus(page, shareStatus, 'Link copied', {
          label: 'icon retry to succeed after clipboard access returns',
        });
        assert.equal(await page.evaluate(() => navigator.clipboard.readText()), share.shareURL,
          'retry must copy the public share URL with its fragment key');
        assert.equal(sharePOSTs.length, postsBeforeRetry,
          'copying an existing URL must not send another prepare or publish request');
        assert.equal(await shareActions.locator('button').count(), 1,
          'copy failure and retry must keep one icon button');
        assertNoSecretInRequests(ownerRequests, [share.key], 'owner page');

        const anonymousContext = await browser.newContext();
        extraContexts.push(anonymousContext);
        assert.deepEqual(await anonymousContext.cookies(), [], 'anonymous checks must start without cookies');
        const anonymousPrepare = await anonymousContext.request.post(`${env.baseURL}${shareAction}`, {
          form: {},
          maxRedirects: 0,
        });
        assert.equal(anonymousPrepare.status(), 303, 'anonymous share preparation must redirect to login');
        assert.match(anonymousPrepare.headers().location ?? '', /\/login/, 'anonymous prepare redirect must target login');
        const foreignNonce = crypto.randomBytes(12).toString('base64url');
        const foreignCiphertext = crypto.randomBytes(32).toString('base64url');
        const anonymousPublish = await anonymousContext.request.post(`${env.baseURL}${shareAction}/publish`, {
          form: {
            token: preparedFixture.token,
            format_version: '1',
            nonce: foreignNonce,
            ciphertext: foreignCiphertext,
          },
          maxRedirects: 0,
        });
        assert.equal(anonymousPublish.status(), 303, 'anonymous share publish must redirect to login');
        assert.match(anonymousPublish.headers().location ?? '', /\/login/, 'anonymous publish redirect must target login');
        const anonymousPrivate = await anonymousContext.request.get(
          `${env.baseURL}/chats/${chatID}/summary`,
          { maxRedirects: 0 },
        );
        assert.equal(anonymousPrivate.status(), 303, 'private source routes must remain protected');
        const anonymousAttachment = await anonymousContext.request.get(
          `${env.baseURL}/chats/${shareActionMatch[1]}/messages/${shareActionMatch[2]}/attachments/0`,
          { maxRedirects: 0 },
        );
        assert.equal(anonymousAttachment.status(), 303, 'private message attachments must remain protected');

        const invalidTokens = [
          'a'.repeat(43),
          'not-a-token',
          crypto.randomBytes(32).toString('base64url'),
        ];
        for (const token of invalidTokens) {
          const invalid = await anonymousContext.request.get(`${env.baseURL}/s/${token}`, { maxRedirects: 0 });
          assert.equal(invalid.status(), 404, `unknown public token ${token} must return 404`);
        }

        const otherContext = await browser.newContext();
        extraContexts.push(otherContext);
        const otherPage = await otherContext.newPage();
        otherPage.setDefaultTimeout(15000);
        await signup(otherPage, username('shareother'), password);
        const foreignPrepare = await otherPage.request.post(`${env.baseURL}${shareAction}`, {
          form: {},
          headers: { 'HX-Request': 'true' },
          maxRedirects: 0,
        });
        assert.equal(foreignPrepare.status(), 404,
          'a different account must not prepare another account response');
        const foreignPublish = await otherPage.request.post(`${env.baseURL}${shareAction}/publish`, {
          form: {
            token: preparedFixture.token,
            format_version: '1',
            nonce: foreignNonce,
            ciphertext: foreignCiphertext,
          },
          headers: { 'HX-Request': 'true' },
          maxRedirects: 0,
        });
        assert.equal(foreignPublish.status(), 404,
          'a different account must not publish another account response');

        const publicContext = await browser.newContext({ viewport: { width: 1280, height: 900 } });
        extraContexts.push(publicContext);
        assert.deepEqual(await publicContext.cookies(), [], 'public security checks must start without cookies');
        const missingPage = await publicContext.newPage();
        missingPage.setDefaultTimeout(15000);
        await assertShareFails(missingPage, share.rawURL, plaintextMarkers, 'missing-fragment share');
        const wrongKey = crypto.randomBytes(32).toString('base64url');
        assert.notEqual(wrongKey, share.key, 'wrong-key fixture must differ from the real key');
        const wrongPage = await publicContext.newPage();
        wrongPage.setDefaultTimeout(15000);
        await assertShareFails(
          wrongPage,
          `${share.rawURL}#key=${wrongKey}`,
          plaintextMarkers,
          'wrong-key share',
        );
        await capturePublicScreenshot(t, wrongPage, 'share-error-wrong-key');
        const malformedPage = await publicContext.newPage();
        malformedPage.setDefaultTimeout(15000);
        await assertShareFails(
          malformedPage,
          `${share.rawURL}#key=not-a-canonical-key`,
          plaintextMarkers,
          'malformed-key share',
        );

        const bootstrapResponse = await publicContext.request.get(share.rawURL);
        assert.equal(bootstrapResponse.status(), 200);
        const bootstrapHTML = await bootstrapResponse.text();
        const tampered = tamperShareBootstrap(bootstrapHTML, 'tampered bootstrap fixture');
        assert.equal(tampered.bootstrap.ciphertext, publish.ciphertext,
          'tamper fixture must start from the published ciphertext');
        const tamperPage = await publicContext.newPage();
        tamperPage.setDefaultTimeout(15000);
        await tamperPage.route(share.rawURL, (route) => route.fulfill({
          status: 200,
          contentType: 'text/html; charset=utf-8',
          body: tampered.tamperedHTML,
        }));
        try {
          await assertShareFails(tamperPage, share.shareURL, plaintextMarkers, 'tampered-ciphertext share');
          await capturePublicScreenshot(t, tamperPage, 'share-error-tampered-ciphertext');
        } finally {
          await tamperPage.unroute(share.rawURL);
        }

        const maliciousMarker = 'MALICIOUS-SENTINEL-encrypted-fixture-5f27';
        const maliciousPrepare = await prepareShareFixture(page, env.baseURL, shareAction);
        const maliciousHTML = [
          '<!DOCTYPE html>',
          '<html lang="en" data-theme="1975">',
          '<head><meta charset="utf-8"><title>Malicious fixture</title></head>',
          '<body>',
          '<main class="shared-page">',
          `<h1 class="shared-title">${maliciousMarker}</h1>`,
          '<article class="shared-article">',
          `<p>${maliciousMarker}</p>`,
          '<script>',
          'window.__maliciousExecuted = true;',
          `try { fetch('/chats/${chatID}/summary', { credentials: 'include' }); } catch (error) {}`,
          "try { window.__parentRead = parent.document.documentElement.outerHTML.length; } catch (error) { window.__parentError = error.name; }",
          '</script>',
          `<img alt="" src="data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==" onload="window.__maliciousOnload = true; try { fetch('/chats/${chatID}/summary', { credentials: 'include' }); } catch (error) {}">`,
          '</article>',
          '</main>',
          '</body>',
          '</html>',
        ].join('\n');
        const maliciousCipher = await encryptShareFixture(page, maliciousHTML);
        assert.match(maliciousCipher.key, /^[A-Za-z0-9_-]{43}$/, 'malicious fixture key must be canonical base64url');
        assert.equal(Buffer.from(maliciousCipher.key, 'base64url').length, 32,
          'malicious fixture key must be 32 bytes');
        assert.equal(Buffer.from(maliciousCipher.nonce, 'base64url').length, 12,
          'malicious fixture nonce must be 12 bytes');
        assert.notEqual(maliciousCipher.key, share.key, 'malicious fixture must use a fresh key');
        assert.notEqual(maliciousCipher.nonce, publish.nonce, 'malicious fixture must use a fresh nonce');
        const maliciousPublish = await publishShareFixture(
          page,
          env.baseURL,
          shareAction,
          maliciousPrepare,
          maliciousCipher,
        );
        assert.equal(maliciousPublish.sharePath, `/s/${maliciousPrepare.token}`,
          'malicious fixture must use the owned prepare token and form action');

        const maliciousViews = [
          { label: 'anonymous malicious fixture', page: await publicContext.newPage() },
          { label: 'other-account malicious fixture', page: otherPage },
        ];
        for (const view of maliciousViews) {
          view.page.setDefaultTimeout(15000);
          const requests = [];
          const listener = (request) => requests.push(request);
          view.page.on('request', listener);
          try {
            const response = await view.page.goto(maliciousPublish.shareURL, { waitUntil: 'load' });
            assert.ok(response, `${view.label} navigation must return a response`);
            assert.equal(response.status(), 200, `${view.label} returned ${response.status()}`);
            const maliciousBootstrap = Buffer.from(await response.body()).toString('utf8');
            assertNoPlaintext(maliciousBootstrap, [maliciousMarker], view.label);
            assert.ok(!maliciousBootstrap.includes(maliciousCipher.key), `${view.label} bootstrap must not contain the key`);
            assertShareBootstrapMatches(
              parseShareBootstrapHTML(maliciousBootstrap, `${view.label} bootstrap`),
              { formatVersion: '1', nonce: maliciousCipher.nonce, ciphertext: maliciousCipher.ciphertext },
              `${view.label} bootstrap`,
            );
            await waitForShareState(view.page, 'ready', `${view.label} to decrypt`);
            const { frameElement, frame } = await waitForSharedFrame(view.page, view.label);
            await assertFrameSandbox(frameElement, view.label);
            await assertOpaqueFrameOrigin(view.page, frameElement, view.label);
            const state = await frame.locator('.shared-page').evaluate(() => ({
              marker: document.querySelector('.shared-title')?.textContent.trim() ?? '',
              executed: typeof window.__maliciousExecuted,
              onload: typeof window.__maliciousOnload,
              parentRead: typeof window.__parentRead,
              parentError: typeof window.__parentError,
              scripts: document.querySelectorAll('script').length,
            }));
            assert.equal(state.marker, maliciousMarker, `${view.label} must render the decrypted fixture`);
            assert.equal(state.executed, 'undefined', `${view.label} inline script must not execute`);
            assert.equal(state.onload, 'undefined', `${view.label} iframe event handler must not execute`);
            assert.equal(state.parentRead, 'undefined', `${view.label} must not read parent DOM`);
            assert.equal(state.parentError, 'undefined', `${view.label} fixture script must not run to observe parent access`);
            assert.equal(state.scripts, 0, `${view.label} must strip script elements before rendering`);
            await capturePublicScreenshot(
              t,
              view.page,
              view.label === 'anonymous malicious fixture'
                ? 'share-security-malicious-anonymous'
                : 'share-security-malicious-other-account',
            );
            const topDocument = await view.page.content();
            assert.ok(!topDocument.includes(maliciousMarker),
              `${view.label} parent document must not contain decrypted plaintext`);
            const privateRequests = requests.filter((request) => {
              try {
                return new URL(request.url()).pathname.startsWith('/chats/');
              } catch {
                return false;
              }
            });
            assert.deepEqual(privateRequests, [],
              `${view.label} must not issue private application fetches`);
            assertNoSecretInRequests(requests, [maliciousCipher.key], view.label);
          } finally {
            view.page.off('request', listener);
          }
        }
      } finally {
        for (const context of extraContexts) {
          await context.close().catch(() => {});
        }
      }
    },
  });
});

async function readDatabaseFiles(directory, root = directory) {
  const entries = await fs.readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const fullPath = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...await readDatabaseFiles(fullPath, root));
    } else if (entry.isFile() && entry.name.startsWith('data.db')) {
      files.push({
        name: path.relative(root, fullPath),
        data: await fs.readFile(fullPath),
      });
    }
  }
  return files;
}

test('server-key field encryption stays transparent to settings, provider, history, and restart', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'server-key field encryption',
    browser,
    build,
    appEnv: { LLM_KEY: null, LLM_ENDPOINT: null },
    async run(env) {
      const { page, provider } = env;
      const suffix = `${Date.now().toString(36)}_${process.pid.toString(36)}`;
      const prompt = `encrypted-storage prompt ${suffix}`;
      const reply = `encrypted-storage reply ${suffix}`;
      const followup = `encrypted-storage followup ${suffix}`;
      const followupReply = `encrypted-storage followup reply ${suffix}`;
      const title = `encrypted-storage title ${suffix}`;
      const apiKey = `sk-encrypted-storage-${suffix}`;
      const endpoint = provider.chatURL;

      await signup(page, username('crypt'), password);
      const { chatID } = await openChat(page);

      await page.locator('#settings-button').click();
      await waitForPanels(page, 'settings-page');
      await page.locator('#llm-settings').waitFor({ state: 'visible', timeout: 10000 });
      await page.locator('#llm-endpoint').fill(endpoint);
      await page.locator('#llm-api-key').fill(apiKey);

      const saveResponsePromise = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === 'POST' && url.pathname === '/settings';
      }, { timeout: 20000 });
      await page.locator('#settings-page .settings-save').click();
      const saveResponse = await saveResponsePromise;
      assert.ok(
        saveResponse.status() >= 200 && saveResponse.status() < 400,
        `settings save returned ${saveResponse.status()}`,
      );
      await page.locator('[data-settings-saved]').waitFor({ state: 'visible', timeout: 15000 });
      await waitUntil(async () => (await page.locator('#llm-api-key').inputValue()) === '', {
        timeout: 10000,
        label: 'saved LLM API key input to clear',
      });
      assert.equal(
        await page.locator('#llm-endpoint').inputValue(),
        endpoint,
        'saved LLM endpoint must remain visible in settings',
      );
      assert.equal(
        await page.locator('#clear-llm-api-key').count(),
        1,
        'a stored LLM key must expose the clear control',
      );
      const savedSettingsHTML = await page.locator('#settings-page').innerHTML();
      assert.ok(
        !savedSettingsHTML.includes(apiKey),
        'settings HTML must not echo the saved LLM API key',
      );

      await page.locator('#settings-button').click();
      await waitForPanels(page, null);

      await submitPrompt(page, prompt);
      await waitCompletionActive(page);
      const firstCall = await provider.waitForCallCount(1);
      assert.equal(
        firstCall.headers.authorization,
        `Bearer ${apiKey}`,
        'provider must receive the decrypted bearer key stored in settings',
      );
      assert.equal(firstCall.path, '/v1/chat/completions', 'provider must receive the saved full endpoint');
      assert.equal(lastUser(firstCall), prompt, 'provider must receive the decrypted prompt text');
      await provider.release(firstCall.index, finalResponse(reply));
      await waitCompletionDone(page);
      await waitForHistoryText(page, reply);

      await reloadChat(page, chatID);
      await waitForHistoryText(page, prompt);
      await waitForHistoryText(page, reply);

      await submitPrompt(page, `/rename ${title}`);
      const renamedSummary = await chatSummaryHTML(env, chatID, 'renamed chat');
      assert.ok(renamedSummary.includes(title), `renamed chat summary must contain ${title}: ${renamedSummary}`);
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => {
        const currentTitle = page.locator('#history-entries .history-entry.current .history-title-link strong').first();
        if ((await currentTitle.count()) !== 1) {
          return false;
        }
        return (await currentTitle.innerText()).trim() === title;
      }, {
        timeout: 10000,
        label: 'slash rename to reach the open History title',
      });
      await page.locator('#history-button').click();
      await waitForPanels(page, null);

      const appCwd = env.app.cwd;
      await env.app.stop('SIGTERM');
      env.app = null;

      const databaseFiles = await readDatabaseFiles(appCwd);
      assert.ok(databaseFiles.length > 0, `no data.db files found under ${appCwd}`);
      const primaryDatabase = databaseFiles.find((file) => file.name === 'data.db');
      assert.ok(primaryDatabase, `data.db is missing under ${appCwd}`);
      assert.equal(
        primaryDatabase.data.subarray(0, 16).toString('latin1'),
        'SQLite format 3\u0000',
        'data.db must keep a plaintext SQLite header because encryption is per field, not whole-file',
      );

      const plaintextMarkers = [prompt, reply, title, apiKey];
      for (const file of databaseFiles) {
        for (const marker of plaintextMarkers) {
          assert.equal(
            file.data.includes(Buffer.from(marker, 'utf8')),
            false,
            `${file.name} must not contain plaintext ${JSON.stringify(marker)}`,
          );
        }
      }
      assert.ok(
        primaryDatabase.data.includes(Buffer.from('enc:v1:', 'utf8')),
        'data.db must contain encrypted field envelopes',
      );

      const { DatabaseSync } = require('node:sqlite');
      const storedDatabase = new DatabaseSync(path.join(appCwd, 'data.db'), { readOnly: true });
      try {
        const storedMessages = storedDatabase.prepare(
          "SELECT role, content FROM messages WHERE role IN ('user', 'assistant') ORDER BY id",
        ).all();
        assert.ok(storedMessages.length >= 2, `expected encrypted user and assistant rows, saw ${storedMessages.length}`);
        for (const message of storedMessages) {
          const content = String(message.content);
          assert.match(content, /^enc:v1:/, `${message.role} message content must be an encrypted envelope`);
          assert.ok(!content.includes(apiKey), `${message.role} message content must not contain the LLM API key`);
        }

        const storedSettings = storedDatabase.prepare(
          'SELECT llm_api_key FROM settings WHERE llm_api_key IS NOT NULL',
        ).all();
        assert.equal(storedSettings.length, 1, `expected one stored LLM key row, saw ${storedSettings.length}`);
        const storedKey = String(storedSettings[0].llm_api_key);
        assert.match(storedKey, /^enc:v1:/, 'settings.llm_api_key must be an encrypted envelope');
        assert.ok(!storedKey.includes(apiKey), 'settings.llm_api_key must not contain the plaintext key');

        const storedChat = storedDatabase.prepare('SELECT title FROM chats WHERE id = ?').get(Number(chatID));
        assert.ok(storedChat, `chat ${chatID} is missing from the stopped database`);
        const storedTitle = String(storedChat.title);
        assert.match(storedTitle, /^enc:v1:/, 'chats.title must be an encrypted envelope');
        assert.ok(!storedTitle.includes(title), 'chats.title must not contain the plaintext title');
      } finally {
        storedDatabase.close();
      }

      await env.startApp();
      await reloadChat(page, chatID);
      await waitForHistoryText(page, prompt);
      await waitForHistoryText(page, reply);
      await page.locator('#history-button').click();
      await waitForPanels(page, 'history-page');
      await waitUntil(async () => {
        const currentTitle = page.locator('#history-entries .history-entry.current .history-title-link strong').first();
        if ((await currentTitle.count()) !== 1) {
          return false;
        }
        return (await currentTitle.innerText()).trim() === title;
      }, {
        timeout: 10000,
        label: 'renamed title to survive restart',
      });
      await page.locator('#history-button').click();
      await waitForPanels(page, null);

      await submitPrompt(page, followup);
      await waitCompletionActive(page);
      const followupCall = await provider.waitForCallCount(2);
      assert.equal(
        followupCall.headers.authorization,
        `Bearer ${apiKey}`,
        'provider must receive the same decrypted bearer key after restart',
      );
      assert.equal(lastUser(followupCall), followup, 'provider must receive the new follow-up prompt');
      assert.ok(
        followupCall.messages.some((message) => message.role === 'user' && message.content === prompt),
        'restarted provider history must include the decrypted first prompt',
      );
      assert.ok(
        followupCall.messages.some((message) => message.role === 'assistant' && message.content === reply),
        'restarted provider history must include the decrypted first reply',
      );
      await provider.release(followupCall.index, finalResponse(followupReply));
      await waitCompletionDone(page);
      await waitForHistoryText(page, followupReply);
      assertNoCallOverride(provider.calls, 2);
    },
  });
});

function buildTinyPDF(marker) {
  const content = `BT\n/F1 12 Tf\n20 100 Td\n(${marker}) Tj\nET\n`;
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
    `<< /Length ${Buffer.byteLength(content, 'latin1')} >>\nstream\n${content}endstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ];
  let pdf = '%PDF-1.4\n';
  const offsets = [];
  objects.forEach((body, index) => {
    offsets.push(Buffer.byteLength(pdf, 'latin1'));
    pdf += `${index + 1} 0 obj\n${body}\nendobj\n`;
  });
  const xrefOffset = Buffer.byteLength(pdf, 'latin1');
  pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  for (const offset of offsets) {
    pdf += `${String(offset).padStart(10, '0')} 00000 n \n`;
  }
  pdf += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xrefOffset}\n%%EOF\n`;
  return Buffer.from(pdf, 'latin1');
}

test('encrypted PDF attachment uploads, streams authenticated ranges, and survives restart', { timeout: 240000 }, async (t) => {
  await runIsolatedCase(t, {
    name: 'encrypted pdf attachment range',
    browser,
    build,
    async run(env) {
      const { page, provider } = env;
      const suffix = `${Date.now().toString(36)}_${process.pid.toString(36)}`;
      const marker = `pdf-marker-${suffix}`;
      const filename = `pdf-attachment-${suffix}.pdf`;
      const prompt = `encrypted-pdf prompt ${suffix}`;
      const reply = `encrypted-pdf reply ${suffix}`;
      const pdf = buildTinyPDF(marker);
      assert.equal(pdf.subarray(0, 5).toString('latin1'), '%PDF-', 'fixture must be a PDF');

      await signup(page, username('pdfcrypt'), password);
      const { chatID } = await openChat(page);

      await page.locator('#image-input').setInputFiles({
        name: filename,
        mimeType: 'application/pdf',
        buffer: pdf,
      });
      await page.locator('#image-previews .attachment-pdf-name').filter({ hasText: filename })
        .waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(
        await page.locator('#image-previews .attachment-pdf-name').innerText(),
        filename,
        'composer must show the selected PDF filename',
      );

      await submitPrompt(page, prompt);
      await waitCompletionActive(page);
      const call = await provider.waitForCallCount(1);
      const userMessage = [...call.messages].reverse().find((message) => message.role === 'user');
      assert.ok(userMessage, 'provider request must include the user message');
      assert.ok(Array.isArray(userMessage.content), 'PDF user content must use protocol parts');
      const filePart = userMessage.content.find((part) => part?.type === 'file');
      assert.ok(filePart?.file, 'provider request must include a Chat Completions file part');
      assert.equal(filePart.file.filename, filename, 'provider must receive the decrypted PDF filename');
      assert.equal(
        filePart.file.file_data,
        `data:application/pdf;base64,${pdf.toString('base64')}`,
        'provider must receive the exact inline PDF base64',
      );
      assert.ok(
        userMessage.content.some((part) => part.type === 'text' && part.text === prompt),
        'provider request must include the decrypted message text',
      );

      await provider.release(call.index, finalResponse(reply));
      await waitCompletionDone(page);
      await waitForHistoryText(page, reply);

      const sentCard = page.locator('#message-history .message.user .message-attachment-pdf')
        .filter({ hasText: filename }).first();
      await sentCard.waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(await sentCard.count(), 1, 'sent user message must show one PDF card');
      assert.equal(await sentCard.getAttribute('data-pdf-name'), filename);
      const pdfURL = await sentCard.getAttribute('data-pdf-url');
      assert.match(
        pdfURL,
        new RegExp(`^/chats/${chatID}/messages/[1-9]\\d*/attachments/0$`),
        `PDF card must point at its authenticated attachment endpoint: ${pdfURL}`,
      );

      await reloadChat(page, chatID);
      await waitForHistoryText(page, reply);
      const reloadedCard = page.locator('#message-history .message.user .message-attachment-pdf')
        .filter({ hasText: filename }).first();
      await reloadedCard.waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(await reloadedCard.getAttribute('data-pdf-url'), pdfURL, 'PDF card URL must survive reload');

      await reloadedCard.click();
      const viewer = page.locator('#pdf-viewer');
      await viewer.waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(await viewer.evaluate((element) => element.open), true, 'native PDF dialog must open');
      assert.equal(await page.locator('#pdf-viewer-frame').getAttribute('src'), pdfURL,
        'PDF iframe must point at the authenticated attachment endpoint');
      assert.equal(await page.locator('#pdf-viewer-open').getAttribute('href'), pdfURL,
        'Open in new tab must point at the authenticated attachment endpoint');
      assert.equal((await page.locator('#pdf-viewer-name').innerText()).trim(), filename);
      await page.locator('[data-pdf-viewer-close]').click();
      await viewer.waitFor({ state: 'hidden' });

      const rangeResponse = await env.page.request.get(`${env.baseURL}${pdfURL}`, {
        headers: { Range: 'bytes=0-15' },
      });
      assert.equal(rangeResponse.status(), 206, `range GET returned ${rangeResponse.status()}`);
      const rangeHeaders = rangeResponse.headers();
      assert.equal(
        rangeHeaders['content-range'],
        `bytes 0-15/${pdf.length}`,
        'range response must report the exact PDF byte span',
      );
      assert.equal(rangeHeaders['cache-control'], 'no-store', 'PDF range response must not be cached');
      assert.deepEqual(
        Buffer.from(await rangeResponse.body()),
        pdf.subarray(0, 16),
        'range response must return the exact original slice bytes',
      );

      const fullResponse = await env.page.request.get(`${env.baseURL}${pdfURL}`);
      assert.equal(fullResponse.status(), 200, `full GET returned ${fullResponse.status()}`);
      const fullHeaders = fullResponse.headers();
      assert.equal(fullHeaders['content-type'], 'application/pdf');
      assert.equal(fullHeaders['cache-control'], 'no-store', 'PDF response must not be cached');
      assert.ok(
        (fullHeaders['content-disposition'] ?? '').includes(filename),
        'full PDF response must keep the decrypted filename',
      );
      assert.deepEqual(Buffer.from(await fullResponse.body()), pdf, 'full PDF response must return exact bytes');

      let otherContext;
      try {
        otherContext = await browser.newContext();
        const otherPage = await otherContext.newPage();
        otherPage.setDefaultTimeout(15000);
        await signup(otherPage, username('pdfother'), password);
        const foreignResponse = await otherPage.request.get(`${env.baseURL}${pdfURL}`);
        assert.equal(foreignResponse.status(), 404, 'another account must not read the PDF attachment');
      } finally {
        await otherContext?.close().catch(() => {});
      }

      const appCwd = env.app.cwd;
      await env.app.stop('SIGTERM');
      env.app = null;

      const databaseFiles = await readDatabaseFiles(appCwd);
      assert.ok(databaseFiles.length > 0, `no data.db files found under ${appCwd}`);
      const primaryDatabase = databaseFiles.find((file) => file.name === 'data.db');
      assert.ok(primaryDatabase, `data.db is missing under ${appCwd}`);
      assert.equal(
        primaryDatabase.data.subarray(0, 16).toString('latin1'),
        'SQLite format 3\u0000',
        'data.db must keep a plaintext SQLite header',
      );
      for (const file of databaseFiles) {
        for (const plaintext of [marker, filename]) {
          assert.equal(
            file.data.includes(Buffer.from(plaintext, 'utf8')),
            false,
            `${file.name} must not contain plaintext ${JSON.stringify(plaintext)}`,
          );
        }
      }

      await env.startApp();
      await reloadChat(page, chatID);
      await waitForHistoryText(page, reply);
      const restoredCard = page.locator('#message-history .message.user .message-attachment-pdf')
        .filter({ hasText: filename }).first();
      await restoredCard.waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(await restoredCard.getAttribute('data-pdf-url'), pdfURL,
        'session and PDF card must survive restart');

      const restoredRange = await env.page.request.get(`${env.baseURL}${pdfURL}`, {
        headers: { Range: 'bytes=0-15' },
      });
      assert.equal(restoredRange.status(), 206, `post-restart range GET returned ${restoredRange.status()}`);
      assert.equal(restoredRange.headers()['content-range'], `bytes 0-15/${pdf.length}`);
      assert.deepEqual(
        Buffer.from(await restoredRange.body()),
        pdf.subarray(0, 16),
        'post-restart range response must return the exact original slice bytes',
      );

      await restoredCard.click();
      await page.locator('#pdf-viewer').waitFor({ state: 'visible', timeout: 10000 });
      assert.equal(await page.locator('#pdf-viewer-frame').getAttribute('src'), pdfURL,
        'post-restart PDF dialog must use the same authenticated endpoint');
    },
  });
});
