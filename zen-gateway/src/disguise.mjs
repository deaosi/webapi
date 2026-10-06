// disguise.mjs — CLI disguise header set + canonical session/request ids.
// Mirrors the live-probed free-lane gate: Bearer public alone is NOT enough
// since 2026-09-16; upstream also checks the official session id shape and
// the agent-shaped body (see frelane.mjs).
import { createHash, randomBytes } from 'node:crypto';

export const ANONYMOUS_KEY = 'public';
export const CLI_VERSION = '1.18.31';

const BASE62 = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz';
const SESSION_RE = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/;

function base62Fixed(value, width) {
  let n = value;
  const out = new Array(width);
  for (let i = width - 1; i >= 0; i--) {
    out[i] = BASE62.charAt(Number(n % 62n));
    n /= 62n;
  }
  return out.join('');
}

/** Canonical OpenCode session id: passes through when already canonical,
 *  otherwise deterministically hashed into the canonical shape. */
export function canonicalSessionID(signal) {
  const s = String(signal ?? '');
  if (SESSION_RE.test(s)) return s;
  const sum = createHash('sha256').update('ses\x00' + s).digest();
  const timePart = sum.subarray(0, 6).toString('hex');
  const randomPart = base62Fixed(BigInt('0x' + sum.subarray(6, 16).toString('hex')), 14);
  return `ses_${timePart}${randomPart}`;
}

export function conversationSeed(messages) {
  for (const m of messages ?? []) {
    if (m?.role !== 'user') continue;
    const enc = JSON.stringify(m.content ?? null);
    if (enc && enc !== 'null') return enc;
  }
  return '';
}

export function deriveRequestIDs(messages, platform = process.platform, arch = process.arch) {
  let signal = conversationSeed(messages);
  if (!signal) signal = `fallback_${randomBytes(16).toString('hex')}`;
  return {
    session: canonicalSessionID(signal),
    request: `req_${randomBytes(16).toString('hex')}`,
    project: `prj_${createHash('sha256').update('prj\x00opencode2dsh:default-project').digest().subarray(0, 12).toString('hex')}`,
    userAgent: `opencode/${CLI_VERSION} (${platform} ${arch}; node${process.versions.node})`,
  };
}

export function disguiseHeaders(ids) {
  return {
    authorization: `Bearer ${ANONYMOUS_KEY}`,
    'user-agent': ids.userAgent,
    'x-opencode-client': 'cli',
    'x-opencode-session': ids.session,
    'x-session-affinity': ids.session,
    'X-Session-Id': ids.session,
    'x-opencode-request': ids.request,
    'x-opencode-project': ids.project,
    accept: 'application/json, text/event-stream',
    'content-type': 'application/json',
  };
}
