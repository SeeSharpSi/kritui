'use strict';

const fs = require('node:fs/promises');
const path = require('node:path');

const { TestEnvironment } = require('./app');
const { FakeProvider } = require('./fake-provider');

const resultsRoot = path.resolve(__dirname, '..', 'test-results');

let queue = Promise.resolve();

function serialized(task) {
  const next = queue.then(task, task);
  queue = next.catch(() => {});
  return next;
}

function slugify(value) {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 80) || 'case';
}

function timestamp() {
  return new Date().toISOString().replace(/[:.]/g, '-');
}

function sanitizeCall(call) {
  return {
    index: call.index,
    path: call.path,
    model: call.model,
    status: call.status,
    aborted: call.aborted,
    messages: call.messages.map((message) => ({
      role: message.role,
      content: message.content,
      tool_calls: message.tool_calls,
      tool_call_id: message.tool_call_id,
    })),
    tools: call.tools.map((tool) => tool?.function?.name ?? tool?.name ?? 'unknown'),
  };
}

async function captureFailure(env, name, error) {
  const directory = path.join(resultsRoot, `${timestamp()}-${slugify(name)}`);
  await fs.mkdir(directory, { recursive: true });
  if (env.page && !env.page.isClosed()) {
    await env.page.screenshot({ path: path.join(directory, 'failure.png'), fullPage: true }).catch(() => {});
    const html = await env.page.content().catch(() => '');
    if (html) {
      await fs.writeFile(path.join(directory, 'page.html'), html);
    }
  }
  const details = {
    case: name,
    error: {
      name: error?.name,
      message: error?.message,
      stack: error?.stack,
    },
    appLogs: env.appLogs(),
    providerRequests: env.provider.requests,
    providerCalls: env.provider.calls.map(sanitizeCall),
  };
  await fs.writeFile(path.join(directory, 'failure.json'), JSON.stringify(details, null, 2));
  return directory;
}

async function saveScreenshot(env, label) {
  const directory = path.join(resultsRoot, 'screenshots');
  await fs.mkdir(directory, { recursive: true });
  const file = path.join(directory, `${timestamp()}-${slugify(label)}.png`);
  await env.page.screenshot({ path: file, fullPage: true });
  return file;
}

async function runIsolatedCase(t, { name, browser, build, run }) {
  return serialized(async () => {
    const provider = new FakeProvider();
    let appEnvironment;
    let context;
    let page;
    let traceStarted = false;
    let failure;
    const runDirName = `${slugify(name)}-${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;
    const runDir = path.join(build.base, 'runs', runDirName);

    try {
      await provider.start();
      appEnvironment = new TestEnvironment({ build, provider, runDir });
      await appEnvironment.startApp();
      context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
      await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
      traceStarted = true;
      page = await context.newPage();
      page.setDefaultTimeout(15000);
      const env = Object.assign(appEnvironment, {
        provider,
        context,
        page,
        screenshot: (label) => saveScreenshot({ page }, label),
      });
      await run(env);
    } catch (error) {
      failure = error;
      if (appEnvironment) {
        const directory = await captureFailure({
          page,
          provider,
          appLogs: () => appEnvironment.appLogs(),
        }, name, error).catch(() => null);
        if (directory) {
          t.diagnostic(`Failure artifacts: ${directory}`);
          console.error(`Playwright failure artifacts: ${directory}`);
        }
      }
      throw error;
    } finally {
      if (context) {
        if (failure) {
          const tracePath = path.join(
            resultsRoot,
            `${timestamp()}-${slugify(name)}-trace.zip`,
          );
          await fs.mkdir(resultsRoot, { recursive: true });
          await context.tracing.stop({ path: tracePath }).catch(() => {});
          t.diagnostic(`Playwright trace: ${tracePath}`);
          console.error(`Playwright trace: ${tracePath}`);
        } else if (traceStarted) {
          await context.tracing.stop().catch(() => {});
        }
        await context.close().catch(() => {});
      }
      if (appEnvironment?.app) {
        await appEnvironment.app.stop('SIGTERM').catch(() => {});
        appEnvironment.app = null;
      }
      await provider.close().catch(() => {});
      await fs.rm(runDir, { recursive: true, force: true, maxRetries: 3 }).catch(() => {});
    }
  });
}

module.exports = { resultsRoot, runIsolatedCase, slugify };
