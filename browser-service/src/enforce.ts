/**
 * Navigation-ceiling enforcement (TG-6 §4).
 *
 * Pure-ish functions used by BOTH the tool layer (fail-fast pre-checks) and the
 * the Context.route(doublestar-all) callback (security core: every subresource,
 * fetch/XHR/WebSocket passes through here). Fail-closed by design: if we cannot
 * prove a request is safe, we deny it.
 */

import dns from 'node:dns/promises';
import net from 'node:net';

export interface BrowserPolicy {
  deny_hosts?: string[];
  max_pages?: number;
  max_screenshot_bytes?: number;
  idle_timeout_s?: number;
  max_session_minutes?: number;
}

export type RequiredPolicy = Required<BrowserPolicy>;

export const POLICY_DEFAULTS: RequiredPolicy = {
  deny_hosts: [],
  max_pages: 50,
  max_screenshot_bytes: 2_097_152,
  idle_timeout_s: 600,
  max_session_minutes: 30,
};

export function normalizePolicy(p?: BrowserPolicy | null): RequiredPolicy {
  return {
    deny_hosts: Array.isArray(p?.deny_hosts) ? [...p!.deny_hosts] : [],
    max_pages: positiveInt(p?.max_pages, POLICY_DEFAULTS.max_pages),
    max_screenshot_bytes: positiveInt(p?.max_screenshot_bytes, POLICY_DEFAULTS.max_screenshot_bytes),
    idle_timeout_s: positiveInt(p?.idle_timeout_s, POLICY_DEFAULTS.idle_timeout_s),
    max_session_minutes: positiveInt(p?.max_session_minutes, POLICY_DEFAULTS.max_session_minutes),
  };
}

function positiveInt(v: unknown, dflt: number): number {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : dflt;
}

export interface Decision {
  allowed: boolean;
  reason?: string;
}

const ALLOW: Decision = { allowed: true };

function deny(reason: string): Decision {
  return { allowed: false, reason };
}

/**
 * netguard-style host matching: exact match, subdomain match for plain
 * patterns, and glob patterns with `*` wildcards.
 *
 * "metadata.google.internal" matches that host and `x.metadata.google.internal`.
 * "*.internal" matches "foo.internal", "a.b.internal" and bare "internal".
 */
export function matchDenyHost(host: string, denyHosts: string[]): boolean {
  const h = host.toLowerCase().replace(/\.$/, '');
  for (const raw of denyHosts) {
    const pat = String(raw ?? '').toLowerCase().trim();
    if (!pat) continue;
    if (pat.includes('*')) {
      const regex = new RegExp(
        '^' +
          pat
            .split('*')
            .map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
            .join('[a-z0-9._-]*') +
          '$',
      );
      if (regex.test(h)) return true;
    } else if (h === pat || h.endsWith('.' + pat)) {
      return true;
    }
  }
  return false;
}

/** Loopback / RFC1918 / link-local / CGNAT / IPv6 ULA / unspecified. */
export function isPrivateIp(ip: string): boolean {
  const v = ip.trim().toLowerCase().replace(/^\[|\]$/g, '');
  if (net.isIPv6(v)) {
    if (v === '::' || v === '::1') return true;
    if (/^f[cd][0-9a-f]{2}:/i.test(v)) return true; // fc00::/7 ULA
    if (/^fe[89abc][0-9a-f]:/i.test(v)) return true; // fe80::/10 link-local
    const m = v.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
    if (m) return isPrivateIp(m[1]);
    return false;
  }
  if (net.isIPv4(v)) {
    const parts = v.split('.').map((x) => Number(x));
    const [a, b] = parts;
    if (a === 0 || a === 127) return true; // unspecified + loopback
    if (a === 10) return true; // RFC1918
    if (a === 172 && b >= 16 && b <= 31) return true; // RFC1918
    if (a === 192 && b === 168) return true; // RFC1918
    if (a === 169 && b === 254) return true; // link-local (incl. cloud metadata)
    if (a === 100 && b >= 64 && b <= 127) return true; // CGNAT 100.64/10
    return false;
  }
  return true; // unparseable → fail closed
}

/**
 * Host-level check: deny-list first (no DNS needed), then resolve and deny
 * private/loopback/link-local/CGNAT addresses. DNS failure → deny (fail closed).
 */
export async function checkHostAllowed(host: string, policy: RequiredPolicy): Promise<Decision> {
  const bare = host.toLowerCase().replace(/\.$/, '');
  if (!bare) return deny('empty_host');
  if (matchDenyHost(bare, policy.deny_hosts)) return deny(`host_denied:${bare}`);

  let ips: string[];
  if (net.isIP(bare)) {
    ips = [bare];
  } else {
    const results = await Promise.allSettled([dns.resolve4(bare), dns.resolve6(bare)]);
    ips = results.flatMap((r) => (r.status === 'fulfilled' ? r.value : []));
    if (ips.length === 0) return deny(`dns_unresolved_fail_closed:${bare}`);
  }
  for (const ip of ips) {
    if (isPrivateIp(ip)) return deny(`private_ip_denied:${bare}->${ip}`);
  }
  return ALLOW;
}

export interface NavContext {
  /** Top-level navigations (documents) count toward max_pages. */
  topLevel: boolean;
  pagesUsed: number;
}

/** Full navigation check: scheme + max_pages + host policy. */
export async function checkRequest(
  urlStr: string,
  policy: RequiredPolicy,
  nav: NavContext,
): Promise<Decision> {
  let u: URL;
  try {
    u = new URL(urlStr);
  } catch {
    return deny('malformed_url');
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    return deny(`scheme_not_allowed:${u.protocol}`);
  }
  if (nav.topLevel && nav.pagesUsed >= policy.max_pages) {
    return deny(`max_pages_exceeded:${policy.max_pages}`);
  }
  return checkHostAllowed(u.hostname, policy);
}
