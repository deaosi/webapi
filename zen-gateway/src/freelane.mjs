// freelane.mjs — free-lane body shaping + upstream error taxonomy.
// Gate (live-probed): a chat body must stream AND carry function tools named
// "bash" AND "read", otherwise 403 FreeTierError. The injected stubs are inert:
// plain chats get tool_choice "none", real tool_choice values are preserved.
export const GATE_TOOL_NAMES = ['bash', 'read'];

export function gateTool(name) {
  return {
    type: 'function',
    function: {
      name,
      description: 'Reserved for the host runtime; do not call it.',
      parameters: { type: 'object', properties: {} },
    },
  };
}

/** Mutate a chat-completions body in place shape; returns the payload to send. */
export function ensureFreeLaneShape(body) {
  if (!body || typeof body !== 'object' || Array.isArray(body)) return body;
  if (!Array.isArray(body.messages)) return body;
  const tools = Array.isArray(body.tools) ? body.tools : [];
  const names = new Set(tools.map((t) => t?.function?.name));
  const missing = GATE_TOOL_NAMES.filter((n) => !names.has(n));
  if (missing.length === 0) return body;
  const next = { ...body, tools: [...tools, ...missing.map(gateTool)] };
  if (tools.length === 0) next.tool_choice = 'none';
  return next;
}

const EFFORT_LADDER = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];

/** Client reasoning_effort -> Zen wire value. 'off' is the only id that maps
 *  to a different spelling ('none'); unknown ids inject nothing. */
export function reasoningEffortWire(id) {
  if (id === undefined) return undefined;
  if (id === 'off' || id === 'none') return 'none';
  return EFFORT_LADDER.includes(id) ? id : undefined;
}

export function applyReasoningEffort(body, model, effortWire, allowedEfforts) {
  if (effortWire === undefined) return body;
  if (Array.isArray(allowedEfforts) && allowedEfforts.length > 0 && !allowedEfforts.includes(effortWire === 'none' ? 'off' : effortWire) && effortWire !== 'none') {
    return body; // never-advertised value: don't risk the 400
  }
  return { ...body, reasoning_effort: effortWire };
}

/** Map an upstream failure (status + body text) to the gateway error shape. */
export function classifyUpstream(status, text) {
  const t = String(text ?? '');
  if (/RegionError|not available in your country/i.test(t)) {
    return { http: 403, code: 'region_blocked', message: t.slice(0, 500) || 'model not available in this region' };
  }
  if (status === 429 || /\b429\b|rate.?limit|freeusagelimit/i.test(t)) {
    return { http: 429, code: 'rate_limit_exceeded', message: t.slice(0, 500) || 'zen anonymous quota exhausted for this exit IP' };
  }
  if (status === 401 || status === 403) {
    return { http: 503, code: 'zen_refused', message: t.slice(0, 500) || `zen refused with ${status}` };
  }
  if (/\b413\b|payload too large/i.test(t)) {
    return { http: 413, code: 'invalid_request', message: t.slice(0, 500) };
  }
  if (status >= 500 || /\b5\d\d\b/.test(t)) {
    return { http: 503, code: 'zen_server', message: t.slice(0, 500) || `zen server error ${status}` };
  }
  if (/timeout|timed out/i.test(t)) {
    return { http: 503, code: 'zen_timeout', message: t.slice(0, 500) };
  }
  return { http: 503, code: 'zen_upstream', message: t.slice(0, 500) || `zen request failed (${status})` };
}

export function openAIError(http, code, message) {
  return { http, body: { error: { message, type: code, code } } };
}
