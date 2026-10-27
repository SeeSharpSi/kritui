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
