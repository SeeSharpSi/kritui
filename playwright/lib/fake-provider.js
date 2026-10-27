'use strict';

const http = require('node:http');
const { once } = require('node:events');

const { sleep, waitUntil } = require('./wait');

const completionPaths = new Set([
  '/v1/chat/completions',
  '/v1/chat/messages',
  '/v1/messages',
  '/v1/responses',
]);

function finalResponse(content, { model = 'fake-model', tokens = 12 } = {}) {
  return {
    status: 200,
    body: {
      model,
      choices: [
        {
          message: { role: 'assistant', content },
          finish_reason: 'stop',
        },
      ],
      usage: { total_tokens: tokens },
    },
  };
}

function toolCallResponse({ id = 'call_fixture_1', name = 'webfetch', argumentsValue, tokens = 10 } = {}) {
  const rawArguments = typeof argumentsValue === 'string'
    ? argumentsValue
    : JSON.stringify(argumentsValue ?? {});
  return {
    status: 200,
    body: {
      model: 'fake-model',
      choices: [
        {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [
              {
                id,
                type: 'function',
                function: { name, arguments: rawArguments },
              },
            ],
          },
          finish_reason: 'tool_calls',
        },
      ],
      usage: { total_tokens: tokens },
    },
  };
}

function errorResponse(status = 503, message = 'fixture provider failure') {
  return {
    status,
    body: { error: { message, type: 'fixture_error' } },
  };
}

class FakeProvider {
  constructor({ article = 'Fixture article: deterministic test content.' } = {}) {
    this.article = article;
    this.calls = [];
    this.requests = [];
    this.server = null;
    this.port = null;
  }

  async start() {
    if (this.server) {
      return this;
    }
    this.server = http.createServer((request, response) => {
      this._handle(request, response).catch((error) => {
        if (!response.headersSent) {
          response.writeHead(500, { 'Content-Type': 'application/json' });
        }
        if (!response.writableEnded) {
          response.end(JSON.stringify({ error: { message: error.message } }));
        }
      });
    });
    this.server.on('clientError', (_error, socket) => socket.destroy());
    this.server.listen(0, '127.0.0.1');
    await once(this.server, 'listening');
    this.port = this.server.address().port;
    return this;
  }

  get origin() {
    if (!this.port) {
      throw new Error('fake provider is not started');
    }
    return `http://127.0.0.1:${this.port}`;
  }

  get chatURL() {
    return `${this.origin}/v1/chat/completions`;
  }

  get modelsURL() {
    return `${this.origin}/v1/models`;
  }

  get fixtureURL() {
    return `${this.origin}/fixture/article`;
  }

  async _handle(request, response) {
    const url = new URL(request.url, this.origin);
    this.requests.push({ method: request.method, path: url.pathname, at: Date.now() });

    if (request.method === 'GET' && url.pathname === '/v1/models') {
      response.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      response.end(JSON.stringify({
        data: [
          {
            id: 'fake-model',
            architecture: { input_modalities: ['text', 'image'] },
          },
        ],
      }));
      return;
    }

    if (request.method === 'GET' && url.pathname === '/fixture/article') {
      response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
      response.end(`<!doctype html><html><body><article><h1>Fixture article</h1><p>${this.article}</p></article></body></html>`);
      return;
    }

    if (request.method === 'POST' && completionPaths.has(url.pathname)) {
      const body = await readJSONBody(request);
      const call = {
        index: this.calls.length,
        path: url.pathname,
        method: request.method,
        headers: { ...request.headers },
        body,
        messages: Array.isArray(body.messages) ? body.messages : [],
        tools: Array.isArray(body.tools) ? body.tools : [],
        model: body.model,
        receivedAt: Date.now(),
        released: false,
        done: false,
        aborted: false,
        status: null,
      };
      call.responsePromise = new Promise((resolve) => {
        call.resolveResponse = resolve;
      });
      this.calls.push(call);

      response.on('close', () => {
        if (!response.writableEnded && !call.released) {
          call.aborted = true;
          call.resolveResponse({ aborted: true });
        }
      });

      const spec = await call.responsePromise;
      if (call.aborted || response.destroyed) {
        call.done = true;
        return;
      }
      call.status = spec.status ?? 200;
      writeResponse(response, spec);
      call.done = true;
      return;
    }

    response.writeHead(404, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify({ error: { message: `unknown fixture route ${request.method} ${url.pathname}` } }));
  }

  async waitForCallCount(count, { timeout = 15000 } = {}) {
    await waitUntil(() => this.calls.length >= count, {
      timeout,
      label: `fake provider to receive ${count} completion call(s), saw ${this.calls.length}`,
    });
    return this.calls[count - 1];
  }

  async release(index, spec) {
    const call = this.calls[index];
    if (!call) {
      throw new Error(`fake provider has no call ${index}; received ${this.calls.length}`);
    }
    if (call.aborted) {
      throw new Error(`fake provider call ${index} was already aborted`);
    }
    if (call.released) {
      throw new Error(`fake provider call ${index} was already released`);
    }
    call.released = true;
    call.resolveResponse(spec);
    await waitUntil(() => call.done, {
      timeout: 5000,
      label: `fake provider call ${index} to finish writing its response`,
    });
    return call;
  }

  async close() {
    const server = this.server;
    this.server = null;
    if (!server) {
      return;
    }
    for (const call of this.calls) {
      if (!call.released) {
        call.released = true;
        call.aborted = true;
        call.resolveResponse({ aborted: true });
      }
    }
    const closed = new Promise((resolve) => server.close(resolve));
    if (typeof server.closeAllConnections === 'function') {
      server.closeAllConnections();
    }
    await Promise.race([closed, sleep(2000)]);
  }
}

function readJSONBody(request) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    request.on('data', (chunk) => {
      size += chunk.length;
      if (size > 32 * 1024 * 1024) {
        reject(new Error('fixture provider request body exceeds 32 MiB'));
        request.destroy();
        return;
      }
      chunks.push(chunk);
    });
    request.on('error', reject);
    request.on('end', () => {
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}'));
      } catch (error) {
        reject(error);
      }
    });
  });
}

function writeResponse(response, spec) {
  response.writeHead(spec.status ?? 200, {
    'Content-Type': 'application/json; charset=utf-8',
    'Cache-Control': 'no-store',
  });
  response.end(JSON.stringify(spec.body ?? {}));
}

module.exports = {
  FakeProvider,
  completionPaths,
  errorResponse,
  finalResponse,
  toolCallResponse,
};
