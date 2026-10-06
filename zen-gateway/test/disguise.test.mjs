import { test } from 'node:test';
import assert from 'node:assert/strict';
import { canonicalSessionID, conversationSeed, deriveRequestIDs, disguiseHeaders, ANONYMOUS_KEY } from '../src/disguise.mjs';

test('canonical session id has the official shape', () => {
  const sid = canonicalSessionID('hello world');
  assert.match(sid, /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
});

test('canonical session id is deterministic per conversation', () => {
  assert.equal(canonicalSessionID('same seed'), canonicalSessionID('same seed'));
  assert.notEqual(canonicalSessionID('seed a'), canonicalSessionID('seed b'));
});

test('already canonical ids pass through unchanged', () => {
  const canonical = 'ses_0123456789abABCDEFGHIJKLMN';
  assert.match(canonical, /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  assert.equal(canonicalSessionID(canonical), canonical);
});

test('conversation seed uses the first user turn, not later ones', () => {
  const messages = [
    { role: 'system', content: 'sys' },
    { role: 'user', content: 'first' },
    { role: 'assistant', content: 'ok' },
    { role: 'user', content: 'second' },
  ];
  assert.equal(conversationSeed(messages), JSON.stringify('first'));
});

test('deriveRequestIDs gives fresh request ids but a stable session', () => {
  const messages = [{ role: 'user', content: 'hi' }];
  const a = deriveRequestIDs(messages, 'linux', 'x64');
  const b = deriveRequestIDs(messages, 'linux', 'x64');
  assert.equal(a.session, b.session);
  assert.notEqual(a.request, b.request);
  assert.match(a.request, /^req_[0-9a-f]{32}$/);
  assert.match(a.project, /^prj_[0-9a-f]{24}$/);
});

test('disguise headers carry the full CLI correlation set', () => {
  const ids = deriveRequestIDs([{ role: 'user', content: 'hi' }], 'linux', 'x64');
  const h = disguiseHeaders(ids);
  assert.equal(h.authorization, `Bearer ${ANONYMOUS_KEY}`);
  assert.match(h['user-agent'], /^opencode\/\d+\.\d+\.\d+ \(linux x64; node/);
  assert.equal(h['x-opencode-client'], 'cli');
  assert.equal(h['x-opencode-session'], ids.session);
  assert.equal(h['x-session-affinity'], ids.session);
  assert.equal(h['X-Session-Id'], ids.session);
  assert.equal(h['x-opencode-request'], ids.request);
  assert.equal(h['x-opencode-project'], ids.project);
});
