/**
 * HTTP server (TG-6 §1–§4): node:http on :8090.
 *
 * - Control API under /v1/* — internal-token auth (BROWSER_INTERNAL_TOKEN);
 *   missing/wrong → 401.
 * - MCP JSON-RPC 2.0 at POST /mcp — authenticated per-call by the session
 *   token injected in every tool `arguments.session`. Validate the session
 *   BEFORE any browser work.
 *
 * JSON-RPC error codes (pinned): -32001 session_invalid,
 * -32002 ceiling_exceeded, -32003 browser_busy.
 */

import http from 'node:http';
import crypto from 'node:crypto';
import { AddressInfo } from 'node:net';
import { BrowserInstance } from './browser';
import { BrowserPool, BrowserBusyError } from './pool';
import { Session, SessionStore } from './sessions';
import { checkRequest } from './enforce';

export const SERVER_NAME = 'skquad-browser-service';
export const SERVER_VERSION = '0.1.0';
export const MCP_PROTOCOL_VERSION = '2025-03-26';
export const DEFAULT_PORT = 8090;
export const EXTRACT_MAX_BYTES = 200_000; // §3: extract ≤ 200 KB

// ---------- error types ----------

export class HttpError extends Error {
  constructor(
    public readonly status: number,
    public readonly body: unknown,
  ) {
    super(`http ${status}`);
    this.name = 'HttpError';
  }
}

export class McpError extends Error {
  constructor(
    public readonly code: number,
    message: string,
  ) {
    super(message);
    this.name = 'McpError';
  }
}

// ---------- helpers ----------

function timingSafeEqualStr(a: string, b: string): boolean {
  const ab = Buffer.from(a, 'utf8');
  const bb = Buffer.from(b, 'utf8');
  if (ab.length !== bb.length) return false;
  return crypto.timingSafeEqual(ab, bb);
}

function sendJson(res: http.ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(payload);
}

async function readJsonBody(req: http.IncomingMessage): Promise<any> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    total += (chunk as Buffer).length;
    if (total > 1_048_576) throw new HttpError(413, { error: 'body_too_large' });
    chunks.push(chunk as Buffer);
  }
  if (chunks.length === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    throw new HttpError(400, { error: 'invalid_json' });
  }
}

function requireInternal(req: http.IncomingMessage, internalToken: string): void {
  const hdr = req.headers['authorization'];
  if (typeof hdr !== 'string' || !hdr.startsWith('Bearer ')) {
    throw new HttpError(401, { error: 'unauthorized' });
  }
  const got = hdr.slice('Bearer '.length).trim();
  if (!timingSafeEqualStr(got, internalToken)) {
    throw new HttpError(401, { error: 'unauthorized' });
  }
}

// ---------- MCP tool catalog (§3) ----------

const SESSION_PROP = { type: 'string', description: 'Session token injected by the gateway driver.' };

export const TOOLS = [
  {
    name: 'browser.navigate',
    description: 'Navigate the session page to an http(s) URL (subject to the navigation ceiling).',
    inputSchema: {
      type: 'object',
      properties: { session: SESSION_PROP, url: { type: 'string', description: 'http/https URL' } },
      required: ['session', 'url'],
    },
  },
  {
    name: 'browser.click',
    description: 'Click a CSS selector on the current page.',
    inputSchema: {
      type: 'object',
      properties: { session: SESSION_PROP, selector: { type: 'string', description: 'CSS selector' } },
      required: ['session', 'selector'],
    },
  },
  {
    name: 'browser.type',
    description: 'Type text into a CSS selector on the current page.',
    inputSchema: {
      type: 'object',
      properties: {
        session: SESSION_PROP,
        selector: { type: 'string', description: 'CSS selector' },
        text: { type: 'string', description: 'Text to type' },
        submit: { type: 'boolean', description: 'Press Enter after typing' },
      },
      required: ['session', 'selector', 'text'],
    },
  },
  {
    name: 'browser.screenshot',
    description: 'PNG screenshot of the page (capped by max_screenshot_bytes).',
    inputSchema: {
      type: 'object',
      properties: {
        session: SESSION_PROP,
        full_page: { type: 'boolean', description: 'Full-page capture' },
      },
      required: ['session'],
    },
  },
  {
    name: 'browser.extract',
    description: 'Visible body text of the current page (≤ 200 KB, untrusted).',
    inputSchema: {
      type: 'object',
      properties: { session: SESSION_PROP },
      required: ['session'],
    },
  },
  {
    name: 'browser.close_session',
    description: 'Close the session and destroy its browser context.',
    inputSchema: {
      type: 'object',
      properties: { session: SESSION_PROP },
      required: ['session'],
    },
  },
] as const;

// ---------- tool execution ----------

async function ensurePage(session: Session) {
  if (!session.page) {
    session.page = await session.context.newPage();
  }
  return session.page;
}

/**
 * Execute one MCP tool call. `session` token MUST be validated before any
 * browser work. Returns the CallToolResult `text` (JSON string).
 */
export async function executeTool(store: SessionStore, name: string, args: any): Promise<string> {
  // ---- session validation FIRST (before touching any browser) ----
  const token = typeof args?.session === 'string' ? args.session : '';
  const session = token ? store.getByToken(token) : undefined;
  if (!session) throw new McpError(-32001, 'session_invalid');
  store.touch(session);

  switch (name) {
    case 'browser.navigate': {
      const url = String(args?.url ?? '');
      // Fail-fast pre-check (defense-in-depth: route handler also enforces).
      const decision = await checkRequest(url, session.policy, {
        topLevel: true,
        pagesUsed: session.pagesUsed,
      });
      if (!decision.allowed) throw new McpError(-32002, `ceiling_exceeded:${decision.reason}`);
      const page = await ensurePage(session);
      let title = '';
      try {
        const nav = await page.goto(url);
        title = nav?.title ?? '';
      } catch (err: any) {
        if (err?.aborted || /aborted|ERR_FAILED|ERR_BLOCKED|net::/i.test(String(err?.message ?? err))) {
          throw new McpError(-32002, 'ceiling_exceeded:navigation_aborted');
        }
        throw err;
      }
      return JSON.stringify({ url, title, page_index: session.pagesUsed });
    }
    case 'browser.click': {
      const selector = String(args?.selector ?? '');
      if (!selector) throw new McpError(-32602, 'selector_required');
      const page = session.page;
      if (!page) throw toolError('no page in session; call browser.navigate first');
      await page.click(selector);
      return JSON.stringify({ clicked: selector });
    }
    case 'browser.type': {
      const selector = String(args?.selector ?? '');
      const text = String(args?.text ?? '');
      const submit = !!args?.submit;
      if (!selector) throw new McpError(-32602, 'selector_required');
      const page = session.page;
      if (!page) throw toolError('no page in session; call browser.navigate first');
      await page.fill(selector, text, submit);
      return JSON.stringify({ typed: selector });
    }
    case 'browser.screenshot': {
      const page = session.page;
      if (!page) throw toolError('no page in session; call browser.navigate first');
      const cap = session.policy.max_screenshot_bytes;
      let buf = await page.screenshot({ fullPage: !!args?.full_page });
      // §4: over cap → clip to viewport (non-full-page); still over → ceiling.
      if (buf.length > cap && args?.full_page) {
        buf = await page.screenshot({ fullPage: false });
      }
      if (buf.length > cap) {
        throw new McpError(-32002, `ceiling_exceeded:screenshot_${buf.length}_gt_${cap}`);
      }
      return JSON.stringify({
        screenshot_b64: buf.toString('base64'),
        bytes: buf.length,
        mime: 'image/png',
      });
    }
    case 'browser.extract': {
      const page = session.page;
      if (!page) throw toolError('no page in session; call browser.navigate first');
      let text = await page.visibleText();
      if (Buffer.byteLength(text, 'utf8') > EXTRACT_MAX_BYTES) {
        text = Buffer.from(text, 'utf8').subarray(0, EXTRACT_MAX_BYTES).toString('utf8');
      }
      return JSON.stringify({ text });
    }
    case 'browser.close_session': {
      const id = session.id;
      await store.closeSession(session);
      return JSON.stringify({ closed: id });
    }
    default:
      throw new McpError(-32602, `unknown_tool:${name}`);
  }
}

/** A tool-level failure (isError:true), not a JSON-RPC protocol error. */
class ToolError extends Error {}
function toolError(msg: string): ToolError {
  return new ToolError(msg);
}

// ---------- MCP dispatch ----------

export async function dispatchMcp(store: SessionStore, msg: any): Promise<{ response: any; status: number }> {
  const id = msg?.id ?? null;

  if (msg?.jsonrpc !== '2.0' || typeof msg?.method !== 'string') {
    return {
      response: { jsonrpc: '2.0', id, error: { code: -32600, message: 'invalid_request' } },
      status: 200,
    };
  }

  // Notifications: acknowledged with 202, no body.
  if (id === null && msg.method.startsWith('notifications/')) {
    return { response: null, status: 202 };
  }

  try {
    switch (msg.method) {
      case 'initialize': {
        return {
          response: {
            jsonrpc: '2.0',
            id,
            result: {
              protocolVersion: MCP_PROTOCOL_VERSION,
              capabilities: { tools: {} },
              serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
            },
          },
          status: 200,
        };
      }
      case 'tools/list': {
        return { response: { jsonrpc: '2.0', id, result: { tools: TOOLS } }, status: 200 };
      }
      case 'tools/call': {
        const name = String(msg?.params?.name ?? '');
        const args = msg?.params?.arguments ?? {};
        try {
          const text = await executeTool(store, name, args);
          return {
            response: {
              jsonrpc: '2.0',
              id,
              result: { content: [{ type: 'text', text }], isError: false },
            },
            status: 200,
          };
        } catch (err) {
          if (err instanceof McpError) {
            return {
              response: { jsonrpc: '2.0', id, error: { code: err.code, message: err.message } },
              status: 200,
            };
          }
          if (err instanceof BrowserBusyError) {
            return {
              response: { jsonrpc: '2.0', id, error: { code: -32003, message: 'browser_busy' } },
              status: 200,
            };
          }
          if (err instanceof ToolError) {
            return {
              response: {
                jsonrpc: '2.0',
                id,
                result: { content: [{ type: 'text', text: err.message }], isError: true },
              },
              status: 200,
            };
          }
          return {
            response: {
              jsonrpc: '2.0',
              id,
              result: {
                content: [{ type: 'text', text: `tool_failed:${String((err as Error)?.message ?? err)}` }],
                isError: true,
              },
            },
            status: 200,
          };
        }
      }
      default:
        return {
          response: { jsonrpc: '2.0', id, error: { code: -32601, message: 'method_not_found' } },
          status: 200,
        };
    }
  } catch (err) {
    return {
      response: { jsonrpc: '2.0', id, error: { code: -32603, message: 'internal_error' } },
      status: 200,
    };
  }
}

// ---------- control API + routing ----------

export interface ServerDeps {
  store: SessionStore;
  internalToken: string;
}

export function createRequestHandler(deps: ServerDeps) {
  return async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    try {
      const url = new URL(req.url ?? '/', 'http://localhost');
      const path = url.pathname;
      const method = req.method ?? 'GET';

      // ---- MCP (session-token auth inside; NOT internal-token) ----
      if (path === '/mcp' && method === 'POST') {
        const body = await readJsonBody(req);
        const { response, status } = await dispatchMcp(deps.store, body);
        if (response === null) {
          res.writeHead(status);
          res.end();
          return;
        }
        sendJson(res, status, response);
        return;
      }

      // ---- Control API (internal-token auth) ----
      if (path === '/v1/sessions' && method === 'POST') {
        requireInternal(req, deps.internalToken);
        const body = await readJsonBody(req);
        const { agent_id, task_id, resource_id, policy } = body ?? {};
        if (!agent_id || !task_id || !resource_id) {
          throw new HttpError(400, { error: 'binding_required' });
        }
        try {
          const s = await deps.store.create({ agent_id, task_id, resource_id }, policy);
          sendJson(res, 201, {
            session_id: s.id,
            token: s.token,
            expires_at: new Date(s.expiresAt).toISOString(),
          });
        } catch (err) {
          if (err instanceof BrowserBusyError) {
            throw new HttpError(503, { error: 'browser_busy' });
          }
          throw err;
        }
        return;
      }

      if (path === '/v1/sessions' && method === 'GET') {
        requireInternal(req, deps.internalToken);
        const now = Date.now();
        const sessions = deps.store.list().map((s) => ({
          id: s.id,
          agent_id: s.agentId,
          task_id: s.taskId,
          resource_id: s.resourceId,
          pages_used: s.pagesUsed,
          idle_seconds: Math.floor((now - s.lastActivityAt) / 1000),
          expires_at: new Date(s.expiresAt).toISOString(),
        }));
        sendJson(res, 200, { sessions });
        return;
      }

      const delMatch = path.match(/^\/v1\/sessions\/([A-Za-z0-9_]+)$/);
      if (delMatch && method === 'DELETE') {
        requireInternal(req, deps.internalToken);
        await deps.store.deleteById(delMatch[1]);
        res.writeHead(204); // always, idempotent
        res.end();
        return;
      }

      if (path === '/v1/sweeper/pass' && method === 'POST') {
        requireInternal(req, deps.internalToken);
        const reaped = deps.store.sweep();
        sendJson(res, 200, { reaped });
        return;
      }

      if (path === '/healthz' && method === 'GET') {
        sendJson(res, 200, { ok: true });
        return;
      }

      throw new HttpError(404, { error: 'not_found' });
    } catch (err) {
      if (err instanceof HttpError) {
        sendJson(res, err.status, err.body);
        return;
      }
      sendJson(res, 500, { error: 'internal_error' });
    }
  };
}

export function createServer(deps: ServerDeps): http.Server {
  return http.createServer(createRequestHandler(deps));
}

// ---------- composition root ----------

export interface StartOptions {
  port?: number;
  poolSize?: number;
  acquireTimeoutMs?: number;
  internalToken?: string;
  instanceFactory?: { create(id: string): Promise<BrowserInstance> | BrowserInstance };
}

export async function startServer(opts: StartOptions = {}): Promise<{ server: http.Server; store: SessionStore; pool: BrowserPool; port: number }> {
  const port = opts.port ?? Number(process.env.PORT ?? DEFAULT_PORT);
  const poolSize = opts.poolSize ?? Number(process.env.POOL_SIZE ?? 4);
  const acquireTimeoutMs = opts.acquireTimeoutMs ?? Number(process.env.BROWSER_ACQUIRE_TIMEOUT_MS ?? 30_000);
  const internalToken = opts.internalToken ?? process.env.BROWSER_INTERNAL_TOKEN ?? '';
  if (!internalToken) {
    throw new Error('BROWSER_INTERNAL_TOKEN must be set (refusing to run without auth)');
  }

  const factory =
    opts.instanceFactory ??
    {
      create: async (id: string) => {
        const { PlaywrightBrowserInstance } = await import('./browser');
        const inst = new PlaywrightBrowserInstance(id);
        await inst.launch();
        return inst;
      },
    };

  const pool = new BrowserPool(poolSize, factory, acquireTimeoutMs);
  const store = new SessionStore(pool);
  store.startSweeper(60_000);

  const server = createServer({ store, internalToken });
  await new Promise<void>((resolve) => server.listen(port, resolve));
  const actualPort = (server.address() as AddressInfo).port;
  return { server, store, pool, port: actualPort };
}

/* istanbul ignore next */
if (require.main === module) {
  startServer()
    .then(({ port }) => {
      // eslint-disable-next-line no-console
      console.log(`${SERVER_NAME} listening on :${port}`);
    })
    .catch((err) => {
      // eslint-disable-next-line no-console
      console.error('failed to start:', err?.message ?? err);
      process.exit(1);
    });
}
