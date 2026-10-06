import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  ensureFreeLaneShape, gateTool, reasoningEffortWire, applyReasoningEffort,
  classifyUpstream, openAIError, GATE_TOOL_NAMES,
} from '../src/freelane.mjs';

test('gate stubs are injected when tools are missing', () => {
  const out = ensureFreeLaneShape({ model: 'm', messages: [{ role: 'user', content: 'hi' }], stream: true });
  const names = out.tools.map((t) => t.function.name);
  for (const n of GATE_TOOL_NAMES) assert.ok(names.includes(n), `missing ${n}`);
  assert.equal(out.tool_choice, 'none', 'plain chat must not let the model call the stubs');
});

test('gate leaves an already-shaped body untouched (same object)', () => {
  const body = {
    model: 'm',
    messages: [{ role: 'user', content: 'hi' }],
    tools: GATE_TOOL_NAMES.map(gateTool),
  };
  assert.equal(ensureFreeLaneShape(body), body);
});

test('gate preserves a real tool list and its tool_choice', () => {
  const body = {
    model: 'm',
    messages: [],
    tools: [{ type: 'function', function: { name: 'bash', description: 'd', parameters: {} } }],
    tool_choice: 'auto',
  };
  const out = ensureFreeLaneShape(body);
  assert.equal(out.tool_choice, 'auto');
  assert.equal(out.tools.length, 2);
  assert.equal(out.tools.filter((t) => t.function.name === 'bash').length, 1, 'no duplicate bash');
});

test('reasoning effort: off maps to the wire spelling none', () => {
  assert.equal(reasoningEffortWire('off'), 'none');
  assert.equal(reasoningEffortWire('none'), 'none');
  assert.equal(reasoningEffortWire('high'), 'high');
  assert.equal(reasoningEffortWire('bogus'), undefined);
  assert.equal(reasoningEffortWire(undefined), undefined);
});

test('applyReasoningEffort injects only ladder values', () => {
  const body = { model: 'm', messages: [] };
  assert.equal(applyReasoningEffort(body, 'm', 'high').reasoning_effort, 'high');
  assert.equal(applyReasoningEffort(body, 'm', 'none').reasoning_effort, 'none');
  assert.equal(applyReasoningEffort(body, 'm', undefined), body);
});

test('applyReasoningEffort drops values the model did not declare', () => {
  const body = { model: 'm', messages: [] };
  const declared = ['minimal', 'low', 'medium', 'high', 'xhigh'];
  assert.equal(applyReasoningEffort(body, 'm', 'max', declared), body, 'max is not declared');
  assert.equal(applyReasoningEffort(body, 'm', 'low', declared).reasoning_effort, 'low');
  assert.equal(applyReasoningEffort(body, 'm', 'none', declared).reasoning_effort, 'none', 'off stays available');
});

test('region block is 403 with the upstream wording preserved', () => {
  const r = classifyUpstream(403, '{"type":"error","error":{"type":"RegionError","message":"This model is not available in your country."}}');
  assert.equal(r.http, 403);
  assert.equal(r.code, 'region_blocked');
  assert.match(r.message, /not available in your country/);
});

test('429 and free-usage-limit map to rate_limit_exceeded', () => {
  assert.equal(classifyUpstream(429, 'rate limited').http, 429);
  assert.equal(classifyUpstream(429, 'rate limited').code, 'rate_limit_exceeded');
  assert.equal(classifyUpstream(400, 'FreeUsageLimit exceeded').code, 'rate_limit_exceeded');
});

test('auth denials from zen never surface as an auth error to the client', () => {
  const r = classifyUpstream(401, 'unauthorized');
  assert.equal(r.http, 503, 'anonymous lane has no key concept: do not imply a bad key');
  const r403 = classifyUpstream(403, 'forbidden');
  assert.equal(r403.http, 503);
});

test('5xx and transport failures map to 503', () => {
  assert.equal(classifyUpstream(500, 'Internal server error').http, 503);
  assert.equal(classifyUpstream(0, 'fetch failed').http, 503);
  assert.equal(classifyUpstream(0, 'network timeout').code, 'zen_timeout');
});

test('oversized payload is reported as 413, not retried', () => {
  const r = classifyUpstream(413, 'payload too large');
  assert.equal(r.http, 413);
  assert.equal(r.code, 'invalid_request');
});

test('openAIError has the OpenAI error envelope', () => {
  const e = openAIError(429, 'rate_limit_exceeded', 'slow down');
  assert.equal(e.http, 429);
  assert.equal(e.body.error.code, 'rate_limit_exceeded');
  assert.equal(e.body.error.message, 'slow down');
});
