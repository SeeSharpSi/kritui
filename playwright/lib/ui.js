'use strict';

const assert = require('node:assert/strict');

const { BASE_URL } = require('./app');
const { waitUntil } = require('./wait');

async function signup(page, username, password) {
  await page.goto(`${BASE_URL}/signup`);
  await page.locator('#auth-username').fill(username);
  await page.locator('#auth-password').fill(password);
  const responsePromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'POST' && url.pathname === '/signup';
  });
  await page.locator('.auth-submit').click();
  const response = await responsePromise;
  assert.ok(response.status() >= 200 && response.status() < 400, `signup returned ${response.status()}`);
  await page.locator('#message').waitFor({ state: 'visible' });
  const error = page.locator('.auth-error');
  assert.equal(await error.count(), 0, `signup failed: ${await error.first().textContent().catch(() => '')}`);
}

async function openChat(page) {
  await page.goto(`${BASE_URL}/`);
  await page.locator('#message').waitFor({ state: 'visible' });
  const form = page.locator('#message-form');
  const action = await form.getAttribute('hx-post');
  const match = /\?chat=(\d+)/.exec(action ?? '');
  assert.ok(match, `message form has no chat ID: ${action}`);
  return { chatID: match[1] };
}

async function reloadChat(page, chatID) {
  await page.goto(`${BASE_URL}/?chat=${chatID}`);
  await page.locator('#message').waitFor({ state: 'visible' });
}

async function selectWebfetch(page) {
  const options = page.locator('details.chat-options');
  assert.equal(await options.count(), 1, 'chat options are missing');
  if (!(await options.evaluate((element) => element.open))) {
    await options.locator(':scope > summary').click();
  }
  const picker = page.locator('#tool-picker');
  assert.equal(await picker.count(), 1, 'tool picker is missing');
  await picker.locator(':scope > summary').waitFor({ state: 'visible', timeout: 10000 });
  const open = await picker.evaluate((element) => element.open);
  if (!open) {
    await picker.locator(':scope > summary').click();
  }
  const checkbox = picker.locator('input[name="tool"][value="webfetch"]');
  assert.equal(await checkbox.count(), 1, 'webfetch tool checkbox is missing');
  await checkbox.check({ force: true });
  assert.equal(await checkbox.isChecked(), true);
}

async function submitPrompt(page, text, { delivery = 'steer', expectInbox = false } = {}) {
  const input = page.locator('#message');
  await input.fill(text);
  const button = page.locator(delivery === 'queue' ? '#queue-button' : '#send-button');
  if (delivery === 'queue') {
    await button.waitFor({ state: 'visible' });
  }
  assert.equal(await button.isEnabled(), true, `${delivery} submit button is disabled`);
  const ackPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'POST' && url.pathname === '/messages';
  }, { timeout: 20000 });
  await button.click();
  const response = await ackPromise;
  const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
  assert.ok(
    response.status() >= 200 && response.status() < 300,
    `POST /messages returned ${response.status()}${body ? `: ${body}` : ''}`,
  );
  await page.waitForFunction(() => {
    const form = document.querySelector('#message-form');
    return form !== null && !form.classList.contains('htmx-request');
  }, null, { timeout: 15000 });
  await waitUntil(async () => (await input.inputValue()) === '', {
    timeout: 10000,
    label: 'composer to clear after admission',
  });
  if (expectInbox) {
    await waitForInboxText(page, text);
  }
  return response;
}

async function waitCompletionActive(page) {
  await page.locator('.loading-message.completion-active:not(.completion-failed)').first()
    .waitFor({ state: 'visible', timeout: 15000 });
}

async function waitCompletionDone(page) {
  await waitUntil(async () => {
    return (await page.locator('.loading-message.completion-active').count()) === 0;
  }, {
    timeout: 30000,
    label: 'completion placeholder to finish',
  });
}

async function waitCompletionError(page) {
  await page.locator('.completion-error, .loading-message.completion-failed').first()
    .waitFor({ state: 'visible', timeout: 20000 });
}

function inboxItem(page, text) {
  return page.locator('#prompt-inbox [data-inbox-id]').filter({ hasText: text }).first();
}

async function waitForInboxText(page, text) {
  await waitUntil(async () => {
    const item = inboxItem(page, text);
    return (await item.count()) === 1 && (await item.isVisible());
  }, {
    timeout: 10000,
    label: `pending input ${JSON.stringify(text)}`,
  });
}

async function waitForInboxCount(page, count) {
  await waitUntil(async () => (await page.locator('#prompt-inbox [data-inbox-id]').count()) === count, {
    timeout: 10000,
    label: `prompt inbox to contain ${count} item(s)`,
  });
}

async function inboxTexts(page) {
  return page.locator('#prompt-inbox [data-inbox-id]').evaluateAll((nodes) => nodes.map((node) => {
    const preview = node.querySelector('.prompt-inbox-preview');
    return (preview?.textContent ?? node.textContent ?? '').trim();
  }));
}

async function changeInboxDelivery(page, text, targetDelivery) {
  const item = inboxItem(page, text);
  assert.equal(await item.count(), 1, `pending input ${text} is missing`);
  const button = item.getByRole('button', {
    name: targetDelivery === 'steer' ? /Steer now|Steer this pending input now/i : /^Queue$|Queue this pending input/i,
  });
  assert.equal(await button.count(), 1, `delivery button for ${text} is missing`);
  const ackPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'PATCH' && /^\/chats\/\d+\/inbox\/\d+$/.test(url.pathname);
  }, { timeout: 15000 });
  await button.click();
  const response = await ackPromise;
  assert.ok(response.status() >= 200 && response.status() < 300, `PATCH inbox returned ${response.status()}`);
  await waitUntil(async () => {
    const current = inboxItem(page, text);
    if ((await current.count()) !== 1) {
      return false;
    }
    const nextButton = current.getByRole('button', {
      name: targetDelivery === 'steer' ? /^Queue$|Queue this pending input/i : /Steer now|Steer this pending input now/i,
    });
    return (await nextButton.count()) === 1;
  }, {
    timeout: 10000,
    label: `pending input ${JSON.stringify(text)} to switch to ${targetDelivery}`,
  });
}

async function removeInboxItem(page, text) {
  const item = inboxItem(page, text);
  assert.equal(await item.count(), 1, `pending input ${text} is missing`);
  const button = item.getByRole('button', { name: /Remove this pending input|Remove/i });
  assert.equal(await button.count(), 1, `remove button for ${text} is missing`);
  const ackPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'DELETE' && /^\/chats\/\d+\/inbox\/\d+$/.test(url.pathname);
  }, { timeout: 15000 });
  await button.click();
  const response = await ackPromise;
  assert.ok(response.status() >= 200 && response.status() < 300, `DELETE inbox returned ${response.status()}`);
  await waitUntil(async () => (await inboxItem(page, text).count()) === 0, {
    timeout: 10000,
    label: `pending input ${JSON.stringify(text)} to be removed`,
  });
}

async function clickInboxSteerNow(page, text) {
  const item = inboxItem(page, text);
  assert.equal(await item.count(), 1, `pending input ${text} is missing`);
  const delivery = await item.locator('.prompt-inbox-delivery').textContent();
  assert.equal(delivery?.trim(), 'Queued', `pending input ${text} must start queued`);
  const button = item.getByRole('button', { name: /Steer this pending input now|Steer now/i });
  assert.equal(await button.count(), 1, `Steer now button for ${text} is missing`);
  const ackPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'PATCH' && /^\/chats\/\d+\/inbox\/\d+$/.test(url.pathname);
  }, { timeout: 15000 });
  await button.click();
  const response = await ackPromise;
  const body = response.status() >= 400 ? await response.text().catch(() => '') : '';
  assert.ok(
    response.status() >= 200 && response.status() < 300,
    `PATCH inbox wakeup returned ${response.status()}${body ? `: ${body}` : ''}`,
  );
  return response;
}

async function clickRetry(page) {
  const button = page.locator('.completion-retry-button').first();
  await button.waitFor({ state: 'visible', timeout: 15000 });
  const ackPromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return response.request().method() === 'POST' && url.pathname === '/messages/retry';
  }, { timeout: 15000 });
  await button.click();
  const response = await ackPromise;
  assert.ok(response.status() >= 200 && response.status() < 300, `POST /messages/retry returned ${response.status()}`);
}

async function waitForHistoryText(page, text) {
  await waitUntil(async () => {
    const history = page.locator('#message-history');
    return (await history.count()) === 1 && (await history.innerText()).includes(text);
  }, {
    timeout: 20000,
    label: `persisted transcript to contain ${JSON.stringify(text)}`,
  });
}

async function waitForNoCompletionControl(page) {
  await waitUntil(async () => {
    return (await page.locator('#queue-button').isVisible().catch(() => false)) === false;
  }, {
    timeout: 10000,
    label: 'queue control to hide after completion',
  });
}

async function assertNoStaleCompletionFailure(page, { timeout = 10000, includeRetry = true } = {}) {
  let errors = 0;
  let retries = 0;
  try {
    await waitUntil(async () => {
      errors = await page.locator('.completion-error').count();
      retries = includeRetry ? await page.locator('.completion-retry-button').count() : 0;
      return errors === 0 && retries === 0;
    }, {
      timeout,
      label: 'stale completion error and retry controls to disappear',
    });
  } catch (error) {
    throw new Error(
      `${error.message} Observed ${errors} .completion-error and ${retries} .completion-retry-button element(s).`,
    );
  }
}

module.exports = {
  BASE_URL,
  assertNoStaleCompletionFailure,
  changeInboxDelivery,
  clickInboxSteerNow,
  clickRetry,
  inboxItem,
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
};
