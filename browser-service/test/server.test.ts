import test from 'node:test';
import assert from 'node:assert/strict';
import { startServer } from '../src/server';
import { FakeBrowserInstance } from '../src/browser';
import { BrowserBusyError } from '../src/pool';

const TOKEN = 'test-internal-secret-9f3a2b';

interface Stack {
  port: number;
  store: any;
  close: () => Promise<void>;
}

async function stack(poolSize = 4, acquireTimeoutMs = 250): Promise<Stack> {
  const made: FakeBrowserInstance[] = [];
  const { server, store, pool, port } = await startServer({
    port: 0,
    poolSize,
    acquireTimeoutMs,
    internalToken: TOKEN,
    instanceFactory: {
      create: async (id) => {
        const f = new FakeBrowserInstance(id);
        made.push(f);
        await f.launch();
        return f;
      },
    },
  });
  return {
    port,
    store,
    close: async () => {
      store.stopSweeper();
      server.closeAllConnections?.();
      await new Promise<void>((r) => server.close(() => r()));
      await pool.drain();
    },
  };
}

async function api(
  port: number,
  method: string,
  path: string,
  opts: { token?: string; body?: unknown } = {},
) {
  const res = await fetch(`http://127.0.0.1:${port}${path}`, {
    method,
    headers: {
      ...(opts.token !== undefined ? { authorization: `Bearer ${opts.token}` } : {}),
      'content-type': 'application/json',
    },
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  });
  const text = await res.text();
  let json: any = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    /* non-JSON */
  }
  return { status: res.status, json, text };
}

let RPC_ID = 1;
async function mcp(port: number, method: string, params?: unknown) {
  return api(port, 'POST', '/mcp', { body: { jsonrpc: '2.0', id: RPC_ID++, method, params } });
}

async function createSession(
  port: number,
  policy: Record<string, unknown> = {},
  binding = { agent_id: 'a1', task_id: 't1', resource_id: 'r1' },
) {
  const r = await api(port, 'POST', '/v1/sessions', { token: TOKEN, body: { ...binding, policy } });
  assert.equal(r.status, 201, JSON.stringify(r.json));
  return r.json as { session_id: string; token: string; expires_at: string };
}

async function toolCall(port: number, name: string, args: Record<string, unknown>) {
  return mcp(port, 'tools/call', { name, arguments: args });
}

// ---------------- control API auth ----------------

test('control API: missing token → 401', async () => {
  const s = await stack(1);
  try {
    for (const [m, p] of [
      ['POST', '/v1/sessions'],
      ['GET', '/v1/sessions'],
      ['DELETE', '/v1/sessions/bs_x'],
      ['POST', '/v1/sweeper/pass'],
    ] as const) {
      const r = await api(s.port, m, p, { body: m === 'POST' ? {} : undefined });
      assert.equal(r.status, 401, `${m} ${p} → ${r.status}`);
    }
  } finally {
    await s.close();
  }
});

test('control API: wrong token → 401', async () => {
  const s = await stack(1);
  try {
    const r = await api(s.port, 'GET', '/v1/sessions', { token: 'nope' });
    assert.equal(r.status, 401);
  } finally {
    await s.close();
  }
});

test('session create: 201 with bs_<32hex>, 48hex token, RFC3339 expiry', async () => {
  const s = await stack(1);
  try {
    const j = await createSession(s.port, { max_session_minutes: 30 });
    assert.match(j.session_id, /^bs_[0-9a-f]{32}$/);
    assert.match(j.token, /^[0-9a-f]{48}$/);
    assert.ok(!Number.isNaN(Date.parse(j.expires_at)), 'expires_at parses as date');
  } finally {
    await s.close();
  }
});

test('session create: missing binding → 400', async () => {
  const s = await stack(1);
  try {
    const r = await api(s.port, 'POST', '/v1/sessions', {
      token: TOKEN,
      body: { agent_id: 'a1' },
    });
    assert.equal(r.status, 400);
  } finally {
    await s.close();
  }
});

test('GET /v1/sessions lists spec fields', async () => {
  const s = await stack(2);
  try {
    const j = await createSession(s.port, {}, { agent_id: 'agentX', task_id: 'taskY', resource_id: 'resZ' });
    const r = await api(s.port, 'GET', '/v1/sessions', { token: TOKEN });
    assert.equal(r.status, 200);
    assert.equal(r.json.sessions.length, 1);
    const row = r.json.sessions[0];
    assert.deepEqual(
      Object.keys(row).sort(),
      ['agent_id', 'expires_at', 'id', 'idle_seconds', 'pages_used', 'resource_id', 'task_id'],
    );
    assert.equal(row.id, j.session_id);
    assert.equal(row.agent_id, 'agentX');
  } finally {
    await s.close();
  }
});

test('DELETE /v1/sessions/{id}: 204 always, idempotent', async () => {
  const s = await stack(1);
  try {
    const j = await createSession(s.port);
    let r = await api(s.port, 'DELETE', `/v1/sessions/${j.session_id}`, { token: TOKEN });
    assert.equal(r.status, 204);
    r = await api(s.port, 'DELETE', `/v1/sessions/${j.session_id}`, { token: TOKEN });
    assert.equal(r.status, 204, 'idempotent');
    r = await api(s.port, 'DELETE', '/v1/sessions/bs_doesnotexist', { token: TOKEN });
    assert.equal(r.status, 204);
    const list = await api(s.port, 'GET', '/v1/sessions', { token: TOKEN });
    assert.equal(list.json.sessions.length, 0);
  } finally {
    await s.close();
  }
});

test('POST /v1/sweeper/pass: 200 {reaped}', async () => {
  const s = await stack(1);
  try {
    const j = await createSession(s.port, { idle_timeout_s: 1 });
    const sess = s.store.getByToken(j.token);
    sess.lastActivityAt = Date.now() - 2_000;
    const r = await api(s.port, 'POST', '/v1/sweeper/pass', { token: TOKEN });
    assert.equal(r.status, 200);
    assert.equal(r.json.reaped, 1);
  } finally {
    await s.close();
  }
});

// ---------------- MCP surface ----------------

test('MCP initialize: pinned shape', async () => {
  const s = await stack(1);
  try {
    const r = await mcp(s.port, 'initialize', { protocolVersion: '2025-03-26', capabilities: {} });
    assert.equal(r.status, 200);
    assert.equal(r.json.result.protocolVersion, '2025-03-26');
    assert.deepEqual(r.json.result.capabilities, { tools: {} });
    assert.equal(r.json.result.serverInfo.name, 'skquad-browser-service');
    assert.equal(typeof r.json.result.serverInfo.version, 'string');
  } finally {
    await s.close();
  }
});

test('MCP tools/list: six tools, exact names, session required in every schema', async () => {
  const s = await stack(1);
  try {
    const r = await mcp(s.port, 'tools/list', {});
    assert.equal(r.status, 200);
    const tools = r.json.result.tools;
    assert.deepEqual(
      tools.map((t: any) => t.name).sort(),
      [
        'browser.click',
        'browser.close_session',
        'browser.extract',
        'browser.navigate',
        'browser.screenshot',
        'browser.type',
      ],
    );
    for (const t of tools) {
      assert.equal(t.inputSchema.type, 'object');
      assert.ok(t.inputSchema.properties.session, `${t.name} missing session prop`);
      assert.ok(t.inputSchema.required.includes('session'), `${t.name} session not required`);
    }
  } finally {
    await s.close();
  }
});

test('MCP unknown method → -32601', async () => {
  const s = await stack(1);
  try {
    const r = await mcp(s.port, 'bogus/method', {});
    assert.equal(r.json.error.code, -32601);
  } finally {
    await s.close();
  }
});

test('MCP tools/call: missing session token → -32001 (before any browser work)', async () => {
  const s = await stack(1);
  try {
    const r = await toolCall(s.port, 'browser.navigate', { url: 'http://93.184.216.34/' });
    assert.equal(r.json.error.code, -32001);
    assert.equal(r.json.error.message, 'session_invalid');
    // no browser instance was ever created for an invalid session
    assert.equal(s.store.list().length, 0);
  } finally {
    await s.close();
  }
});

test('MCP tools/call: wrong/unknown token → -32001', async () => {
  const s = await stack(1);
  try {
    await createSession(s.port);
    const r = await toolCall(s.port, 'browser.navigate', {
      session: 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef',
      url: 'http://93.184.216.34/',
    });
    assert.equal(r.json.error.code, -32001);
  } finally {
    await s.close();
  }
});

test('MCP navigate happy path: {url,title,page_index}', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    const r = await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/' });
    assert.equal(r.json.error, undefined, JSON.stringify(r.json));
    assert.equal(r.json.result.isError, false);
    const out = JSON.parse(r.json.result.content[0].text);
    assert.equal(out.url, 'http://93.184.216.34/');
    assert.equal(typeof out.title, 'string');
    assert.equal(out.page_index, 1);
  } finally {
    await s.close();
  }
});

test('ceiling: deny_hosts blocks navigate with -32002', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port, { deny_hosts: ['*.internal', 'metadata.google.internal'] });
    const r = await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://db.internal/admin' });
    assert.equal(r.json.error.code, -32002);
    assert.match(r.json.error.message, /ceiling_exceeded/);
  } finally {
    await s.close();
  }
});

test('ceiling: deny_hosts blocks SUBRESOURCE via route callback', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port, { deny_hosts: ['*.internal'] });
    const sess = s.store.getByToken(token);
    const ctx = sess.context as any;
    // Simulated JS fetch/XHR/subresource hitting the Context.route('**/*') handler
    const blocked = await ctx.fireRoute({ url: 'http://evil.internal/x.js', resourceType: 'script' });
    assert.equal(blocked, 'aborted', 'deny-listed subresource must be aborted');
    const xhr = await ctx.fireRoute({ url: 'https://steal.internal/creds', resourceType: 'xhr' });
    assert.equal(xhr, 'aborted');
    const ws = await ctx.fireRoute({ url: 'wss://x.internal/ws', resourceType: 'websocket' }).catch(() => 'aborted');
    // wss scheme is not http(s) → must also be denied by the route handler
    assert.equal(ws, 'aborted');
    const okRes = await ctx.fireRoute({ url: 'http://93.184.216.34/app.js', resourceType: 'script' });
    assert.equal(okRes, 'continued', 'public subresource allowed');
  } finally {
    await s.close();
  }
});

test('ceiling: private/loopback/CGNAT host denial via navigate', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    for (const url of [
      'http://127.0.0.1:8090/admin',
      'http://169.254.169.254/latest/meta-data/',
      'http://100.64.0.1/',
      'http://192.168.68.131/',
    ]) {
      const r = await toolCall(s.port, 'browser.navigate', { session: token, url });
      assert.equal(r.json.error.code, -32002, `${url} must be denied`);
    }
  } finally {
    await s.close();
  }
});

test('ceiling: max_pages enforced', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port, { max_pages: 2 });
    let r = await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/1' });
    assert.equal(r.json.result.isError, false);
    r = await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/2' });
    assert.equal(r.json.result.isError, false);
    r = await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/3' });
    assert.equal(r.json.error.code, -32002, 'third page exceeds max_pages=2');
    assert.match(r.json.error.message, /max_pages_exceeded/);
  } finally {
    await s.close();
  }
});

test('screenshot cap: full-page over cap clips to viewport', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port, { max_screenshot_bytes: 3000 });
    await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/' });
    // fake: viewport=1000 bytes, full-page=4000 bytes
    const sess = s.store.getByToken(token);
    (sess.page as any).screenshotSize = 1000;
    const r = await toolCall(s.port, 'browser.screenshot', { session: token, full_page: true });
    assert.equal(r.json.error, undefined);
    const out = JSON.parse(r.json.result.content[0].text);
    assert.equal(out.bytes, 1000, 'clipped to viewport');
    assert.equal(out.mime, 'image/png');
    assert.ok(Buffer.from(out.screenshot_b64, 'base64').length === 1000);
  } finally {
    await s.close();
  }
});

test('screenshot cap: viewport-only still over cap → -32002', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port, { max_screenshot_bytes: 500 });
    await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/' });
    const r = await toolCall(s.port, 'browser.screenshot', { session: token });
    assert.equal(r.json.error.code, -32002);
    assert.match(r.json.error.message, /screenshot/);
  } finally {
    await s.close();
  }
});

test('extract: capped at 200 KB', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/' });
    const sess = s.store.getByToken(token);
    (sess.page as any).text = 'x'.repeat(250_000);
    const r = await toolCall(s.port, 'browser.extract', { session: token });
    const out = JSON.parse(r.json.result.content[0].text);
    assert.ok(Buffer.byteLength(out.text, 'utf8') <= 200_000);
    assert.equal(Buffer.byteLength(out.text, 'utf8'), 200_000);
  } finally {
    await s.close();
  }
});

test('click + type tools work after navigate', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    await toolCall(s.port, 'browser.navigate', { session: token, url: 'http://93.184.216.34/' });
    let r = await toolCall(s.port, 'browser.click', { session: token, selector: '#btn' });
    assert.equal(r.json.result.isError, false);
    assert.deepEqual(JSON.parse(r.json.result.content[0].text), { clicked: '#btn' });
    r = await toolCall(s.port, 'browser.type', { session: token, selector: '#q', text: 'hello', submit: true });
    assert.equal(r.json.result.isError, false);
    assert.deepEqual(JSON.parse(r.json.result.content[0].text), { typed: '#q' });
  } finally {
    await s.close();
  }
});

test('click before navigate → tool-level isError (not protocol error)', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    const r = await toolCall(s.port, 'browser.click', { session: token, selector: '#x' });
    assert.equal(r.json.error, undefined);
    assert.equal(r.json.result.isError, true);
  } finally {
    await s.close();
  }
});

test('browser.close_session: closes, then token is invalid (-32001)', async () => {
  const s = await stack(1);
  try {
    const { session_id, token } = await createSession(s.port);
    const r = await toolCall(s.port, 'browser.close_session', { session: token });
    assert.equal(r.json.result.isError, false);
    assert.deepEqual(JSON.parse(r.json.result.content[0].text), { closed: session_id });
    const r2 = await toolCall(s.port, 'browser.extract', { session: token });
    assert.equal(r2.json.error.code, -32001);
  } finally {
    await s.close();
  }
});

test('pool exhaustion: POST /v1/sessions → 503 browser_busy', async () => {
  const s = await stack(1, 120);
  try {
    await createSession(s.port); // takes the only instance
    const r = await api(s.port, 'POST', '/v1/sessions', {
      token: TOKEN,
      body: { agent_id: 'a2', task_id: 't2', resource_id: 'r2' },
    });
    assert.equal(r.status, 503);
    assert.deepEqual(r.json, { error: 'browser_busy' });
  } finally {
    await s.close();
  }
});

test('MCP -32003 mapping: BrowserBusyError surfaces as browser_busy', async () => {
  const s = await stack(1);
  try {
    const { token } = await createSession(s.port);
    const origTouch = s.store.touch.bind(s.store);
    (s.store as any).touch = () => {
      throw new BrowserBusyError();
    };
    try {
      const r = await toolCall(s.port, 'browser.extract', { session: token });
      assert.equal(r.json.error.code, -32003);
      assert.equal(r.json.error.message, 'browser_busy');
    } finally {
      (s.store as any).touch = origTouch;
    }
  } finally {
    await s.close();
  }
});

test('session isolation: two sessions get different instances', async () => {
  const s = await stack(2);
  try {
    const a = await createSession(s.port, {}, { agent_id: 'a', task_id: 'ta', resource_id: 'ra' });
    const b = await createSession(s.port, {}, { agent_id: 'b', task_id: 'tb', resource_id: 'rb' });
    const sa = s.store.getByToken(a.token);
    const sb = s.store.getByToken(b.token);
    assert.notEqual(sa.instance, sb.instance, 'two sessions never share a Chromium instance');
  } finally {
    await s.close();
  }
});
