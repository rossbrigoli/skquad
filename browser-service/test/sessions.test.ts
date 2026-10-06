import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserPool } from '../src/pool';
import { SessionStore } from '../src/sessions';
import { FakeBrowserInstance } from '../src/browser';

function makeStore(size = 4) {
  const made: FakeBrowserInstance[] = [];
  const pool = new BrowserPool(
    size,
    {
      create: async (id) => {
        const f = new FakeBrowserInstance(id);
        made.push(f);
        await f.launch();
        return f;
      },
    },
    250,
  );
  return { pool, store: new SessionStore(pool), made };
}

const BINDING = { agent_id: 'agent-1', task_id: 'task-1', resource_id: 'res-1' };

test('session create: id/token format, hard cap, binding', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { max_session_minutes: 30 });
  assert.match(s.id, /^bs_[0-9a-f]{32}$/);
  assert.match(s.token, /^[0-9a-f]{48}$/);
  assert.equal(s.agentId, 'agent-1');
  assert.equal(s.taskId, 'task-1');
  assert.equal(s.resourceId, 'res-1');
  assert.equal(s.expiresAt - s.createdAt, 30 * 60_000);
  assert.equal(s.pagesUsed, 0);
  assert.equal(s.closed, false);
  // one instance + one context per session (§4 isolation)
  assert.equal((s.instance as any).contexts.length, 1);
  await store.closeSession(s);
  await pool.drain();
});

test('session: route handler registered with **/* pattern', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, null);
  const ctx = s.context as any;
  assert.equal(ctx.routeHandlers.length, 1, 'exactly one route handler wired');
  await store.closeSession(s);
  await pool.drain();
});

test('session route: document nav increments pagesUsed; subresource does not', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { deny_hosts: [] });
  const ctx = s.context as any;
  const r1 = await ctx.fireRoute({ url: 'http://93.184.216.34/', resourceType: 'document' });
  assert.equal(r1, 'continued');
  assert.equal(s.pagesUsed, 1);
  const r2 = await ctx.fireRoute({ url: 'http://93.184.216.34/app.js', resourceType: 'script' });
  assert.equal(r2, 'continued');
  assert.equal(s.pagesUsed, 1, 'subresources must not count toward max_pages');
  await store.closeSession(s);
  await pool.drain();
});

test('session close: destroys context, releases instance, token invalidated', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, null);
  const inst = s.instance as FakeBrowserInstance;
  await store.closeSession(s);
  assert.equal(s.closed, true);
  assert.equal((s.context as any).closed, true, 'context destroyed');
  assert.equal(pool.stats.borrowed, 0, 'instance released back to pool');
  assert.equal(inst.closed, false, 'alive instance is pooled, not closed');
  assert.equal(store.getByToken(s.token), undefined);
  await pool.drain();
});

test('sweep: idle timeout reaps', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { idle_timeout_s: 1 });
  s.lastActivityAt = Date.now() - 2_000;
  const reaped = store.sweep();
  assert.equal(reaped, 1);
  assert.equal(store.getByToken(s.token), undefined);
  await pool.drain();
});

test('sweep: hard cap reaps even when active', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { max_session_minutes: 1, idle_timeout_s: 3600 });
  const reaped = store.sweep(Date.now() + 61_000);
  assert.equal(reaped, 1);
  assert.equal(store.getById(s.id), undefined);
  await pool.drain();
});

test('sweep: crashed instance reaps', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { idle_timeout_s: 3600, max_session_minutes: 999 });
  (s.instance as FakeBrowserInstance).crash();
  const reaped = store.sweep();
  assert.equal(reaped, 1);
  await pool.drain();
});

test('sweep: healthy session survives', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { idle_timeout_s: 3600, max_session_minutes: 999 });
  const reaped = store.sweep();
  assert.equal(reaped, 0);
  assert.notEqual(store.getById(s.id), undefined);
  await store.closeSession(s);
  await pool.drain();
});

test('deleteById: idempotent', async () => {
  const { store, pool } = makeStore(1);
  assert.equal(await store.deleteById('bs_nonexistent'), false);
  const s = await store.create(BINDING, null);
  assert.equal(await store.deleteById(s.id), true);
  assert.equal(await store.deleteById(s.id), false, 'second delete is a no-op');
  await pool.drain();
});

test('touch: resets idle clock', async () => {
  const { store, pool } = makeStore(1);
  const s = await store.create(BINDING, { idle_timeout_s: 1 });
  s.lastActivityAt = Date.now() - 900;
  store.touch(s);
  assert.ok(Date.now() - s.lastActivityAt < 100);
  assert.equal(store.sweep(), 0);
  await store.closeSession(s);
  await pool.drain();
});
