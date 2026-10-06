// forward.mjs — upstream fetch with first-byte + body-idle watchdogs,
// SSE byte passthrough, and one pre-content retry.
import { classifyUpstream, openAIError } from './freelane.mjs';

export async function fetchWithWatchdogs(url, init, { firstByteMs, bodyIdleMs, signal, fetchImpl }) {
  const doFetch = fetchImpl ?? fetch;
  const ctl = new AbortController();
  const onAbort = () => ctl.abort();
  if (signal) {
    if (signal.aborted) ctl.abort();
    else signal.addEventListener('abort', onAbort, { once: true });
  }
  let firstTimer;
  const firstRace = new Promise((_, reject) => {
    firstTimer = setTimeout(() => reject(Object.assign(new Error('first byte timeout'), { watchdog: 'first-byte' })), firstByteMs);
    firstTimer.unref?.();
  });
  let resp;
  try {
    resp = await Promise.race([doFetch(url, { ...init, signal: ctl.signal }), firstRace]);
  } catch (e) {
    clearTimeout(firstTimer);
    throw e;
  }
  clearTimeout(firstTimer);
  return { resp, signal: ctl.signal, cleanup: () => signal?.removeEventListener('abort', onAbort) };
}

/** Pipe resp.body to res with a body-idle watchdog. Calls hooks for stats.
 *  Returns { deliveredBytes: boolean(content chunk seen), usage: object|null }. */
export async function pipeSSE(resp, res, { bodyIdleMs, translate, ctx }) {
  let deliveredContent = false;
  let usage = null;
  let buf = '';
  let timer;
  const arm = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      try { res.write(`data: ${JSON.stringify({ error: { message: 'stream body idle timeout', code: 'zen_timeout' } })}\n\n`); } catch { /* closed */ }
      try { res.end(); } catch { /* closed */ }
      try { resp.body.cancel(); } catch { /* ignore */ }
    }, bodyIdleMs);
    timer.unref?.();
  };
  const flush = () => { clearTimeout(timer); };
  arm();
  try {
    for await (const chunk of resp.body) {
      arm();
      const text = Buffer.from(chunk).toString('utf8');
      if (!translate) {
        // Byte-passthrough for the client, but parse complete SSE events on
        // the side for usage/content stats. A regex cannot be used here:
        // Zen's usage object nests details (prompt_tokens_details), so only a
        // real JSON parse survives the payload intact.
        res.write(chunk);
        buf += text;
        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of block.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const payload = line.slice(5).trim();
            if (!payload || payload === '[DONE]') continue;
            let obj;
            try { obj = JSON.parse(payload); } catch { continue; }
            if (obj && typeof obj.usage === 'object' && obj.usage) usage = obj.usage;
            for (const ch of Array.isArray(obj?.choices) ? obj.choices : []) {
              const d = ch?.delta;
              if (d && (d.content || d.reasoning || d.tool_calls)) deliveredContent = true;
            }
          }
        }
        continue;
      }
      // Translated path (Responses -> chat).
      buf += text;
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let evType = '';
        let dataStr = '';
        for (const line of block.split('\n')) {
          if (line.startsWith('event:')) evType = line.slice(6).trim();
          else if (line.startsWith('data:')) dataStr += line.slice(5).trim();
        }
        if (!dataStr || dataStr === '[DONE]') continue;
        let data;
        try { data = JSON.parse(dataStr); } catch { continue; }
        const r = translate(evType, data, ctx);
        for (const line of r.out) {
          if (/content":"/.test(line)) deliveredContent = true;
          res.write(line);
        }
        if (r.usage) usage = r.usage;
        if (r.done) {
          flush();
          if (!r.fail) res.write('data: [DONE]\n\n');
          return { deliveredContent, usage, fail: r.fail };
        }
      }
    }
  } finally { flush(); }
  return { deliveredContent, usage, fail: null };
}

export function readErrorBody(resp) {
  return resp.text().then((t) => t.slice(0, 2000)).catch(() => '');
}

/** POST json upstream and either stream SSE to res or return parsed JSON. */
export async function upstreamChat(url, headers, payload, res, opts) {
  const { firstByteMs, bodyIdleMs, upstreamTimeoutMs, clientSignal, fetchImpl } = opts;
  const maxAttempts = 1 + (opts.preContentRetries ?? 0);
  let lastErr = null;
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    const ctl = new AbortController();
    const to = setTimeout(() => ctl.abort(), upstreamTimeoutMs);
    if (clientSignal) {
      if (clientSignal.aborted) { clearTimeout(to); throw Object.assign(new Error('client aborted'), { clientAborted: true }); }
      clientSignal.addEventListener('abort', () => ctl.abort(), { once: true });
    }
    try {
      const { resp } = await fetchWithWatchdogs(url, {
        method: 'POST', headers, body: JSON.stringify(payload),
      }, { firstByteMs, bodyIdleMs: firstByteMs, signal: ctl.signal, fetchImpl });
      if (!resp.ok) {
        // Capture the upstream hint before the body is consumed.
        const retryAfter = resp.headers.get('retry-after');
        const text = await readErrorBody(resp);
        clearTimeout(to);
        lastErr = classifyUpstream(resp.status, text);
        // Retryable before content: 429 / 5xx / transport-ish. 403 region/deny: no retry.
        if (attempt + 1 < maxAttempts && (lastErr.http === 429 || lastErr.http === 503)) continue;
        const mapped = openAIError(lastErr.http, lastErr.code, lastErr.message);
        if (!res.headersSent) {
          const out = { 'content-type': 'application/json' };
          if (retryAfter) out['retry-after'] = retryAfter;
          res.writeHead(mapped.http, out);
        }
        res.end(JSON.stringify(mapped.body));
        return { ok: false, ...lastErr, deliveredContent: false, usage: null, retryAfter };
      }
      if (payload.stream === false) {
        const text = await resp.text();
        clearTimeout(to);
        let parsed;
        try { parsed = JSON.parse(text); } catch { parsed = null; }
        if (!parsed) {
          const mapped = openAIError(503, 'zen_upstream', 'non-stream response was not JSON');
          if (!res.headersSent) res.writeHead(503, { 'content-type': 'application/json' });
          res.end(JSON.stringify(mapped.body));
          return { ok: false, http: 503, code: 'zen_upstream', deliveredContent: false, usage: null };
        }
        if (!res.headersSent) res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify(parsed));
        const usage = parsed.usage ? {
          prompt_tokens: parsed.usage.prompt_tokens ?? parsed.usage.input_tokens ?? 0,
          completion_tokens: parsed.usage.completion_tokens ?? parsed.usage.output_tokens ?? 0,
          total_tokens: parsed.usage.total_tokens ?? 0,
        } : null;
        return { ok: true, deliveredContent: true, usage };
      }
      if (!res.headersSent) {
        res.writeHead(200, {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
          connection: 'keep-alive',
        });
      }
      const out = await pipeSSE(resp, res, {
        bodyIdleMs,
        translate: opts.translate,
        ctx: opts.translateCtx,
      });
      clearTimeout(to);
      try { res.end(); } catch { /* closed */ }
      if (out.fail) return { ok: false, http: 503, code: 'responses_failed', deliveredContent: out.deliveredContent, usage: out.usage };
      return { ok: true, deliveredContent: out.deliveredContent, usage: out.usage };
    } catch (e) {
      clearTimeout(to);
      if (e?.clientAborted) throw e;
      const text = e?.watchdog === 'first-byte' ? 'zen first byte timeout'
        : e?.name === 'AbortError' ? 'zen upstream timeout'
        : (e instanceof Error ? e.message : String(e));
      lastErr = classifyUpstream(0, text);
      if (attempt + 1 < maxAttempts) continue;
      const mapped = openAIError(lastErr.http, lastErr.code, lastErr.message);
      if (!res.headersSent) res.writeHead(mapped.http, { 'content-type': 'application/json' });
      try { res.end(JSON.stringify(mapped.body)); } catch { /* closed */ }
      return { ok: false, ...lastErr, deliveredContent: false, usage: null };
    }
  }
  const mapped = openAIError(lastErr.http, lastErr.code, lastErr.message);
  if (!res.headersSent) res.writeHead(mapped.http, { 'content-type': 'application/json' });
  try { res.end(JSON.stringify(mapped.body)); } catch { /* closed */ }
  return { ok: false, ...lastErr, deliveredContent: false, usage: null };
}
