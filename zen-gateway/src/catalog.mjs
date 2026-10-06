// catalog.mjs — Zen free-model directory: S1 live list ∩ S2 models.dev
// free decision, S3 static seeds + disk cache. Pure logic + injectable fetch
// so the whole chain is unit-testable offline.
export const ZEN_CHAT_MODELS_SEED = [
  { id: 'big-pickle', contextWindow: 200000, maxOutput: 32000, input: ['text'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'space-bunny-free', contextWindow: 1048576, maxOutput: 524288, input: ['text', 'image', 'video'], reasoning: true, efforts: ['low', 'medium', 'high', 'xhigh', 'max'], api: 'chat' },
  { id: 'longcat-2.5-preview-free', contextWindow: 1000000, maxOutput: 131072, input: ['text', 'image'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'fledge-alpha-free', contextWindow: 1048576, maxOutput: 131072, input: ['text', 'image'], reasoning: true, efforts: ['low', 'high', 'max'], api: 'chat' },
  { id: 'mimo-v2.6-flash-free', contextWindow: 200000, maxOutput: 32000, input: ['text', 'image', 'audio', 'video'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'mimo-v2.5-free', contextWindow: 200000, maxOutput: 32000, input: ['text', 'image', 'audio', 'video'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'ling-3.1-flash-free', contextWindow: 262144, maxOutput: 32768, input: ['text'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'ling-3.0-flash-fin-free', contextWindow: 262144, maxOutput: 32768, input: ['text'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'nemotron-3-ultra-free', contextWindow: 1000000, maxOutput: 128000, input: ['text'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'nemotron-3.5-lightning-free', contextWindow: 262144, maxOutput: 262144, input: ['text'], reasoning: true, efforts: [], api: 'chat' },
  { id: 'muse-spark-1.3-contributor-free', contextWindow: 1048576, maxOutput: 131072, input: ['text', 'image', 'video', 'pdf', 'audio'], reasoning: true, efforts: ['minimal', 'low', 'medium', 'high', 'xhigh'], api: 'responses' },
];
export const SEED_IDS = new Set(ZEN_CHAT_MODELS_SEED.map((m) => m.id));
export const JEV_MODEL = 'jev-1.13-free';

export function isFreeName(id) {
  return String(id ?? '').toLowerCase().includes('free');
}

/** Decode the opencode section of models.dev into id -> price record. */
export function decodeModelsDev(data) {
  const out = new Map();
  if (!data || typeof data !== 'object') return out;
  const keys = Object.keys(data);
  const rank = (k) => {
    const l = k.toLowerCase();
    if (l === 'opencode' || l === 'opencode-zen' || l === 'opencode_zen') return 0;
    return l.includes('opencode') ? 1 : 2;
  };
  keys.sort((a, b) => rank(a) - rank(b) || (a < b ? -1 : 1));
  for (const k of keys) {
    if (rank(k) > 1) continue;
    const models = data[k]?.models;
    if (!models || typeof models !== 'object') continue;
    for (const [mk, raw] of Object.entries(models)) {
      if (!raw || typeof raw !== 'object') continue;
      const mid = typeof raw.id === 'string' && raw.id ? raw.id : mk;
      const cost = raw.cost ?? {};
      const limit = raw.limit ?? {};
      const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);
      const status = String(raw.status ?? raw.lifecycle ?? '').toLowerCase();
      const deprecated = raw.deprecated === true || status === 'deprecated'
        || status === 'retired' || status === 'disabled'
        || raw.deprecated_at != null || raw.retirement_date != null;
      const mods = Array.isArray(raw.modalities?.input)
        ? raw.modalities.input.filter((x) => typeof x === 'string' && x)
        : undefined;
      const efforts = [];
      if (Array.isArray(raw.reasoning_options)) {
        for (const o of raw.reasoning_options) {
          if (o && typeof o === 'object' && o.type === 'effort' && Array.isArray(o.values)) {
            for (const v of o.values) {
              if (typeof v === 'string' && v && !efforts.includes(v)) efforts.push(v);
            }
          }
        }
      }
      out.set(mid, {
        input: num(cost.input), output: num(cost.output), deprecated,
        reasoning: raw.reasoning === true, efforts,
        contextWindow: num(limit.context), maxOutput: num(limit.output),
        // NB: separate from the numeric `input` cost above — models.dev reuses
        // the key name "input" for two different things.
        ...(mods?.length ? { inputModalities: mods } : {}),
      });
    }
    if (out.size > 0) return out;
  }
  return out;
}

/** Free decision for one id. Metadata verdict wins; seeds vouch for ids the
 *  metadata cannot speak for (pending/missing) — and for compile-time verified
 *  ids even when stale metadata claims deprecation. */
export function decide(id, prices, ready) {
  const nameFree = isFreeName(id);
  if (!ready || prices.size === 0) {
    return nameFree || SEED_IDS.has(id)
      ? { allowed: true, source: 'seed_or_name' }
      : { allowed: false, source: 'metadata_pending' };
  }
  const p = prices.get(id);
  if (!p) {
    return nameFree || SEED_IDS.has(id)
      ? { allowed: true, source: SEED_IDS.has(id) ? 'static_verified' : 'name_free' }
      : { allowed: false, source: 'metadata_model_missing' };
  }
  if (p.deprecated) {
    return SEED_IDS.has(id)
      ? { allowed: true, source: 'static_verified' }
      : { allowed: false, source: 'metadata_deprecated' };
  }
  if (p.input === 0 && p.output === 0) {
    return { allowed: true, source: nameFree ? 'name_and_metadata_free' : 'metadata_free' };
  }
  if (p.input === undefined || p.output === undefined) {
    return SEED_IDS.has(id)
      ? { allowed: true, source: 'static_verified' }
      : { allowed: false, source: 'metadata_cost_unknown' };
  }
  return { allowed: false, source: 'metadata_paid' };
}

/** Merge a seed entry with live metadata (limits/modalities/efforts). */
export function enrich(seed, price) {
  if (!price) return { ...seed };
  const next = { ...seed };
  if (Number.isSafeInteger(price.contextWindow) && price.contextWindow > 0) {
    next.contextWindow = price.contextWindow;
  }
  if (Number.isSafeInteger(price.maxOutput) && price.maxOutput > 0) {
    next.maxOutput = price.maxOutput;
  }
  if (Array.isArray(price.inputModalities) && price.inputModalities.length > 0) {
    next.input = [...price.inputModalities];
  }
  if (price.reasoning === true) next.reasoning = true;
  if (Array.isArray(price.efforts) && price.efforts.length > 0) next.efforts = [...price.efforts];
  return next;
}

export class ZenCatalog {
  #zenBase; #metadataUrl; #fetch; #now; #cachePath;
  #zen = new Set(); #updatedAt = 0;
  #prices = new Map(); #pricesReady = false;
  #lastError = '';
  #refreshSeconds; #metadataMs; #timer = null; #stopped = false;
  #onRefresh;

  constructor(opts = {}) {
    this.#zenBase = (opts.zenBase ?? 'https://opencode.ai/zen').replace(/\/+$/, '');
    this.#metadataUrl = opts.metadataUrl ?? 'https://models.dev/api.json';
    this.#fetch = opts.fetchImpl ?? fetch;
    this.#now = opts.now ?? Date.now;
    this.#cachePath = opts.cachePath;
    this.#refreshSeconds = opts.refreshSeconds ?? 600;
    this.#metadataMs = opts.metadataMs ?? 24 * 3600 * 1000;
    this.#onRefresh = opts.onRefresh;
  }

  async start() {
    await this.refreshOnce();
    let n = 0;
    while (this.#zen.size === 0 && n < 4 && !this.#stopped) {
      n += 1;
      await new Promise((r) => setTimeout(r, 15000));
      if (this.#stopped) return;
      await this.refreshOnce();
    }
    if (this.#stopped) return;
    this.#timer = setInterval(() => void this.refreshOnce(), this.#refreshSeconds * 1000);
    this.#timer.unref?.();
  }

  stop() {
    this.#stopped = true;
    if (this.#timer) { clearInterval(this.#timer); this.#timer = null; }
  }

  async refreshOnce() {
    await Promise.allSettled([this.#refreshZen(), this.#refreshMetadata()]);
    try { this.#onRefresh?.(this.snapshot(), this.#lastError); } catch { /* observer-safe */ }
  }

  async #refreshZen() {
    try {
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 30000);
      try {
        const r = await this.#fetch(`${this.#zenBase}/v1/models`, {
          signal: ctl.signal,
          headers: {
            authorization: 'Bearer public',
            'user-agent': `opencode/1.18.31 (linux x64; node${process.versions.node})`,
            'x-opencode-client': 'cli',
            accept: 'application/json',
          },
        });
        if (!r.ok) throw new Error(`models endpoint HTTP ${r.status}`);
        const payload = await r.json();
        const ids = [];
        for (const it of payload?.data ?? []) {
          if (typeof it?.id === 'string' && it.id) ids.push(it.id);
        }
        if (!ids.length) throw new Error('models endpoint returned empty list');
        this.#zen = new Set(ids);
        this.#updatedAt = this.#now();
        this.#lastError = '';
      } finally { clearTimeout(t); }
    } catch (e) { this.#lastError = e instanceof Error ? e.message : String(e); }
  }

  async #refreshMetadata() {
    // 24h cadence is enforced by the caller schedule; refreshOnce refreshes zen
    // every round and metadata opportunistically (cheap guard via updatedAt file).
    try {
      if (this.#cachePath) {
        try {
          const { readFile } = await import('node:fs/promises');
          const raw = JSON.parse(await readFile(this.#cachePath, 'utf8'));
          if (this.#now() - (raw.updatedAt ?? 0) < this.#metadataMs && Array.isArray(raw.prices) && raw.prices.length > 0) {
            this.#prices = new Map(raw.prices);
            this.#pricesReady = true;
            return;
          }
        } catch { /* stale/missing cache -> fetch live */ }
      }
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 30000);
      let prices;
      try {
        const r = await this.#fetch(this.#metadataUrl, {
          signal: ctl.signal, headers: { accept: 'application/json' },
        });
        if (!r.ok) throw new Error(`models.dev HTTP ${r.status}`);
        prices = decodeModelsDev(await r.json());
        if (!prices.size) throw new Error('models.dev has no opencode metadata');
      } finally { clearTimeout(t); }
      this.#prices = prices;
      this.#pricesReady = true;
      if (this.#cachePath) {
        try {
          const { mkdir, writeFile, rename, rm } = await import('node:fs/promises');
          const { dirname } = await import('node:path');
          await mkdir(dirname(this.#cachePath), { recursive: true });
          const tmp = `${this.#cachePath}.${process.pid}.tmp`;
          await writeFile(tmp, JSON.stringify({ updatedAt: this.#now(), prices: [...prices] }), 'utf8');
          await rm(this.#cachePath, { force: true });
          await rename(tmp, this.#cachePath);
        } catch { /* cache write is best-effort */ }
      }
    } catch (e) {
      if (!this.#pricesReady && this.#cachePath) {
        try {
          const { readFile } = await import('node:fs/promises');
          const raw = JSON.parse(await readFile(this.#cachePath, 'utf8'));
          if (Date.now() - (raw.updatedAt ?? 0) < 7 * 24 * 3600 * 1000 && Array.isArray(raw.prices) && raw.prices.length > 0) {
            this.#prices = new Map(raw.prices);
            this.#pricesReady = true;
            return;
          }
        } catch { /* ignore */ }
      }
      this.#lastError = e instanceof Error ? e.message : String(e);
    }
  }

  decision(id) { return decide(id, this.#prices, this.#pricesReady); }

  /** Chat-capable free models (responses-only flagged via api field).
   *  jev is excluded on purpose: it is a structured-decision endpoint
   *  (/v1/systemone), not a chat model, and is advertised separately. */
  list() {
    const seeds = new Map(ZEN_CHAT_MODELS_SEED.map((m) => [m.id, m]));
    if (this.#zen.size === 0) return [...seeds.values()];
    const out = [];
    for (const id of this.#zen) {
      if (id === JEV_MODEL) continue;
      if (!this.decision(id).allowed) continue;
      const seed = seeds.get(id);
      if (seed) out.push(enrich(seed, this.#prices.get(id)));
      else if (isFreeName(id)) {
        out.push({
          id, contextWindow: 262144, maxOutput: 32768,
          input: ['text'], reasoning: false, efforts: [],
          api: id.toLowerCase().startsWith('muse-spark') ? 'responses' : 'chat',
        });
      }
    }
    return out.sort((a, b) => (a.id < b.id ? -1 : 1));
  }

  get(id) { return this.list().find((m) => m.id === id); }

  snapshot() {
    const age = this.#updatedAt === 0 ? Infinity : this.#now() - this.#updatedAt;
    return {
      status: this.#updatedAt === 0 ? 'pending' : age > 10 * 60 * 1000 ? 'stale' : 'ready',
      total: this.#zen.size,
      exposed: this.list().length,
      ...(this.#updatedAt ? { lastRefresh: new Date(this.#updatedAt).toISOString() } : {}),
    };
  }

  get lastError() { return this.#lastError; }
}
