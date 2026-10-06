import test from 'node:test';
import assert from 'node:assert/strict';
import { matchDenyHost, isPrivateIp, checkRequest, normalizePolicy } from '../src/enforce';

test('matchDenyHost: plain pattern matches exact host and subdomains', () => {
  assert.equal(matchDenyHost('metadata.google.internal', ['metadata.google.internal']), true);
  assert.equal(matchDenyHost('x.metadata.google.internal', ['metadata.google.internal']), true);
  assert.equal(matchDenyHost('notgoogle.internal', ['metadata.google.internal']), false);
});

test('matchDenyHost: glob *.internal matches sub/superdomain and bare domain', () => {
  assert.equal(matchDenyHost('foo.internal', ['*.internal']), true);
  assert.equal(matchDenyHost('a.b.internal', ['*.internal']), true);
  assert.equal(matchDenyHost('internal', ['*.internal']), false, 'bare domain needs exact entry');
  assert.equal(matchDenyHost('external.com', ['*.internal']), false);
});

test('matchDenyHost: case-insensitive, trailing dot tolerated', () => {
  assert.equal(matchDenyHost('FOO.INTERNAL.', ['*.internal']), true);
  assert.equal(matchDenyHost('Evil.Example.COM', ['evil.example.com']), true);
});

test('isPrivateIp: loopback/private/link-local/CGNAT/ULA denied', () => {
  for (const ip of [
    '127.0.0.1', '127.5.5.5', '10.0.0.1', '10.255.255.255',
    '172.16.0.1', '172.31.255.255', '192.168.1.1', '169.254.169.254',
    '100.64.0.1', '100.127.255.255', '0.0.0.0',
    '::1', 'fc00::1', 'fd12::34', 'fe80::1', '::', '::ffff:192.168.0.1',
  ]) {
    assert.equal(isPrivateIp(ip), true, `${ip} should be private`);
  }
});

test('isPrivateIp: public IPs allowed', () => {
  for (const ip of ['8.8.8.8', '93.184.216.34', '172.15.0.1', '172.32.0.1', '100.63.0.1', '100.128.0.1', '2606:4700::1111']) {
    assert.equal(isPrivateIp(ip), false, `${ip} should be public`);
  }
});

test('checkRequest: non-http(s) schemes denied', async () => {
  const p = normalizePolicy(null);
  for (const url of ['file:///etc/passwd', 'gopher://x/y', 'ftp://host/f', 'data:text/html,hi', 'chrome://settings']) {
    const d = await checkRequest(url, p, { topLevel: true, pagesUsed: 0 });
    assert.equal(d.allowed, false, url);
    assert.match(d.reason!, /scheme_not_allowed/);
  }
});

test('checkRequest: malformed URL denied', async () => {
  const d = await checkRequest('not a url', normalizePolicy(null), { topLevel: true, pagesUsed: 0 });
  assert.equal(d.allowed, false);
  assert.match(d.reason!, /malformed_url/);
});

test('checkRequest: deny_hosts blocks before DNS', async () => {
  const p = normalizePolicy({ deny_hosts: ['*.internal'] });
  const d = await checkRequest('http://anything.internal/x', p, { topLevel: true, pagesUsed: 0 });
  assert.equal(d.allowed, false);
  assert.match(d.reason!, /host_denied/);
});

test('checkRequest: private literal IPs denied (no DNS needed)', async () => {
  const p = normalizePolicy(null);
  for (const url of [
    'http://127.0.0.1/admin',
    'http://10.1.2.3/',
    'http://172.20.0.5/',
    'http://192.168.68.131/',
    'http://169.254.169.254/latest/meta-data/',
    'http://100.64.1.2/',
  ]) {
    const d = await checkRequest(url, p, { topLevel: true, pagesUsed: 0 });
    assert.equal(d.allowed, false, url);
    assert.match(d.reason!, /private_ip_denied/);
  }
});

test('checkRequest: DNS failure fails closed', async () => {
  const p = normalizePolicy(null);
  const d = await checkRequest('http://definitely-does-not-exist.invalid/', p, {
    topLevel: true,
    pagesUsed: 0,
  });
  assert.equal(d.allowed, false);
  assert.match(d.reason!, /dns_unresolved_fail_closed/);
});

test('checkRequest: max_pages enforced for top-level only', async () => {
  const p = normalizePolicy({ max_pages: 2 });
  const d = await checkRequest('http://93.184.216.34/', p, { topLevel: true, pagesUsed: 2 });
  assert.equal(d.allowed, false);
  assert.match(d.reason!, /max_pages_exceeded/);
  // subresources don't count toward max_pages
  const ok = await checkRequest('http://93.184.216.34/app.js', p, { topLevel: false, pagesUsed: 99 });
  assert.equal(ok.allowed, true);
});

test('checkRequest: public literal IP allowed', async () => {
  const d = await checkRequest('http://93.184.216.34/page', normalizePolicy(null), {
    topLevel: true,
    pagesUsed: 0,
  });
  assert.equal(d.allowed, true);
});

test('normalizePolicy: defaults applied', () => {
  const p = normalizePolicy({});
  assert.equal(p.max_pages, 50);
  assert.equal(p.max_screenshot_bytes, 2_097_152);
  assert.equal(p.idle_timeout_s, 600);
  assert.equal(p.max_session_minutes, 30);
  assert.deepEqual(p.deny_hosts, []);
});
