// responses.mjs — Responses SSE -> OpenAI chat SSE translation (M2).
// muse-spark-* is Responses-only on Zen (500 on /chat/completions).
// Clients keep speaking chat-completions; the gateway translates both ways.
// First release: text + usage. Tool-call translation is M2+ (different
// function-call semantics between the two protocols).
//
// Wire convention (OpenAI Responses): event lines "event: <type>" +
// "data: <json>". We only need: response.output_text.delta,
// response.completed (usage), response.failed/incomplete, error.

/** Build a Responses request body from a chat body. */
export function toResponsesBody(chatBody, model, effortWire) {
  const input = [];
  for (const m of chatBody.messages ?? []) {
    if (m.role === 'system') {
      input.push({ role: 'system', content: [{ type: 'input_text', text: String(m.content ?? '') }] });
      continue;
    }
    if (m.role === 'user') {
      const parts = [];
      if (typeof m.content === 'string') {
        if (m.content) parts.push({ type: 'input_text', text: m.content });
      } else if (Array.isArray(m.content)) {
        for (const p of m.content) {
          if (p?.type === 'text' && p.text) parts.push({ type: 'input_text', text: p.text });
          else if (p?.type === 'image_url' && (p.image_url?.url)) {
            parts.push({ type: 'input_image', image_url: p.image_url.url });
          }
        }
      }
      if (parts.length) input.push({ role: 'user', content: parts });
      continue;
    }
    if (m.role === 'assistant' && typeof m.content === 'string' && m.content) {
      input.push({ role: 'assistant', content: [{ type: 'output_text', text: m.content }] });
    }
  }
  const body = { model, input, stream: true };
  const maxOut = Number.isSafeInteger(chatBody.max_tokens) && chatBody.max_tokens > 0
    ? chatBody.max_tokens
    : Number.isSafeInteger(chatBody.max_completion_tokens) && chatBody.max_completion_tokens > 0
      ? chatBody.max_completion_tokens
      : undefined;
  if (maxOut !== undefined) body.max_output_tokens = maxOut;
  if (typeof chatBody.temperature === 'number') body.temperature = chatBody.temperature;
  if (effortWire && effortWire !== 'none') body.reasoning = { effort: effortWire };
  const tools = [];
  if (Array.isArray(chatBody.tools)) {
    for (const t of chatBody.tools) {
      if (t?.type === 'function' && t.function?.name) {
        // Responses flat function tools; the free-lane gate needs bash+read.
        tools.push({ type: 'function', name: t.function.name, description: t.function.description ?? 'x', parameters: t.function.parameters ?? { type: 'object', properties: {} } });
      }
    }
  }
  const names = new Set(tools.map((t) => t.name));
  for (const n of ['bash', 'read']) {
    if (!names.has(n)) tools.push({ type: 'function', name: n, description: 'Reserved for the host runtime; do not call it.', parameters: { type: 'object', properties: {} } });
  }
  body.tools = tools;
  body.tool_choice = 'auto';
  return body;
}

/** Translate one Responses SSE event into zero or more chat SSE data payloads.
 *  Returns { out: string[], done: boolean, usage: object|null, fail: string|null }. */
export function translateEvent(evType, data, ctx) {
  const out = [];
  const id = ctx.id;
  const created = ctx.created;
  const model = ctx.model;
  const envelope = (delta, finish) => ({
    id, object: 'chat.completion.chunk', created, model,
    choices: [{ index: 0, delta, ...(finish ? { finish_reason: finish } : { finish_reason: null }) }],
  });
  const sse = (payload) => `data: ${JSON.stringify(payload)}\n\n`;

  switch (evType) {
    case 'response.output_text.delta': {
      const text = typeof data?.delta === 'string' ? data.delta : '';
      if (text) {
        ctx.sawContent = true;
        out.push(sse(envelope({ role: ctx.first ? 'assistant' : undefined, content: text })));
        ctx.first = false;
      }
      return { out, done: false, usage: null, fail: null };
    }
    case 'response.completed': {
      const resp = data?.response ?? data ?? {};
      const usage = resp.usage ? {
        prompt_tokens: resp.usage.input_tokens ?? 0,
        completion_tokens: resp.usage.output_tokens ?? 0,
        total_tokens: resp.usage.total_tokens ?? 0,
      } : null;
      out.push(sse(envelope({}, 'stop')));
      return { out, done: true, usage, fail: null };
    }
    case 'response.incomplete': {
      const reason = data?.response?.incomplete_details?.reason;
      out.push(sse(envelope({}, reason === 'max_output_tokens' ? 'length' : 'stop')));
      return { out, done: true, usage: null, fail: null };
    }
    case 'response.failed': {
      const msg = data?.response?.error?.message ?? data?.error?.message ?? 'responses stream failed';
      return { out, done: true, usage: null, fail: String(msg).slice(0, 500) };
    }
    case 'error': {
      const msg = data?.error?.message ?? data?.message ?? 'responses stream error';
      return { out, done: true, usage: null, fail: String(msg).slice(0, 500) };
    }
    default:
      return { out, done: false, usage: null, fail: null };
  }
}

export function newTranslateCtx(model) {
  return {
    id: `chatcmpl-zen-${Date.now().toString(36)}`,
    created: Math.floor(Date.now() / 1000),
    model,
    first: true,
    sawContent: false,
  };
}
