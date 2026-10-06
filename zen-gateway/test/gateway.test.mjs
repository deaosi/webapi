// End-to-end gateway tests with a stubbed upstream (no network).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createGateway } from '../src/server.mjs';

const GW_KEY = 'gw-test-key';
const PANEL_KEY = 'sk-wb2api-panel-key';

function sseResponse(chunks) {
  const enc = new TextEncoder();
  const body = new ReadableStream({
    start(c) {
      for (const s of chunks) c.enqueue(enc.encode(s));
      c.close();
    },
  });
  return new Response(body, { status: 200, headers: { 'content-type': 'text/event-stream' } });
}

function catalogStub(models = [
  { id: 'mimo-v2.6-flash-free', contextWindow: 200000, maxOutput: 32000, input: ['text', 'image'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'muse-spark-1.3-contributor-free', contextWindow: 1048576, maxOutput: 131072, input: ['text', 'image'], reasoning: true, efforts: ['minimal', 'low', 'high'], api: 'responses' },
]) {
  return {
    list: () => models,
    get: (id) => models.find((m) => m.id === id),
    decision: () => ({ allowed: true, source: 'test' }),
    snapshot: () => ({ status: 'ready', total: models.length, exposed: models.length }),
    start: async () => {},
    stop: () => {},
  };
}

async function startGateway({ fetchImpl, limits, catalog } = {}) {
  const gw = createGateway({
    listen: '127.0.0.1:0',
    api_keys: [GW_KEY],
    wb2api: { base: 'http://panel.test', api_key: PANEL_KEY },
    zen: { base: 'https://zen.test' },
    logging: { request_log: '' },
    limits: { first_byte_ms: 2000, body_idle_ms: 3000, upstream_timeout_ms: 5000, pre_content_retries: 0, ...(limits ?? {}) },
  }, {
    fetch: fetchImpl,
    catalog: catalog ?? catalogStub(),
    logger: { warn() {}, error() {}, info() {} },
  });
  const addr = await gw.listen();
  return { gw, base: `http://127.0.0.1:${addr.port}` };
}

function authed(base, path, init = {}) {
  return fetch(`${base}${path}`, {
    ...init,
    headers: { authorization: `Bearer ${GW_KEY}`, 'content-type': 'application/json', ...(init.headers ?? {}) },
  });
}

test('unauthenticated requests are rejected', async () => {
  const { gw, base } = await startGateway({ fetchImpl: async () => new Response('{}', { status: 200 }) });
  try {
    const r = await fetch(`${base}/v1/models`);
    assert.equal(r.status, 401);
    const body = await r.json();
    assert.equal(body.error.code, 'invalid_api_key');
    const wrong = await fetch(`${base}/v1/models`, { headers: { authorization: 'Bearer nope' } });
    assert.equal(wrong.status, 401);
  } finally { await gw.close(); }
});

test('healthz is open, status requires the key', async () => {
  const { gw, base } = await startGateway({ fetchImpl: async () => new Response('{}', { status: 200 }) });
  try {
    assert.equal((await fetch(`${base}/healthz`)).status, 200);
    assert.equal((await fetch(`${base}/status`)).status, 401);
    const r = await authed(base, '/status');
    assert.equal(r.status, 200);
    const s = await r.json();
    assert.ok(Array.isArray(s.zen.models));
    assert.ok(s.inflight.global);
  } finally { await gw.close(); }
});

test('model list merges panel models and zen free models', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('panel.test/v1/models')) {
      return new Response(JSON.stringify({
        object: 'list',
        data: [{ id: 'cn:deepseek-v4.1-flash', object: 'model', owned_by: 'workbuddy' }],
      }), { status: 200, headers: { 'content-type': 'application/json' } });
    }
    throw new Error(`unexpected ${url}`);
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/models');
    assert.equal(r.status, 200);
    const ids = (await r.json()).data.map((m) => m.id);
    assert.ok(ids.includes('cn:deepseek-v4.1-flash'), 'panel models pass through with their names');
    assert.ok(ids.includes('zen:mimo-v2.6-flash-free'));
    assert.ok(ids.includes('zen:muse-spark-1.3-contributor-free'));
    assert.ok(ids.includes('zen:jev-1.13-free'), 'the systemone model is discoverable');
    assert.equal(ids.filter((id) => id === 'zen:jev-1.13-free').length, 1, 'jev must be listed exactly once');
    assert.equal(new Set(ids).size, ids.length, 'no duplicate ids anywhere in the merged list');
  } finally { await gw.close(); }
});

test('zen chat requests carry the disguise and stream back unchanged', async () => {
  const seen = {};
  const fetchImpl = async (url, init) => {
    seen.url = String(url);
    seen.headers = init.headers;
    seen.body = JSON.parse(init.body);
    return sseResponse([
      'data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}\n\n',
      'data: {"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}\n\n',
      'data: [DONE]\n\n',
    ]);
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }], stream: true }),
    });
    assert.equal(r.status, 200);
    const text = await r.text();
    assert.match(text, /"content":"hi"/);
    assert.match(text, /data: \[DONE\]/);
    assert.equal(seen.url, 'https://zen.test/v1/chat/completions');
    assert.equal(seen.headers.authorization, 'Bearer public');
    assert.match(seen.headers['user-agent'], /^opencode\//);
    assert.equal(seen.headers['x-opencode-client'], 'cli');
    assert.match(seen.headers['x-opencode-session'], /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
    assert.equal(seen.body.model, 'mimo-v2.6-flash-free', 'the zen: prefix is stripped before dispatch');
    assert.equal(seen.body.stream, true);
    const names = seen.body.tools.map((t) => t.function.name);
    assert.ok(names.includes('bash') && names.includes('read'), 'free-lane gate tools injected');
    assert.equal(seen.body.tool_choice, 'none');
  } finally { await gw.close(); }
});

test('panel models are proxied byte-for-byte with the panel key, never the gateway key', async () => {
  const seen = {};
  const fetchImpl = async (url, init) => {
    seen.url = String(url);
    seen.headers = init.headers;
    seen.body = init.body ? JSON.parse(init.body) : null;
    return sseResponse(['data: {"choices":[{"delta":{"content":"panel"}}]}\n\n', 'data: [DONE]\n\n']);
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'cn:deepseek-v4.1-flash', messages: [{ role: 'user', content: 'hi' }], stream: true }),
    });
    assert.equal(r.status, 200);
    assert.match(await r.text(), /panel/);
    assert.equal(seen.url, 'http://panel.test/v1/chat/completions');
    assert.equal(seen.headers.authorization, `Bearer ${PANEL_KEY}`, 'panel key is injected server-side');
    assert.notEqual(seen.headers.authorization, `Bearer ${GW_KEY}`, 'the gateway key must not leak upstream');
    assert.equal(seen.body.model, 'cn:deepseek-v4.1-flash', 'panel model names are untouched');
  } finally { await gw.close(); }
});

test('muse-spark goes to /v1/responses and comes back as chat chunks', async () => {
  const seen = {};
  const fetchImpl = async (url, init) => {
    seen.url = String(url);
    seen.body = JSON.parse(init.body);
    return sseResponse([
      'event: response.created\ndata: {"type":"response.created"}\n\n',
      'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"Hel"}\n\n',
      'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"lo"}\n\n',
      'event: response.completed\ndata: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}}\n\n',
    ]);
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:muse-spark-1.3-contributor-free', messages: [{ role: 'user', content: 'hi' }], stream: true }),
    });
    assert.equal(r.status, 200);
    const text = await r.text();
    const payloads = text.split('\n\n').filter((b) => b.startsWith('data: ') && !b.includes('[DONE]'))
      .map((b) => JSON.parse(b.slice(6)));
    const content = payloads.map((p) => p.choices?.[0]?.delta?.content ?? '').join('');
    assert.equal(content, 'Hello');
    assert.equal(payloads[0].object, 'chat.completion.chunk');
    assert.equal(payloads.at(-1).choices[0].finish_reason, 'stop');
    assert.match(text, /data: \[DONE\]/);
    assert.equal(seen.url, 'https://zen.test/v1/responses');
    assert.equal(seen.body.input[0].content[0].text, 'hi');
    assert.equal(seen.body.tool_choice, 'auto');
  } finally { await gw.close(); }
});

test('zen 429 becomes a 429 with Retry-After and cools the model', async () => {
  let calls = 0;
  const fetchImpl = async () => {
    calls += 1;
    return new Response('{"error":"rate limited"}', { status: 429, headers: { 'retry-after': '17' } });
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(r.status, 429);
    assert.equal(r.headers.get('retry-after'), '17', "upstream's own hint is forwarded");
    assert.equal((await r.json()).error.code, 'rate_limit_exceeded');
    const callsAfterFirst = calls;
    const r2 = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(r2.status, 429);
    assert.ok(Number(r2.headers.get('retry-after')) > 0, 'the breaker advertises its own cooldown');
    assert.equal(calls, callsAfterFirst, 'the breaker must absorb the second request instead of hammering upstream');
  } finally { await gw.close(); }
});

test('region block is reported as 403, not as an auth failure', async () => {
  const fetchImpl = async () => new Response(JSON.stringify({
    type: 'error', error: { type: 'RegionError', message: 'This model is not available in your country.' },
  }), { status: 403 });
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:muse-spark-1.3-contributor-free', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(r.status, 403);
    const body = await r.json();
    assert.equal(body.error.code, 'region_blocked');
    assert.match(body.error.message, /not available in your country/);
  } finally { await gw.close(); }
});

test('jev systemone passes through on its own endpoint', async () => {
  const seen = {};
  const fetchImpl = async (url, init) => {
    seen.url = String(url);
    seen.headers = init.headers;
    seen.body = JSON.parse(init.body);
    return new Response(JSON.stringify({ model: 'jev-1.13-free', answers: { is_urgent: { type: 'noul', noul: 0.78 } } }), {
      status: 200, headers: { 'content-type': 'application/json' },
    });
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/systemone', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:jev-1.13-free', state: 'broken', questions: { is_urgent: { type: 'noul', instructions: 'urgent?' } } }),
    });
    assert.equal(r.status, 200);
    const body = await r.json();
    assert.equal(body.answers.is_urgent.noul, 0.78);
    assert.equal(seen.url, 'https://zen.test/v1/systemone');
    assert.equal(seen.body.model, 'jev-1.13-free', 'prefix stripped');
    assert.equal(seen.headers.authorization, 'Bearer public');
  } finally { await gw.close(); }
});

test('systemone rejects other models and malformed bodies', async () => {
  const { gw, base } = await startGateway({ fetchImpl: async () => new Response('{}', { status: 200 }) });
  try {
    const bad = await authed(base, '/v1/systemone', { method: 'POST', body: JSON.stringify({ model: 'zen:big-pickle', questions: {} }) });
    assert.equal(bad.status, 400);
    const noQuestions = await authed(base, '/v1/systemone', { method: 'POST', body: JSON.stringify({ model: 'zen:jev-1.13-free' }) });
    assert.equal(noQuestions.status, 400);
  } finally { await gw.close(); }
});

test('malformed json and unknown routes are handled, not crashed', async () => {
  const { gw, base } = await startGateway({ fetchImpl: async () => new Response('{}', { status: 200 }) });
  try {
    const bad = await authed(base, '/v1/chat/completions', { method: 'POST', body: '{not json' });
    assert.equal(bad.status, 400);
    assert.equal((await bad.json()).error.code, 'invalid_request');
    const nf = await authed(base, '/v1/nope');
    assert.equal(nf.status, 404);
  } finally { await gw.close(); }
});

test('a dead panel surfaces as 503 without breaking the zen lane', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('panel.test')) throw new Error('ECONNREFUSED');
    return sseResponse(['data: {"choices":[{"delta":{"content":"zen"}}]}\n\n', 'data: [DONE]\n\n']);
  };
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST', body: JSON.stringify({ model: 'cn:deepseek-v4.1-flash', messages: [] }),
    });
    assert.equal(r.status, 503);
    assert.equal((await r.json()).error.code, 'panel_unreachable');
    const zen = await authed(base, '/v1/chat/completions', {
      method: 'POST', body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(zen.status, 200, 'zen lane is independent of the panel');
    assert.match(await zen.text(), /zen/);
  } finally { await gw.close(); }
});

test('the mounted /zen/... path is accepted as well as the stripped one', async () => {
  const fetchImpl = async () => new Response(JSON.stringify({ object: 'list', data: [] }), { status: 200, headers: { 'content-type': 'application/json' } });
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/zen/v1/models');
    assert.equal(r.status, 200);
  } finally { await gw.close(); }
});

test('usage is accounted even when the payload nests token details', async () => {
  const fetchImpl = async () => sseResponse([
    'data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}\n\n',
    // Nested details are the real Zen shape; a naive regex truncates here.
    'data: {"id":"1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":106,"completion_tokens":8,"total_tokens":114,"prompt_tokens_details":{"audio_tokens":0,"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":10}}}\n\n',
    'data: [DONE]\n\n',
  ]);
  const { gw, base } = await startGateway({ fetchImpl });
  try {
    const r = await authed(base, '/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }], stream: true }),
    });
    assert.equal(r.status, 200);
    await r.text();
    const s = await (await authed(base, '/status')).json();
    const m = s.stats.by_model['mimo-v2.6-flash-free'];
    assert.equal(m.ok, 1);
    assert.equal(m.prompt, 106, 'prompt tokens recorded despite nested details');
    assert.equal(m.completion, 8);
  } finally { await gw.close(); }
});

test('concurrency cap queues instead of failing or hammering upstream', async () => {
  let inflight = 0;
  let peak = 0;
  const fetchImpl = async () => {
    inflight += 1;
    peak = Math.max(peak, inflight);
    await new Promise((r) => setTimeout(r, 60));
    inflight -= 1;
    return sseResponse(['data: {"choices":[{"delta":{"content":"x"}}]}\n\n', 'data: [DONE]\n\n']);
  };
  const { gw, base } = await startGateway({ fetchImpl, limits: { max_in_flight_per_model: 1, max_in_flight_global: 2 } });
  try {
    const rs = await Promise.all([1, 2, 3, 4].map(() => authed(base, '/v1/chat/completions', {
      method: 'POST', body: JSON.stringify({ model: 'zen:mimo-v2.6-flash-free', messages: [{ role: 'user', content: 'hi' }] }),
    })));
    for (const r of rs) assert.equal(r.status, 200);
    await Promise.all(rs.map((r) => r.text()));
    assert.ok(peak <= 2, `upstream concurrency must stay capped (peak=${peak})`);
  } finally { await gw.close(); }
});
