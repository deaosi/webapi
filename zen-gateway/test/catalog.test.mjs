import { test } from 'node:test';
import assert from 'node:assert/strict';
import { decodeModelsDev, decide, enrich, ZenCatalog, ZEN_CHAT_MODELS_SEED, SEED_IDS } from '../src/catalog.mjs';

const MODELS_DEV = {
  openrouter: { models: { 'x/y': { cost: { input: 1, output: 1 } } } },
  opencode: {
    models: {
      'mimo-v2.6-flash-free': { id: 'mimo-v2.6-flash-free', cost: { input: 0, output: 0 }, limit: { context: 200000, output: 32000 }, modalities: { input: ['text', 'image'] }, reasoning: true },
      'space-bunny-free': { id: 'space-bunny-free', cost: { input: 0, output: 0 }, limit: { context: 1048576, output: 524288 }, reasoning: true, reasoning_options: [{ type: 'effort', values: ['low', 'high'] }] },
      'paid-model': { id: 'paid-model', cost: { input: 3, output: 15 } },
      'dead-model': { id: 'dead-model', cost: { input: 0, output: 0 }, status: 'deprecated' },
      'gone-model': { id: 'gone-model', cost: { input: 0, output: 0 }, deprecated_at: '2026-09-01' },
      'unknown-cost': { id: 'unknown-cost' },
    },
  },
};

test('decodeModelsDev picks the opencode section and its fields', () => {
  const m = decodeModelsDev(MODELS_DEV);
  assert.ok(m.has('mimo-v2.6-flash-free'));
  assert.equal(m.get('mimo-v2.6-flash-free').contextWindow, 200000);
  assert.equal(m.get('mimo-v2.6-flash-free').input, 0, 'numeric cost stays numeric');
  assert.deepEqual(m.get('mimo-v2.6-flash-free').inputModalities, ['text', 'image']);
  assert.deepEqual(m.get('space-bunny-free').efforts, ['low', 'high']);
  assert.equal(m.get('paid-model').input, 3);
  assert.equal(m.has('x/y'), false, 'other providers must not leak in');
});

test('free decision: zero cost allows, paid denies', () => {
  const m = decodeModelsDev(MODELS_DEV);
  assert.equal(decide('mimo-v2.6-flash-free', m, true).allowed, true);
  assert.equal(decide('paid-model', m, true).allowed, false);
  assert.equal(decide('paid-model', m, true).source, 'metadata_paid');
});

test('deprecated metadata denies unless the id is compile-time verified', () => {
  const m = decodeModelsDev(MODELS_DEV);
  assert.equal(decide('dead-model', m, true).allowed, false);
  assert.equal(decide('dead-model', m, true).source, 'metadata_deprecated');
  assert.equal(decide('gone-model', m, true).allowed, false);
  // mimo-v2.5-free is a verified seed; stale metadata must not kill it.
  assert.ok(SEED_IDS.has('mimo-v2.5-free'));
  const withDeprecatedSeed = new Map(m);
  withDeprecatedSeed.set('mimo-v2.5-free', { input: 0, output: 0, deprecated: true });
  const d = decide('mimo-v2.5-free', withDeprecatedSeed, true);
  assert.equal(d.allowed, true);
  assert.equal(d.source, 'static_verified');
});

test('metadata pending falls back to seed/name, not to a blanket allow', () => {
  assert.equal(decide('mimo-v2.6-flash-free', new Map(), false).allowed, true);
  assert.equal(decide('some-unknown-model', new Map(), false).allowed, false);
  assert.equal(decide('brand-new-free', new Map(), false).allowed, true, 'name heuristic still allows *-free');
});

test('enrich overlays live metadata onto the seed entry', () => {
  const seed = ZEN_CHAT_MODELS_SEED.find((m) => m.id === 'mimo-v2.6-flash-free');
  const out = enrich(seed, { contextWindow: 123456, maxOutput: 777, inputModalities: ['text'], reasoning: true, efforts: ['low'] });
  assert.equal(out.contextWindow, 123456);
  assert.equal(out.maxOutput, 777);
  assert.deepEqual(out.input, ['text']);
  assert.deepEqual(out.efforts, ['low']);
  assert.equal(seed.contextWindow, 200000, 'seed must not be mutated');
  assert.deepEqual(seed.input, ['text', 'image', 'audio', 'video'], 'seed modalities survive when metadata is silent');
});

test('catalog lists S1 ∩ S2 and keeps seeds when the live list is empty', async () => {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(String(url));
    if (String(url).includes('/v1/models')) {
      return new Response(JSON.stringify({
        object: 'list',
        data: [
          { id: 'mimo-v2.6-flash-free' },
          { id: 'space-bunny-free' },
          { id: 'paid-model' },
          { id: 'dead-model' },
          { id: 'some-unlisted-free' },
        ],
      }), { status: 200, headers: { 'content-type': 'application/json' } });
    }
    return new Response(JSON.stringify(MODELS_DEV), { status: 200, headers: { 'content-type': 'application/json' } });
  };
  const c = new ZenCatalog({ fetchImpl, zenBase: 'https://zen.test', metadataUrl: 'https://md.test/api.json', now: () => 1000 });
  await c.refreshOnce();
  const ids = c.list().map((m) => m.id);
  assert.deepEqual(ids, ['mimo-v2.6-flash-free', 'some-unlisted-free', 'space-bunny-free']);
  assert.equal(ids.includes('paid-model'), false, 'paid model must not be exposed');
  assert.equal(ids.includes('dead-model'), false, 'deprecated model must not be exposed');
  const bunny = c.get('space-bunny-free');
  assert.equal(bunny.api, 'chat');
  assert.equal(c.snapshot().status, 'ready');
  assert.ok(calls.some((u) => u.includes('/v1/models')));
});

test('catalog survives a dead zen endpoint and reports the error', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('/v1/models')) return new Response('boom', { status: 500 });
    return new Response(JSON.stringify(MODELS_DEV), { status: 200 });
  };
  const c = new ZenCatalog({ fetchImpl, zenBase: 'https://zen.test', metadataUrl: 'https://md.test/api.json' });
  await c.refreshOnce();
  assert.match(c.lastError, /HTTP 500/);
  assert.equal(c.snapshot().status, 'pending');
  assert.ok(c.list().length >= 10, 'seeds keep the lane usable while the live list is down');
});

test('jev is not offered as a chat model', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('/v1/models')) {
      return new Response(JSON.stringify({ object: 'list', data: [{ id: 'jev-1.13-free' }, { id: 'big-pickle' }] }), { status: 200 });
    }
    return new Response(JSON.stringify(MODELS_DEV), { status: 200 });
  };
  const c = new ZenCatalog({ fetchImpl, zenBase: 'https://zen.test', metadataUrl: 'https://md.test/api.json' });
  await c.refreshOnce();
  const ids = c.list().map((m) => m.id);
  assert.ok(ids.includes('big-pickle'), 'chat models stay');
  assert.equal(ids.includes('jev-1.13-free'), false, 'jev is systemone-only, not a chat model');
});

test('every seed declares the fields the model list needs', () => {
  for (const m of ZEN_CHAT_MODELS_SEED) {
    assert.ok(m.id, 'id');
    assert.ok(Number.isSafeInteger(m.contextWindow) && m.contextWindow > 0, `${m.id} contextWindow`);
    assert.ok(Number.isSafeInteger(m.maxOutput) && m.maxOutput > 0, `${m.id} maxOutput`);
    assert.ok(Array.isArray(m.input) && m.input.length > 0, `${m.id} input`);
    assert.ok(m.api === 'chat' || m.api === 'responses', `${m.id} api`);
  }
  const muse = ZEN_CHAT_MODELS_SEED.find((m) => m.id === 'muse-spark-1.3-contributor-free');
  assert.equal(muse.api, 'responses', 'muse-spark is Responses-only on Zen');
});
