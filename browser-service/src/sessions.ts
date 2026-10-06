/**
 * Session store (TG-6 §2 + §4).
 *
 * A session is immutably bound to (agent_id, task_id, resource_id), owns
 * EXACTLY ONE Chromium instance + ONE BrowserContext (destroyed on
 * close/idle/reap, no profile persistence), and carries the enforcement
 * policy. The Context.route(doublestar-all) wiring lives here — this is the security
 * core: every request the page makes (top-level, subresources, fetch/XHR,
 * WebSocket) is checked against the session policy before it may leave.
 */

import crypto from 'node:crypto';
import { BrowserContextHandle, BrowserInstance, BrowserPageHandle } from './browser';
import { BrowserPolicy, RequiredPolicy, checkRequest, normalizePolicy } from './enforce';
import { BrowserPool } from './pool';

export interface SessionBinding {
  agent_id: string;
  task_id: string;
  resource_id: string;
}

export interface Session {
  id: string; // bs_<32hex>
  token: string; // <48hex>
  readonly agentId: string;
  readonly taskId: string;
  readonly resourceId: string;
  readonly policy: RequiredPolicy;
  readonly createdAt: number;
  expiresAt: number; // hard cap: createdAt + max_session_minutes
  lastActivityAt: number;
  pagesUsed: number;
  instance: BrowserInstance;
  context: BrowserContextHandle;
  /** Lazily-created working page for click/type/screenshot/extract. */
  page: BrowserPageHandle | null;
  closed: boolean;
}

export function newSessionId(): string {
  return 'bs_' + crypto.randomBytes(16).toString('hex');
}

export function newSessionToken(): string {
  return crypto.randomBytes(24).toString('hex'); // 48 hex chars
}

export class SessionStore {
  private byId = new Map<string, Session>();
  private byToken = new Map<string, Session>();
  private sweeperTimer: NodeJS.Timeout | null = null;

  constructor(private readonly pool: BrowserPool) {}

  /**
   * Create a session: acquire a pool instance, build a private context, wire
   * route enforcement. Propagates BrowserBusyError when no capacity.
   */
  async create(binding: SessionBinding, policyIn?: BrowserPolicy | null): Promise<Session> {
    const policy = normalizePolicy(policyIn);
    const instance = await this.pool.acquire();
    let context: BrowserContextHandle;
    try {
      context = await instance.newContext();
    } catch (err) {
      this.pool.release(instance);
      throw err;
    }

    const now = Date.now();
    const session: Session = {
      id: newSessionId(),
      token: newSessionToken(),
      agentId: binding.agent_id,
      taskId: binding.task_id,
      resourceId: binding.resource_id,
      policy,
      createdAt: now,
      expiresAt: now + policy.max_session_minutes * 60_000,
      lastActivityAt: now,
      pagesUsed: 0,
      instance,
      context,
      page: null,
      closed: false,
    };

    // ---- SECURITY CORE: Context.route(doublestar-all) → enforce.ts for ALL traffic ----
    await context.route('**/*', async (req, ctrl) => {
      const topLevel = req.resourceType === 'document';
      const decision = await checkRequest(req.url, policy, {
        topLevel,
        pagesUsed: session.pagesUsed,
      });
      if (!decision.allowed) {
        await ctrl.abort();
        return;
      }
      if (topLevel) session.pagesUsed += 1; // top-level navigations count toward max_pages
      await ctrl.continue();
    });

    this.byId.set(session.id, session);
    this.byToken.set(session.token, session);
    return session;
  }

  getByToken(token: string): Session | undefined {
    const s = this.byToken.get(token);
    if (!s || s.closed) return undefined;
    return s;
  }

  getById(id: string): Session | undefined {
    const s = this.byId.get(id);
    if (!s || s.closed) return undefined;
    return s;
  }

  /** Record activity (resets the idle clock). */
  touch(session: Session): void {
    session.lastActivityAt = Date.now();
  }

  list(): Session[] {
    return [...this.byId.values()].filter((s) => !s.closed);
  }

  /** Force-close a session by id. Idempotent: returns true if one was closed. */
  async deleteById(id: string): Promise<boolean> {
    const s = this.byId.get(id);
    if (!s || s.closed) return false;
    await this.closeSession(s);
    return true;
  }

  /** Close a session: destroy context + release the instance back to the pool. */
  async closeSession(session: Session): Promise<void> {
    if (session.closed) return;
    session.closed = true;
    this.byId.delete(session.id);
    this.byToken.delete(session.token);
    try {
      await session.context.close();
    } catch {
      /* context may already be gone (crash) */
    }
    if (session.instance.isAlive()) {
      this.pool.release(session.instance);
    }
    // Crashed instances are not returned to the pool.
  }

  /**
   * Orphan sweep (§2): reap sessions idle past idle_timeout_s, past the hard
   * cap, or whose instance crashed. Returns the number reaped.
   */
  sweep(now: number = Date.now()): number {
    let reaped = 0;
    for (const s of [...this.byId.values()]) {
      const idleSeconds = (now - s.lastActivityAt) / 1000;
      if (idleSeconds >= s.policy.idle_timeout_s || now >= s.expiresAt || !s.instance.isAlive()) {
        void this.closeSession(s);
        reaped += 1;
      }
    }
    return reaped;
  }

  /** Start the internal 60 s sweeper. */
  startSweeper(intervalMs = 60_000): void {
    if (this.sweeperTimer) return;
    this.sweeperTimer = setInterval(() => this.sweep(), intervalMs);
    this.sweeperTimer.unref?.();
  }

  stopSweeper(): void {
    if (this.sweeperTimer) {
      clearInterval(this.sweeperTimer);
      this.sweeperTimer = null;
    }
  }
}
