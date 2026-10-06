import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserPool, BrowserBusyError } from '../src/pool';
import { FakeBrowserInstance } from '../src/browser';

function makePool(size: number, timeoutMs = 250) {
  const made: FakeBrowserInstance[] = [];
  const pool = new BrowserPool(
    size,
    {
      create: async (id) => {
        const i = new FakeBrowserInstance(id);
        made.push(i);
        await i.launch();
        return i;
      },
    },
    timeoutMs,
  );
  return { pool, made };
}

test('pool: hands out instances exclusively (two acquires never same instance)', async () => {
  const { pool } = makePool(3);
  const a = await pool.acquire();
  const b = await pool.acquire();
  const c = await pool.acquire();
  assert.notEqual(a, b);
  assert.notEqual(b, c);
  assert.notEqual(a, c);
  assert.equal(pool.stats.borrowed, 3);
  await pool.drain();
});

test('pool: released instance is reused, still exclusive', async () => {
  const { pool } = makePool(1);
  const a = await pool.acquire();
  pool.release(a);
  const b = await pool.acquire();
  assert.equal(a, b, 'released instance should be reused');
  assert.equal(pool.stats.borrowed, 1);
  await pool.drain();
});

test('pool: busy timeout → BrowserBusyError', async () => {
  const { pool } = makePool(1, 120);
  const a = await pool.acquire();
  await assert.rejects(pool.acquire(), BrowserBusyError);
  pool.release(a);
  await pool.drain();
});

test('pool: queued waiter receives instance released before timeout', async () => {
  const { pool } = makePool(1, 5_000);
  const a = await pool.acquire();
  const p = pool.acquire();
  setTimeout(() => pool.release(a), 30);
  const b = await p;
  assert.equal(a, b);
  pool.release(b);
  await pool.drain();
});

test('pool: dead idle instance is replaced, not handed out', async () => {
  const { pool, made } = makePool(1);
  const a = await pool.acquire();
  pool.release(a);
  (a as FakeBrowserInstance).crash();
  const b = await pool.acquire();
  assert.notEqual(a, b, 'crashed instance must not be handed out');
  assert.equal(b.isAlive(), true);
  assert.equal(made.length, 2);
  await pool.drain();
});

test('pool: drain rejects queued waiters', async () => {
  const { pool } = makePool(1, 5_000);
  const a = await pool.acquire();
  const p = pool.acquire();
  await pool.drain();
  await assert.rejects(p, BrowserBusyError);
});
