// server.mjs — zen-gateway: one base URL + one key in front of
//   (a) the existing wb2api panel (cn:*/global:* and bare names, byte-passthrough)
//   (b) the OpenCode Zen anonymous free lane (zen:* names, CLI disguise)
// Zero third-party runtime dependencies (Node >= 20 built-ins only).
import http from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { appendFile, mkdir } from 'node:fs/promises';
import { dirname } from 'node:path';

import { ZenCatalog, ZEN_CHAT_MODELS_SEED, JEV_MODEL } from './catalog.mjs';
import { deriveRequestIDs, disguiseHeaders } from './disguise.mjs';
import { ensureFreeLaneShape, reasoningEffortWire, applyReasoningEffort, openAIError } from './freelane.mjs';
import { newTranslateCtx, toResponsesBody, translateEvent } from './responses.mjs';
import { Gate } from './semaphore.mjs';
import { Breaker } from './breaker.mjs';
import { upstreamChat } from './forward.mjs';

export const ZEN_PREFIX = 'zen:';
const MAX_BODY_BYTES = 32 * 1024 * 1024;

export function defaults() {
  return {
    listen: '127.0.0.1:7864',
    api_keys: [],
    wb2api: { base: 'http://127.0.0.1:7863', api_key: '', models_ttl_ms: 600000 },
    zen: { base: 'https://opencode.ai/zen', catalog_ttl_ms: 600000, metadata_ttl_ms: 86400000, metadata_cache: '' },
    limits: {
      first_byte_ms: 30000,
      body_idle_ms: 120000,
      responses_body_idle_ms: 300000,
      max_in_flight_per_model: 3,
      max_in_flight_global: 8,
      upstream_timeout_ms: 120000,
      pre_content_retries: 1,
    },
    logging: { request_log: '', log_prompts: false, retention_days: 7 },
  };
}

export function mergeConfig(raw = {}) {
  const d = defaults();
  return {
    ...d,
    ...raw,
    wb2api: { ...d.wb2api, ...(raw.wb2api ?? {}) },
    zen: { ...d.zen, ...(raw.zen ?? {}) },
    limits: { ...d.limits, ...(raw.limits ?? {}) },
    logging: { ...d.logging, ...(raw.logging ?? {}) },
  };
}

export async function loadConfig(path) {
  if (!path) return mergeConfig({});
  const raw = JSON.parse(await readFile(path, 'utf8'));
  return mergeConfig(raw);
}

function safeEqual(a, b) {
  const ab = Buffer.from(String(a ?? ''));
  const bb = Buffer.from(String(b ?? ''));
  if (ab.length !== bb.length) return false;
  try { return timingSafeEqual(ab, bb); } catch { return false; }
}

function bearer(req) {
  const h = req.headers.authorization ?? '';
  const m = /^Bearer\s+(.+)$/i.exec(h.trim());
  return m ? m[1].trim() : '';
}

function dateStamp(fmt, now = new Date()) {
  const p = (n) => String(n).padStart(2, '0');
  return fmt
    .replace('%Y', String(now.getFullYear()))
    .replace('%m', p(now.getMonth() + 1))
    .replace('%d', p(now.getDate()));
}

function readBody(req, limit = MAX_BODY_BYTES) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > limit) {
        reject(Object.assign(new Error('request body too large'), { tooLarge: true }));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function json(res, status, body) {
  const text = JSON.stringify(body);
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

/** Create the gateway. `deps.fetch` is injectable for tests. */
export function createGateway(config, deps = {}) {
  const cfg = mergeConfig(config);
  const doFetch = deps.fetch ?? fetch;
  const logger = deps.logger ?? console;
  const now = deps.now ?? Date.now;

  const catalog = deps.catalog ?? new ZenCatalog({
    zenBase: cfg.zen.base,
    cachePath: cfg.zen.metadata_cache || undefined,
    refreshSeconds: Math.max(30, Math.round(cfg.zen.catalog_ttl_ms / 1000)),
    metadataMs: cfg.zen.metadata_ttl_ms,
    fetchImpl: doFetch,
    now,
    onRefresh: (snap, lastError) => {
      if (lastError) logger.warn?.(`[zen] catalog: ${lastError}`);
      lastCatalog = { ...snap, lastError };
    },
  });
  let lastCatalog = { status: 'pending', total: 0, exposed: 0, lastError: '' };
  const gate = new Gate(cfg.limits.max_in_flight_per_model, cfg.limits.max_in_flight_global);
  const breaker = deps.breaker ?? new Breaker();
  const stats = { requests: 0, zen_ok: 0, zen_fail: 0, wb2api_ok: 0, wb2api_fail: 0, by_model: {} };
  let wb2apiModels = { at: 0, data: [], error: '' };

  async function log(entry) {
    if (!cfg.logging.request_log) return;
    try {
      const path = dateStamp(cfg.logging.request_log);
      await mkdir(dirname(path), { recursive: true });
      await appendFile(path, `${JSON.stringify(entry)}\n`, 'utf8');
    } catch { /* logging must never break a request */ }
  }

  /** Panel model list (cached). Merge failures never break the zen half. */
  async function panelModels() {
    if (wb2apiModels.data.length > 0 && now() - wb2apiModels.at < cfg.wb2api.models_ttl_ms) return wb2apiModels;
    try {
      const headers = { accept: 'application/json' };
      if (cfg.wb2api.api_key) headers.authorization = `Bearer ${cfg.wb2api.api_key}`;
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 10000);
      try {
        const r = await doFetch(`${cfg.wb2api.base.replace(/\/+$/, '')}/v1/models`, { headers, signal: ctl.signal });
        if (!r.ok) throw new Error(`panel /v1/models HTTP ${r.status}`);
        const payload = await r.json();
        wb2apiModels = { at: now(), data: Array.isArray(payload?.data) ? payload.data : [], error: '' };
      } finally { clearTimeout(t); }
    } catch (e) {
      wb2apiModels = { ...wb2apiModels, error: e instanceof Error ? e.message : String(e) };
    }
    return wb2apiModels;
  }

  function zenModelsPayload() {
    const out = [];
    const seen = new Set();
    for (const m of catalog.list()) {
      const id = `${ZEN_PREFIX}${m.id}`;
      if (seen.has(id)) continue;
      seen.add(id);
      out.push({
        id,
        object: 'model',
        created: 1753600000,
        owned_by: 'zen-free',
        context_length: m.contextWindow,
        max_output_tokens: m.maxOutput,
        ...(m.input?.includes('image') ? { supports_images: true } : {}),
        ...(m.reasoning ? { supports_reasoning: true } : {}),
        ...(Array.isArray(m.efforts) && m.efforts.length > 0 ? { reasoning_effort: m.efforts } : {}),
      });
    }
    const jevId = `${ZEN_PREFIX}${JEV_MODEL}`;
    if (!seen.has(jevId)) {
      out.push({
        id: jevId,
        object: 'model',
        created: 1753600000,
        owned_by: 'zen-free',
        description: 'structured decision endpoint (/v1/systemone)',
      });
    }
    return out;
  }

  async function handleModels(res) {
    const panel = await panelModels();
    const data = [...panel.data, ...zenModelsPayload()];
    json(res, 200, { object: 'list', data });
  }

  /** Byte-passthrough to the panel: models, streaming, errors all untouched. */
  async function proxyToPanel(req, res, rawBody, path, startedAt) {
    const headers = {};
    for (const [k, v] of Object.entries(req.headers)) {
      if (['host', 'connection', 'content-length', 'authorization', 'accept-encoding'].includes(k.toLowerCase())) continue;
      if (typeof v === 'string') headers[k] = v;
      else if (Array.isArray(v)) headers[k] = v.join(', ');
    }
    headers['content-type'] = 'application/json';
    if (cfg.wb2api.api_key) headers.authorization = `Bearer ${cfg.wb2api.api_key}`;
    const ctl = new AbortController();
    const onAbort = () => ctl.abort();
    req.on('aborted', onAbort);
    try {
      const upstream = await doFetch(`${cfg.wb2api.base.replace(/\/+$/, '')}${path}`, {
        method: req.method, headers, body: rawBody.length ? rawBody : undefined, signal: ctl.signal,
      });
      const outHeaders = {};
      for (const [k, v] of upstream.headers.entries()) {
        if (['content-encoding', 'transfer-encoding', 'connection', 'content-length'].includes(k.toLowerCase())) continue;
        outHeaders[k] = v;
      }
      res.writeHead(upstream.status, outHeaders);
      if (upstream.body) {
        for await (const chunk of upstream.body) {
          if (!res.write(chunk)) await new Promise((r) => res.once('drain', r));
        }
      }
      res.end();
      stats.wb2api_ok += 1;
      await log({ t: new Date().toISOString(), lane: 'panel', path, status: upstream.status, ms: now() - startedAt });
      return upstream.status;
    } catch (e) {
      stats.wb2api_fail += 1;
      const msg = e instanceof Error ? e.message : String(e);
      const mapped = openAIError(503, 'panel_unreachable', `wb2api panel request failed: ${msg}`);
      if (!res.headersSent) { json(res, mapped.http, mapped.body); }
      else { try { res.end(); } catch { /* closed */ } }
      await log({ t: new Date().toISOString(), lane: 'panel', path, status: 503, error: msg, ms: now() - startedAt });
      return 503;
    } finally {
      req.off('aborted', onAbort);
    }
  }

  /** Zen chat lane (chat-completions, or Responses for muse-spark-*). */
  async function handleZenChat(req, res, body, zenModel, startedAt) {
    const isStream = body.stream !== false;
    const chatBody = { ...body, model: zenModel, stream: isStream };
    const meta = catalog.get(zenModel);
    const entry = meta ?? ZEN_CHAT_MODELS_SEED.find((m) => m.id === zenModel);
    if (!entry) {
      // Unknown zen:* name — still allow (upstream is the authority), but no metadata.
      if (!catalog.decision(zenModel).allowed) {
        const d = catalog.decision(zenModel);
        json(res, 404, { error: { message: `model "${ZEN_PREFIX}${zenModel}" is not an available free model (${d.source})`, type: 'invalid_request_error', code: 'model_not_found' } });
        return 404;
      }
    }
    const isResponses = entry?.api === 'responses' || zenModel.toLowerCase().startsWith('muse-spark');

    const cooldown = breaker.blockedMs(zenModel, now());
    if (cooldown > 0) {
      res.writeHead(429, { 'content-type': 'application/json', 'retry-after': String(Math.ceil(cooldown / 1000)) });
      res.end(JSON.stringify({ error: { message: `zen model cooling down after rate limit (${Math.ceil(cooldown / 1000)}s)`, type: 'rate_limit_exceeded', code: 'rate_limit_exceeded' } }));
      return 429;
    }

    let release;
    try {
      release = await gate.acquire(zenModel, req.signal);
    } catch (e) {
      if (e?.aborted) { try { res.end(); } catch { /* closed */ } return 499; }
      throw e;
    }

    const ids = deriveRequestIDs(body.messages ?? []);
    const headers = disguiseHeaders(ids);
    const effortId = typeof body.reasoning_effort === 'string' ? body.reasoning_effort : undefined;
    const effortWire = reasoningEffortWire(effortId);
    const base = cfg.zen.base.replace(/\/+$/, '');
    const limits = {
      firstByteMs: cfg.limits.first_byte_ms,
      bodyIdleMs: isResponses ? Math.max(cfg.limits.body_idle_ms, cfg.limits.responses_body_idle_ms) : cfg.limits.body_idle_ms,
      upstreamTimeoutMs: cfg.limits.upstream_timeout_ms,
      preContentRetries: cfg.limits.pre_content_retries,
      clientSignal: req.signal,
      fetchImpl: doFetch,
    };

    let result;
    try {
      if (isResponses) {
        const rbody = toResponsesBody(chatBody, zenModel, effortWire);
        result = await upstreamChat(`${base}/v1/responses`, headers, rbody, res, {
          ...limits,
          translate: translateEvent,
          translateCtx: newTranslateCtx(zenModel),
        });
      } else {
        let payload = ensureFreeLaneShape(chatBody);
        payload = applyReasoningEffort(payload, zenModel, effortWire, entry?.efforts);
        result = await upstreamChat(`${base}/v1/chat/completions`, headers, payload, res, limits);
      }
    } finally {
      release?.();
    }

    if (result.ok) {
      stats.zen_ok += 1;
      breaker.markOk(zenModel);
      if (isStream || result.deliveredContent) {
        const u = result.usage ?? {};
        stats.by_model[zenModel] = stats.by_model[zenModel] ?? { ok: 0, fail: 0, prompt: 0, completion: 0 };
        stats.by_model[zenModel].ok += 1;
        stats.by_model[zenModel].prompt += u.prompt_tokens ?? 0;
        stats.by_model[zenModel].completion += u.completion_tokens ?? 0;
      }
    } else {
      stats.zen_fail += 1;
      stats.by_model[zenModel] = stats.by_model[zenModel] ?? { ok: 0, fail: 0, prompt: 0, completion: 0 };
      stats.by_model[zenModel].fail += 1;
      if (result.http === 429) {
        const ms = breaker.markLimited(zenModel, now());
        logger.warn?.(`[zen] ${zenModel} rate limited; cooling ${Math.round(ms / 1000)}s`);
      } else if (result.http === 403) {
        // Region block is a property of the exit IP + model, not a transient
        // condition: cool it for the same bounded window to stop hammering.
        breaker.markLimited(zenModel, now());
      }
    }
    await log({
      t: new Date().toISOString(), lane: 'zen', model: zenModel, api: isResponses ? 'responses' : 'chat',
      stream: !!isStream, status: result.ok ? 200 : result.http, ms: now() - startedAt,
      usage: result.usage ?? null, error: result.ok ? undefined : result.message,
      ...(cfg.logging.log_prompts ? { prompt: body.messages } : {}),
    });
    return result.ok ? 200 : result.http;
  }

  /** jev: structured decision endpoint, verbatim passthrough. */
  async function handleSystemOne(req, res, body, startedAt) {
    const raw = String(body.model ?? '');
    const model = raw.startsWith(ZEN_PREFIX) ? raw.slice(ZEN_PREFIX.length) : raw;
    if (model !== JEV_MODEL) {
      json(res, 400, { error: { message: `/v1/systemone only serves "${JEV_MODEL}"`, type: 'invalid_request_error', code: 'model_not_found' } });
      return 400;
    }
    const payload = { ...body, model };
    if (!payload.questions || typeof payload.questions !== 'object') {
      json(res, 400, { error: { message: 'systemone requires a "questions" object', type: 'invalid_request_error', code: 'invalid_request' } });
      return 400;
    }
    const ids = deriveRequestIDs([{ role: 'user', content: JSON.stringify(payload.state ?? '') }]);
    const headers = disguiseHeaders(ids);
    delete headers.accept;
    headers.accept = 'application/json';
    const base = cfg.zen.base.replace(/\/+$/, '');
    const ctl = new AbortController();
    const t = setTimeout(() => ctl.abort(), cfg.limits.upstream_timeout_ms);
    try {
      const r = await doFetch(`${base}/v1/systemone`, { method: 'POST', headers, body: JSON.stringify(payload), signal: ctl.signal });
      const text = await r.text();
      if (!r.ok) {
        json(res, r.status === 429 ? 429 : 503, { error: { message: text.slice(0, 500) || `systemone HTTP ${r.status}`, type: 'zen_upstream', code: 'zen_upstream' } });
        await log({ t: new Date().toISOString(), lane: 'systemone', model, status: r.status, ms: now() - startedAt });
        return r.status;
      }
      res.writeHead(200, { 'content-type': 'application/json; charset=utf-8' });
      res.end(text);
      stats.zen_ok += 1;
      await log({ t: new Date().toISOString(), lane: 'systemone', model, status: 200, ms: now() - startedAt });
      return 200;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      json(res, 503, { error: { message: `systemone request failed: ${msg}`, type: 'zen_upstream', code: 'zen_upstream' } });
      await log({ t: new Date().toISOString(), lane: 'systemone', model, status: 503, error: msg, ms: now() - startedAt });
      return 503;
    } finally { clearTimeout(t); }
  }

  const server = http.createServer(async (req, res) => {
    const startedAt = now();
    stats.requests += 1;
    let path = (req.url ?? '/').split('?')[0];
    // Tolerate both mounted (/zen/...) and stripped (/...) deployments.
    if (path === '/zen') path = '/';
    else if (path.startsWith('/zen/')) path = path.slice(4);

    try {
      if (path === '/healthz') { json(res, 200, { status: 'ok' }); return; }

      // Auth: gateway keys only. The panel key never leaves this process.
      const key = bearer(req);
      const authorized = cfg.api_keys.length > 0 && cfg.api_keys.some((k) => safeEqual(k, key));
      if (path === '/status') {
        if (!authorized) { json(res, 401, { error: { message: 'missing or invalid gateway api key', type: 'invalid_request_error', code: 'invalid_api_key' } }); return; }
        const panel = await panelModels();
        json(res, 200, {
          gateway: { uptime_s: Math.round((now() - startTime) / 1000), requests: stats.requests },
          zen: { ...lastCatalog, breaker: breaker.snapshot(now()), models: catalog.list().map((m) => m.id) },
          panel: { base: cfg.wb2api.base, models: panel.data.length, error: panel.error, cached_age_ms: panel.at ? now() - panel.at : null },
          inflight: gate.stats(),
          stats: { zen_ok: stats.zen_ok, zen_fail: stats.zen_fail, wb2api_ok: stats.wb2api_ok, wb2api_fail: stats.wb2api_fail, by_model: stats.by_model },
        });
        return;
      }

      if (!authorized) {
        json(res, 401, { error: { message: 'missing or invalid gateway api key', type: 'invalid_request_error', code: 'invalid_api_key' } });
        return;
      }

      if (path === '/v1/models' && req.method === 'GET') { await handleModels(res); return; }

      if (path === '/v1/chat/completions' && req.method === 'POST') {
        let raw;
        try { raw = await readBody(req); } catch (e) {
          if (e?.tooLarge) { json(res, 413, { error: { message: 'request body too large', type: 'invalid_request_error', code: 'invalid_request' } }); return; }
          throw e;
        }
        let body;
        try { body = JSON.parse(raw.toString('utf8')); } catch {
          json(res, 400, { error: { message: 'request body is not valid JSON', type: 'invalid_request_error', code: 'invalid_request' } });
          return;
        }
        const model = String(body.model ?? '');
        if (model.startsWith(ZEN_PREFIX)) {
          await handleZenChat(req, res, body, model.slice(ZEN_PREFIX.length), startedAt);
        } else {
          await proxyToPanel(req, res, raw, '/v1/chat/completions', startedAt);
        }
        return;
      }

      if (path === '/v1/systemone' && req.method === 'POST') {
        let raw;
        try { raw = await readBody(req); } catch (e) {
          if (e?.tooLarge) { json(res, 413, { error: { message: 'request body too large', type: 'invalid_request_error', code: 'invalid_request' } }); return; }
          throw e;
        }
        let body;
        try { body = JSON.parse(raw.toString('utf8')); } catch {
          json(res, 400, { error: { message: 'request body is not valid JSON', type: 'invalid_request_error', code: 'invalid_request' } });
          return;
        }
        await handleSystemOne(req, res, body, startedAt);
        return;
      }

      json(res, 404, { error: { message: `unknown route ${req.method} ${path}`, type: 'invalid_request_error', code: 'not_found' } });
    } catch (e) {
      logger.error?.('[zen-gateway] unhandled', e);
      if (!res.headersSent) json(res, 500, { error: { message: 'gateway internal error', type: 'server_error', code: 'internal_error' } });
      else { try { res.end(); } catch { /* closed */ } }
    }
  });

  const startTime = now();
  return {
    server, catalog, gate, breaker, cfg,
    async listen() {
      const [host, port] = String(cfg.listen).split(':');
      await new Promise((resolve) => server.listen(Number(port), host || '127.0.0.1', resolve));
      return server.address();
    },
    async close() {
      catalog.stop();
      await new Promise((resolve) => server.close(resolve));
    },
    address: () => server.address(),
  };
}

export async function main(argv = process.argv.slice(2)) {
  const idx = argv.indexOf('--config');
  const path = idx >= 0 ? argv[idx + 1] : process.env.ZEN_GATEWAY_CONFIG;
  const cfg = await loadConfig(path);
  if (!cfg.api_keys.length) {
    console.error('[zen-gateway] refusing to start: no api_keys configured');
    process.exit(2);
  }
  const gw = createGateway(cfg);
  const addr = await gw.listen();
  console.log(`[zen-gateway] listening on ${addr.address}:${addr.port}`);
  void gw.catalog.start().catch((e) => console.error('[zen-gateway] catalog start failed', e));
  const shutdown = async (sig) => {
    console.log(`[zen-gateway] ${sig} -> shutting down`);
    try { await gw.close(); } catch { /* ignore */ }
    process.exit(0);
  };
  process.on('SIGTERM', () => void shutdown('SIGTERM'));
  process.on('SIGINT', () => void shutdown('SIGINT'));
  return gw;
}

const isDirect = process.argv[1] && import.meta.url.endsWith(process.argv[1].replace(/\\/g, '/').split('/').pop());
if (isDirect) {
  main().catch((e) => { console.error('[zen-gateway] fatal', e); process.exit(1); });
}
