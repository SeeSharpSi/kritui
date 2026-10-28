'use strict';

const assert = require('node:assert/strict');
const { after, before, test } = require('node:test');
const { chromium } = require('playwright');

const { buildApp, cleanupBuild } = require('./lib/build');
const { errorResponse, finalResponse, toolCallResponse } = require('./lib/fake-provider');
const { runIsolatedCase } = require('./lib/isolated-case');
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
