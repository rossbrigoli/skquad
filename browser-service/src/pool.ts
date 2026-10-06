/**
 * Warm pool of BrowserInstance (TG-6 §4).
 *
 * Instances are handed out EXCLUSIVELY — two sessions never share a Chromium
 * process. When the pool is exhausted, callers queue; after `acquireTimeoutMs`
 * (default 30 s) they get a BrowserBusyError (mapped to HTTP 503 / JSON-RPC
 * -32003 by the server).
 */

import { BrowserInstance } from './browser';

export class BrowserBusyError extends Error {
  constructor(message = 'browser_busy') {
    super(message);
    this.name = 'BrowserBusyError';
  }
}

export interface InstanceFactory {
  create(id: string): Promise<BrowserInstance> | BrowserInstance;
}

interface Waiter {
  resolve: (i: BrowserInstance) => void;
  reject: (e: Error) => void;
  timer: NodeJS.Timeout;
}

export class BrowserPool {
  private idle: BrowserInstance[] = [];
  private borrowed = new Set<BrowserInstance>();
  private waiters: Waiter[] = [];
  private created = 0;
  private closed = false;

  constructor(
    private readonly size: number,
    private readonly factory: InstanceFactory,
    private readonly acquireTimeoutMs: number = 30_000,
  ) {
    if (!Number.isFinite(size) || size < 1) throw new Error('pool size must be >= 1');
  }

  get stats(): { size: number; idle: number; borrowed: number; queued: number } {
    return {
      size: this.size,
      idle: this.idle.length,
      borrowed: this.borrowed.size,
      queued: this.waiters.length,
    };
  }

  /** Acquire an instance exclusively. Throws BrowserBusyError after timeout. */
  async acquire(timeoutMs: number = this.acquireTimeoutMs): Promise<BrowserInstance> {
    if (this.closed) throw new BrowserBusyError('pool_closed');

    const inst = this.idle.pop();
    if (inst) {
      if (!inst.isAlive()) {
        // Dead instance: drop it and try again (created count shrinks).
        this.created = Math.max(0, this.created - 1);
        return this.acquire(timeoutMs);
      }
      this.borrowed.add(inst);
      return inst;
    }

    if (this.created < this.size) {
      const fresh = await this.factory.create(`inst_${this.created}`);
      this.created += 1;
      this.borrowed.add(fresh);
      return fresh;
    }

    return new Promise<BrowserInstance>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w.timer !== timer);
        reject(new BrowserBusyError());
      }, timeoutMs);
      // Don't hold the event loop open just for the queue timer.
      timer.unref?.();
      this.waiters.push({ resolve, reject, timer });
    });
  }

  /** Return an instance. Handed directly to the next queued waiter if any. */
  release(inst: BrowserInstance): void {
    if (!this.borrowed.delete(inst)) return; // not borrowed by us — ignore
    if (this.closed) {
      void inst.close().catch(() => undefined);
      return;
    }
    const next = this.waiters.shift();
    if (next) {
      clearTimeout(next.timer);
      this.borrowed.add(inst);
      next.resolve(inst);
    } else {
      this.idle.push(inst);
    }
  }

  /** Close every instance and reject pending waiters. */
  async drain(): Promise<void> {
    this.closed = true;
    const all = [...this.idle, ...this.borrowed];
    this.idle = [];
    this.borrowed.clear();
    this.created = 0;
    const pending = this.waiters;
    this.waiters = [];
    for (const w of pending) {
      clearTimeout(w.timer);
      w.reject(new BrowserBusyError('pool_draining'));
    }
    await Promise.all(all.map((i) => i.close().catch(() => undefined)));
  }
}
