import { test } from 'node:test';
import assert from 'node:assert/strict';
import { toResponsesBody, translateEvent, newTranslateCtx } from '../src/responses.mjs';

test('chat body converts to a responses body with the gate tools', () => {
  const out = toResponsesBody({
    model: 'muse-spark-1.3-contributor-free',
    messages: [
      { role: 'system', content: 'be brief' },
      { role: 'user', content: 'hi' },
    ],
    max_tokens: 128,
    temperature: 0.2,
  }, 'muse-spark-1.3-contributor-free', 'minimal');
  assert.equal(out.model, 'muse-spark-1.3-contributor-free');
  assert.equal(out.stream, true);
  assert.equal(out.max_output_tokens, 128);
  assert.equal(out.temperature, 0.2);
  assert.deepEqual(out.reasoning, { effort: 'minimal' });
  assert.equal(out.tool_choice, 'auto', 'responses accepts auto only');
  const names = out.tools.map((t) => t.name);
  assert.ok(names.includes('bash') && names.includes('read'), 'free-lane gate needs both');
  assert.equal(out.tools[0].type, 'function');
  assert.equal(out.input[0].role, 'system');
  assert.deepEqual(out.input[0].content, [{ type: 'input_text', text: 'be brief' }]);
});

test('image parts become input_image on the responses path', () => {
  const out = toResponsesBody({
    model: 'm',
    messages: [{ role: 'user', content: [{ type: 'text', text: 'look' }, { type: 'image_url', image_url: { url: 'data:image/png;base64,AAA' } }] }],
  }, 'm');
  const parts = out.input[0].content;
  assert.deepEqual(parts[0], { type: 'input_text', text: 'look' });
  assert.equal(parts[1].type, 'input_image');
  assert.equal(parts[1].image_url, 'data:image/png;base64,AAA');
});

test('reasoning effort off is not sent on the responses path', () => {
  const out = toResponsesBody({ model: 'm', messages: [] }, 'm', 'none');
  assert.equal(out.reasoning, undefined, 'omitting keeps the provider default; "none" is not a valid effort here');
});

test('text deltas become chat chunks', () => {
  const ctx = newTranslateCtx('muse-spark-1.3-contributor-free');
  const a = translateEvent('response.output_text.delta', { delta: 'Hel' }, ctx);
  assert.equal(a.out.length, 1);
  assert.match(a.out[0], /^data: /);
  const payload = JSON.parse(a.out[0].slice(6));
  assert.equal(payload.object, 'chat.completion.chunk');
  assert.equal(payload.choices[0].delta.content, 'Hel');
  assert.equal(payload.choices[0].delta.role, 'assistant', 'first chunk carries the role');
  const b = translateEvent('response.output_text.delta', { delta: 'lo' }, ctx);
  const pb = JSON.parse(b.out[0].slice(6));
  assert.equal(pb.choices[0].delta.role, undefined, 'subsequent chunks omit the role');
  assert.equal(pb.choices[0].delta.content, 'lo');
  assert.equal(ctx.sawContent, true);
});

test('completion emits a finish chunk and usage', () => {
  const ctx = newTranslateCtx('m');
  const r = translateEvent('response.completed', {
    response: { usage: { input_tokens: 106, output_tokens: 8, total_tokens: 114 } },
  }, ctx);
  assert.equal(r.done, true);
  assert.equal(r.fail, null);
  assert.deepEqual(r.usage, { prompt_tokens: 106, completion_tokens: 8, total_tokens: 114 });
  const payload = JSON.parse(r.out[0].slice(6));
  assert.equal(payload.choices[0].finish_reason, 'stop');
});

test('incomplete maps max_output_tokens to finish_reason length', () => {
  const ctx = newTranslateCtx('m');
  const r = translateEvent('response.incomplete', { response: { incomplete_details: { reason: 'max_output_tokens' } } }, ctx);
  assert.equal(r.done, true);
  const payload = JSON.parse(r.out[0].slice(6));
  assert.equal(payload.choices[0].finish_reason, 'length');
});

test('failures surface as a message, not as silent success', () => {
  const ctx = newTranslateCtx('m');
  const a = translateEvent('response.failed', { response: { error: { message: 'upstream exploded' } } }, ctx);
  assert.equal(a.done, true);
  assert.equal(a.fail, 'upstream exploded');
  const b = translateEvent('error', { error: { message: 'bad request' } }, ctx);
  assert.equal(b.fail, 'bad request');
});

test('unrelated events are ignored', () => {
  const ctx = newTranslateCtx('m');
  for (const t of ['response.created', 'response.in_progress', 'response.output_item.added', 'response.reasoning_summary_text.delta']) {
    const r = translateEvent(t, {}, ctx);
    assert.equal(r.out.length, 0);
    assert.equal(r.done, false);
  }
});
