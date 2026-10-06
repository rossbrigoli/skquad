/**
 * TG-6: Chromium must ALWAYS launch routed through the netguard egress
 * sidecar (docs/tg6-browser-protocol.md §5). These tests pin the launch
 * options that make that true.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromiumLaunchOptions, DEFAULT_BROWSER_PROXY } from '../src/browser';

test('default launch routes Chromium through the netguard sidecar proxy', () => {
  const saved = process.env.BROWSER_PROXY;
  delete process.env.BROWSER_PROXY;
  try {
    const opts = chromiumLaunchOptions('/usr/bin/chromium');
    assert.equal(opts.headless, true);
    assert.equal(opts.executablePath, '/usr/bin/chromium');
    assert.ok(
      opts.args.includes(`--proxy-server=${DEFAULT_BROWSER_PROXY}`),
      `expected proxy arg, got: ${JSON.stringify(opts.args)}`,
    );
    // Loopback must NOT bypass the guard either.
    assert.ok(opts.args.includes('--proxy-bypass-list=<-loopback>'));
  } finally {
    if (saved !== undefined) process.env.BROWSER_PROXY = saved;
  }
});

test('BROWSER_PROXY env overrides the sidecar URL', () => {
  const opts = chromiumLaunchOptions('/usr/bin/chromium', 'http://127.0.0.1:9999');
  assert.ok(opts.args.includes('--proxy-server=http://127.0.0.1:9999'));
  assert.ok(!opts.args.includes('--proxy-server=http://127.0.0.1:8888'));
});

test("BROWSER_PROXY='none' disables the proxy (dev-only escape hatch)", () => {
  const opts = chromiumLaunchOptions('/usr/bin/chromium', 'none');
  assert.deepEqual(opts.args, []);
});

test('BROWSER_PROXY env var is read when no explicit proxy passed', () => {
  const saved = process.env.BROWSER_PROXY;
  process.env.BROWSER_PROXY = 'http://127.0.0.1:7777';
  try {
    const opts = chromiumLaunchOptions('/usr/bin/chromium');
    assert.ok(opts.args.includes('--proxy-server=http://127.0.0.1:7777'));
  } finally {
    if (saved === undefined) delete process.env.BROWSER_PROXY;
    else process.env.BROWSER_PROXY = saved;
  }
});
