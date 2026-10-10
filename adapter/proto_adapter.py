#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""wk2api stateless proto adapter.

Panel (Go) only speaks Chat (POST /v1/chat/completions, GET /v1/models).
This adapter (default listen 127.0.0.1:8788) adds translation on top:
  POST /v1/chat/completions  -> passthrough to panel
  POST /v1/responses         -> translate to chat, reply Responses object/SSE
  POST /v1/messages          -> translate to chat, reply Anthropic object/SSE
  POST /v1/messages/count_tokens -> local CJK estimate

PANEL_BASE default http://127.0.0.1:7863 (server panel port);
local dev panel on :8888 overrides via --panel.
Key lookup: PANEL_KEY env > /etc/wb2api/config.json > local config.json;
empty = open (same as panel).

stdlib only, no pip deps.
"""
import argparse
import hashlib
import hmac
import http.client
import json
import os
import re
import time
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PANEL_BASE = os.environ.get("PANEL_BASE", "http://127.0.0.1:7863")
PANEL_KEY = os.environ.get("PANEL_KEY", "")
ADAPTER_LISTEN = os.environ.get("ADAPTER_LISTEN", "127.0.0.1:8788")

# panel config.json fallback key (local dev; server injects PANEL_KEY via systemd Environment)
_DEFAULT_PANEL_KEY = ""

CJK_RE = re.compile(r"[\u4e00-\u9fff\u3400-\u4dbf\uf900-\ufaff"
                    r"\u3040-\u30ff\uac00-\ud7af\uff00-\uffef]")


def _new_id(prefix):
    return prefix + uuid.uuid4().hex


def estimate_tokens(text):
    if not text:
        return 0
    if not isinstance(text, str):
        text = str(text)
    cjk = len(text) - len(CJK_RE.sub("", text))
    other = len(text) - cjk
    return cjk + max(1, int(other / 3.6)) if text else 0


# ---------------- Responses -> Chat ----------------

def _flatten_content(content):
    """Responses content -> text(str) or multimodal parts(list)."""
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return str(content)
    texts, parts = [], []
    for piece in content:
        if isinstance(piece, str):
            texts.append(piece)
            parts.append({"type": "text", "text": piece})
            continue
        if not isinstance(piece, dict):
            continue
        ptype = piece.get("type") or ""
        if ptype in ("input_text", "output_text", "text", "summary_text"):
            t = piece.get("text") or ""
            texts.append(t)
            parts.append({"type": "text", "text": t})
        elif ptype in ("input_image", "image_url", "image") or "image_url" in piece:
            url = piece.get("image_url") or piece.get("url")
            if isinstance(url, dict):
                url = url.get("url")
            if url:
                parts.append({"type": "image_url", "image_url": {"url": url}})
    if any(p.get("type") == "image_url" for p in parts):
        return parts
    return "\n".join(t for t in texts if t)


def _is_custom_tool(tool):
    return isinstance(tool, dict) and str(tool.get("type") or "").lower() == "custom"


def custom_tool_names(tools):
    names = set()
    for t in tools or []:
        if _is_custom_tool(t) and t.get("name"):
            names.add(str(t["name"]))
    return names


_CUSTOM_HINT = ("This is a freeform tool. Put the COMPLETE raw payload into the single "
                "'input' string parameter, verbatim. Do not wrap it in JSON.")


def _downgrade_custom_tool(tool):
    desc = tool.get("description") or ""
    fmt = tool.get("format") or {}
    extra = ""
    if isinstance(fmt, dict) and fmt.get("definition"):
        extra = "\n\nGrammar:\n" + str(fmt["definition"])
    return {
        "type": "function",
        "function": {
            "name": tool.get("name") or "",
            "description": (desc + "\n\n" + _CUSTOM_HINT + extra).strip(),
            "parameters": {
                "type": "object",
                "properties": {"input": {"type": "string",
                                          "description": "Complete raw payload, verbatim."}},
                "required": ["input"],
            },
        },
    }


_NS_SEP = "__"


def expand_namespace_tools(tools, _depth=0):
    """Expand namespace into flat function list. Returns (flat, name->ns)."""
    flat, mapping, seen = [], {}, set()

    def collect(entry, depth, ns_name):
        if not isinstance(entry, dict) or depth > 4:
            return
        etype = str(entry.get("type") or "").lower()
        if etype == "namespace":
            subs = entry.get("tools")
            if not isinstance(subs, list):
                subs = entry.get("children")
            if not isinstance(subs, list):
                subs = entry.get("functions")
            if not isinstance(subs, list):
                subs = []
            child_ns = str(entry.get("name") or ns_name or "")
            for sub in subs:
                collect(sub, depth + 1, child_ns)
            return
        if ns_name and etype in ("", "function"):
            fn = entry.get("function") if isinstance(entry.get("function"), dict) else None
            if fn is None:
                fn = {"name": entry.get("name"),
                      "description": entry.get("description") or "",
                      "parameters": entry.get("parameters") or entry.get("input_schema")
                      or {"type": "object", "properties": {}}}
            name = str(fn.get("name") or "").strip()
            if not name or name in seen:
                return
            seen.add(name)
            mapping[name] = ns_name
            flat.append({"type": "function", "function": {
                "name": name, "description": fn.get("description") or "",
                "parameters": fn.get("parameters") or {"type": "object", "properties": {}}}})
            return
        fn = entry.get("function") if isinstance(entry.get("function"), dict) else {}
        name = str(entry.get("name") or fn.get("name") or "").strip()
        if name:
            if name in seen:
                return
            seen.add(name)
            if ns_name:
                mapping[name] = ns_name
        flat.append(entry)

    for e in tools or []:
        collect(e, 0, "")
    return flat, mapping


def resolve_namespaced_name(name, mapping):
    if not name:
        return name, ""
    name = str(name)
    if name in mapping:
        return name, mapping[name]
    if "::" in name:
        i = name.find("::")
        if i > 0:
            tail, head = name[i + 2:], name[:i]
            if tail in mapping:
                return tail, mapping[tail]
            return tail, head
    for tool, ns in (mapping or {}).items():
        if name == ns + _NS_SEP + tool:
            return tool, ns
    return name, ""


def stamp_namespace(item, mapping):
    if not mapping or not isinstance(item, dict):
        return item
    bare, ns = resolve_namespaced_name(item.get("name"), mapping)
    if ns:
        item["name"] = bare
        item["namespace"] = ns
    return item


def _tools_for_chat(tools):
    out = []
    for t in tools or []:
        if not isinstance(t, dict):
            continue
        out.append(_downgrade_custom_tool(t) if _is_custom_tool(t) else t)
    return out


def _unwrap_custom_input(args):
    if not isinstance(args, str):
        return json.dumps(args or "", ensure_ascii=False)
    try:
        parsed = json.loads(args)
    except Exception:
        return args
    if isinstance(parsed, dict):
        v = parsed.get("input")
        if isinstance(v, str):
            return v
        if v is not None:
            return json.dumps(v, ensure_ascii=False)
    if isinstance(parsed, str):
        return parsed
    return args


def _responses_input_to_messages(payload):
    messages = []
    instr = payload.get("instructions")
    if isinstance(instr, str) and instr.strip():
        messages.append({"role": "system", "content": instr})
    inp = payload.get("input")
    pending_reasoning = ""
    if isinstance(inp, str):
        if inp:
            messages.append({"role": "user", "content": inp})
    elif isinstance(inp, list):
        for item in inp:
            if isinstance(item, str):
                messages.append({"role": "user", "content": item})
                continue
            if not isinstance(item, dict):
                continue
            itype = item.get("type")
            if itype in (None, "message", "user"):
                body = _flatten_content(item.get("content"))
                if not body:
                    continue
                role = item.get("role") or "user"
                if role == "developer":
                    role = "system"
                if role == "assistant" and messages and messages[-1].get("role") == "assistant":
                    prev = messages[-1]
                    prev["content"] = str(prev.get("content") or "") + "\n" + str(body) \
                        if prev.get("content") else body
                    if pending_reasoning and "reasoning_content" not in prev:
                        prev["reasoning_content"] = pending_reasoning
                        pending_reasoning = ""
                else:
                    d = {"role": role, "content": body}
                    if role == "assistant" and pending_reasoning:
                        d["reasoning_content"] = pending_reasoning
                        pending_reasoning = ""
                    messages.append(d)
            elif itype == "reasoning":
                r = ""
                summ = item.get("summary")
                if isinstance(summ, list):
                    r = "\n".join(p.get("text", "") for p in summ
                                  if isinstance(p, dict) and p.get("text"))
                elif isinstance(summ, str):
                    r = summ
                if not r:
                    c = item.get("content")
                    r = c if isinstance(c, str) else _flatten_content(c)
                if r:
                    if messages and messages[-1].get("role") == "assistant":
                        messages[-1]["reasoning_content"] = r
                    else:
                        pending_reasoning = r
            elif itype == "function_call_output":
                raw = item.get("output")
                cid = item.get("call_id") or ""
                if not cid:
                    t = raw if isinstance(raw, str) else json.dumps(raw or "", ensure_ascii=False)
                    if (t or "").strip():
                        messages.append({"role": "user", "content":
                            "[Message from another task]\n\n" + t.strip()})
                    continue
                content = raw if isinstance(raw, str) else (
                    _flatten_content(raw) if isinstance(raw, list)
                    else json.dumps(raw or "", ensure_ascii=False))
                messages.append({"role": "tool", "tool_call_id": cid, "content": content})
            elif itype == "function_call":
                tc = {"id": item.get("call_id") or item.get("id") or "",
                      "type": "function",
                      "function": {"name": item.get("name") or "",
                                   "arguments": item.get("arguments") or "{}"}}
                if messages and messages[-1].get("role") == "assistant":
                    prev = messages[-1]
                    prev.setdefault("tool_calls", []).append(tc)
                else:
                    messages.append({"role": "assistant", "content": "", "tool_calls": [tc]})
            elif itype == "custom_tool_call":
                raw = item.get("input")
                raw = raw if isinstance(raw, str) else json.dumps(raw or "", ensure_ascii=False)
                tc = {"id": item.get("call_id") or item.get("id") or "",
                      "type": "function",
                      "function": {"name": item.get("name") or "",
                                   "arguments": json.dumps({"input": raw}, ensure_ascii=False)}}
                if messages and messages[-1].get("role") == "assistant":
                    messages[-1].setdefault("tool_calls", []).append(tc)
                else:
                    messages.append({"role": "assistant", "content": "", "tool_calls": [tc]})
            elif itype == "custom_tool_call_output":
                raw = item.get("output")
                content = raw if isinstance(raw, str) else json.dumps(raw or "", ensure_ascii=False)
                messages.append({"role": "tool",
                                 "tool_call_id": item.get("call_id") or "", "content": content})
            else:
                continue
    return messages


def responses_to_chat(payload):
    messages = _responses_input_to_messages(payload)
    chat = {"model": payload.get("model"), "messages": messages}
    for k in ("temperature", "top_p", "seed"):
        if payload.get(k) is not None:
            chat[k] = payload[k]
    if payload.get("max_output_tokens") is not None:
        chat["max_tokens"] = payload["max_output_tokens"]
    effort = None
    rs = payload.get("reasoning")
    if isinstance(rs, dict):
        effort = rs.get("effort")
    if not effort:
        effort = payload.get("reasoning_effort")
    if effort:
        chat["reasoning_effort"] = effort
    if payload.get("tools"):
        flat, ns_map = expand_namespace_tools(payload["tools"])
        chat["tools"] = _tools_for_chat(flat)
        if ns_map:
            chat["_namespace_map"] = ns_map
    if payload.get("tool_choice"):
        chat["tool_choice"] = payload["tool_choice"]
    if payload.get("parallel_tool_calls") is not None:
        chat["parallel_tool_calls"] = payload["parallel_tool_calls"]
    if payload.get("stream") is not None:
        chat["stream"] = bool(payload.get("stream"))
    return chat


# ---------------- Chat -> Responses ----------------

def _responses_usage(u):
    if not u:
        return None
    det = u.get("completion_tokens_details") or {}
    pdet = u.get("prompt_tokens_details") or {}
    return {
        "input_tokens": u.get("prompt_tokens") or 0,
        "input_tokens_details": {"cached_tokens": u.get("prompt_cache_hit_tokens")
                                 or det.get("cached_tokens") or pdet.get("cached_tokens") or 0},
        "output_tokens": u.get("completion_tokens") or 0,
        "output_tokens_details": {"reasoning_tokens": det.get("reasoning_tokens") or 0},
        "total_tokens": u.get("total_tokens") or 0,
    }


def chat_to_response(chat_obj, model, custom_names=None, request_meta=None, namespace_map=None):
    custom_names = custom_names or set()
    choice = (chat_obj.get("choices") or [{}])[0]
    msg = choice.get("message") or {}
    text = msg.get("content") or ""
    reasoning = msg.get("reasoning_content") or ""
    output = []
    if reasoning:
        output.append({"id": _new_id("rs_"), "type": "reasoning", "status": "completed",
                       "summary": [{"type": "summary_text", "text": reasoning}]})
    for tc in msg.get("tool_calls") or []:
        fn = tc.get("function") or {}
        cid = tc.get("id") or _new_id("call_")
        name = fn.get("name") or ""
        if name and name in custom_names:
            item = {"id": _new_id("ctc_"), "type": "custom_tool_call", "status": "completed",
                    "call_id": cid, "name": name,
                    "input": _unwrap_custom_input(fn.get("arguments") or "")}
        else:
            item = {"id": _new_id("fc_"), "type": "function_call", "status": "completed",
                    "call_id": cid, "name": name, "arguments": fn.get("arguments") or "{}"}
        stamp_namespace(item, namespace_map)
        output.append(item)
    if text or not output:
        output.append({"id": _new_id("msg_"), "type": "message", "status": "completed",
                       "role": "assistant",
                       "content": [{"type": "output_text", "text": text, "annotations": []}]
                       if text else []})
    finish = choice.get("finish_reason") or "stop"
    obj = {"id": _new_id("resp_"), "object": "response", "created_at": int(time.time()),
           "status": "completed" if finish != "length" else "incomplete",
           "model": model, "output": output, "output_text": text, "metadata": {}}
    meta = request_meta or {}
    obj["parallel_tool_calls"] = meta.get("parallel_tool_calls", True)
    obj["tool_choice"] = meta.get("tool_choice", "auto")
    obj["tools"] = meta.get("tools") or []
    u = _responses_usage(chat_obj.get("usage"))
    if u:
        obj["usage"] = u
    if finish == "length":
        obj["incomplete_details"] = {"reason": "max_output_tokens"}
    return obj


def _resp_ev(etype, payload, seq):
    data = {"type": etype, "sequence_number": seq[0]}
    seq[0] += 1
    data.update(payload)
    return ("event: " + etype + "\ndata: " + json.dumps(data, ensure_ascii=False)
            + "\n\n").encode("utf-8")


def stream_responses_events(chat_line_iter, model, holder):
    """Chat SSE lines -> Responses SSE frames. Writes usage back into holder."""
    resp_id, msg_id, rs_id = _new_id("resp_"), _new_id("msg_"), _new_id("rs_")
    created = int(time.time())
    seq = [0]
    text_parts, reason_parts, outputs = [], [], []
    reason_index = msg_index = None
    finish, usage = "stop", None
    tool_map = {}
    ns_map = holder.get("namespace_map") or {}
    custom_names = set(holder.get("custom_names") or ())
    meta = holder.get("request_meta") or {}

    def resp_obj(status):
        o = {"id": resp_id, "object": "response", "created_at": created, "status": status,
             "model": model, "output": [x for x in outputs if x],
             "output_text": "".join(text_parts),
             "parallel_tool_calls": meta.get("parallel_tool_calls", True),
             "tool_choice": meta.get("tool_choice", "auto"),
             "tools": meta.get("tools") or [], "metadata": {}}
        u = _responses_usage(usage)
        if u:
            o["usage"] = u
        return o

    def reason_item(status):
        return {"id": rs_id, "type": "reasoning", "status": status,
                "summary": [{"type": "summary_text", "text": "".join(reason_parts)}]}

    def msg_item(status):
        it = {"id": msg_id, "type": "message", "status": status,
              "role": "assistant", "content": []}
        if text_parts:
            it["content"] = [{"type": "output_text", "text": "".join(text_parts),
                              "annotations": []}]
        return it

    yield _resp_ev("response.created", {"response": resp_obj("in_progress")}, seq)
    yield _resp_ev("response.in_progress", {"response": resp_obj("in_progress")}, seq)
    for raw in chat_line_iter:
        line = raw.decode("utf-8", "replace") if isinstance(raw, bytes) else raw
        line = line.strip()
        if not line or line.startswith(":"):
            continue
        while line.startswith("data:"):
            line = line[5:].strip()
        if not line or line == "[DONE]":
            continue
        try:
            chunk = json.loads(line)
        except Exception:
            continue
        if isinstance(chunk.get("usage"), dict):
            usage = chunk["usage"]
            holder["usage"] = usage
        for choice in chunk.get("choices") or []:
            delta = choice.get("delta") or {}
            piece = delta.get("reasoning_content")
            if piece:
                if reason_index is None:
                    reason_index = len(outputs)
                    outputs.append(None)
                    yield _resp_ev("response.output_item.added",
                                   {"output_index": reason_index,
                                    "item": dict(reason_item("in_progress"),
                                                 id=rs_id)}, seq)
                    yield _resp_ev("response.reasoning_summary_part.added",
                                   {"item_id": rs_id, "output_index": reason_index,
                                    "summary_index": 0,
                                    "part": {"type": "summary_text", "text": ""}}, seq)
                reason_parts.append(piece)
                yield _resp_ev("response.reasoning_summary_text.delta",
                               {"item_id": rs_id, "output_index": reason_index,
                                "summary_index": 0, "delta": piece}, seq)
            for tc in delta.get("tool_calls") or []:
                try:
                    idx = int(tc.get("index") or 0)
                except Exception:
                    idx = 0
                fn = tc.get("function") or {}
                fn_name, fn_args = fn.get("name") or "", fn.get("arguments") or ""
                call_id = tc.get("id") or ""
                if idx not in tool_map:
                    out_idx = len(outputs)
                    outputs.append(None)
                    c_id = call_id or _new_id("call_")
                    is_c = bool(fn_name) and fn_name in custom_names
                    tool_map[idx] = {"output_index": out_idx, "id": c_id, "name": fn_name,
                                     "arguments": fn_args, "custom": is_c,
                                     "item_id": _new_id("ctc_" if is_c else "fc_")}
                    e = tool_map[idx]
                    item = {"id": e["item_id"], "status": "in_progress", "call_id": c_id,
                            "name": fn_name}
                    if is_c:
                        item.update(type="custom_tool_call", input="")
                    else:
                        item.update(type="function_call", arguments="")
                    stamp_namespace(item, ns_map)
                    yield _resp_ev("response.output_item.added",
                                   {"output_index": out_idx, "item": item}, seq)
                else:
                    e = tool_map[idx]
                    if fn_name and not e["name"]:
                        e["name"] = fn_name
                        e["custom"] = fn_name in custom_names
                    if fn_args:
                        e["arguments"] += fn_args
                        if e.get("custom"):
                            yield _resp_ev("response.custom_tool_call_input.delta",
                                           {"output_index": e["output_index"],
                                            "item_id": e["item_id"], "call_id": e["id"],
                                            "delta": fn_args}, seq)
                        else:
                            yield _resp_ev("response.function_call_arguments.delta",
                                           {"output_index": e["output_index"],
                                            "item_id": e["item_id"], "call_id": e["id"],
                                            "delta": fn_args}, seq)
            piece = delta.get("content")
            if piece:
                if msg_index is None:
                    msg_index = len(outputs)
                    outputs.append(None)
                    yield _resp_ev("response.output_item.added",
                                   {"output_index": msg_index,
                                    "item": {"id": msg_id, "type": "message",
                                             "status": "in_progress", "role": "assistant",
                                             "content": []}}, seq)
                    yield _resp_ev("response.content_part.added",
                                   {"item_id": msg_id, "output_index": msg_index,
                                    "content_index": 0,
                                    "part": {"type": "output_text", "text": "",
                                             "annotations": []}}, seq)
                text_parts.append(piece)
                yield _resp_ev("response.output_text.delta",
                               {"item_id": msg_id, "output_index": msg_index,
                                "content_index": 0, "delta": piece}, seq)
            if choice.get("finish_reason"):
                finish = choice["finish_reason"]
    # finalize
    if reason_index is not None and outputs[reason_index] is None:
        full_r = "".join(reason_parts)
        yield _resp_ev("response.reasoning_summary_text.done",
                       {"item_id": rs_id, "output_index": reason_index,
                        "summary_index": 0, "text": full_r}, seq)
        yield _resp_ev("response.reasoning_summary_part.done",
                       {"item_id": rs_id, "output_index": reason_index,
                        "summary_index": 0,
                        "part": {"type": "summary_text", "text": full_r}}, seq)
        outputs[reason_index] = reason_item("completed")
        yield _resp_ev("response.output_item.done",
                       {"output_index": reason_index,
                        "item": outputs[reason_index]}, seq)
    for idx in sorted(tool_map.keys()):
        e = tool_map[idx]
        if e.get("custom"):
            yield _resp_ev("response.custom_tool_call_input.done",
                           {"output_index": e["output_index"], "item_id": e["item_id"],
                            "call_id": e["id"],
                            "input": _unwrap_custom_input(e["arguments"])}, seq)
            item = {"id": e["item_id"], "type": "custom_tool_call", "status": "completed",
                    "call_id": e["id"], "name": e["name"],
                    "input": _unwrap_custom_input(e["arguments"])}
        else:
            yield _resp_ev("response.function_call_arguments.done",
                           {"output_index": e["output_index"], "item_id": e["item_id"],
                            "call_id": e["id"], "arguments": e["arguments"]}, seq)
            item = {"id": e["item_id"], "type": "function_call", "status": "completed",
                    "call_id": e["id"], "name": e["name"], "arguments": e["arguments"]}
        stamp_namespace(item, ns_map)
        outputs[e["output_index"]] = item
        yield _resp_ev("response.output_item.done",
                       {"output_index": e["output_index"], "item": item}, seq)
    full_text = "".join(text_parts)
    if msg_index is not None or full_text or not any(o for o in outputs if o):
        if msg_index is None:
            msg_index = len(outputs)
            outputs.append(None)
            yield _resp_ev("response.output_item.added",
                           {"output_index": msg_index,
                            "item": {"id": msg_id, "type": "message", "status": "in_progress",
                                     "role": "assistant", "content": []}}, seq)
            yield _resp_ev("response.content_part.added",
                           {"item_id": msg_id, "output_index": msg_index, "content_index": 0,
                            "part": {"type": "output_text", "text": "", "annotations": []}}, seq)
        yield _resp_ev("response.output_text.done",
                       {"item_id": msg_id, "output_index": msg_index,
                        "content_index": 0, "text": full_text}, seq)
        yield _resp_ev("response.content_part.done",
                       {"item_id": msg_id, "output_index": msg_index, "content_index": 0,
                        "part": {"type": "output_text", "text": full_text,
                                 "annotations": []}}, seq)
        outputs[msg_index] = msg_item("completed")
        yield _resp_ev("response.output_item.done",
                       {"output_index": msg_index, "item": outputs[msg_index]}, seq)
    final = resp_obj("completed" if finish != "length" else "incomplete")
    if finish == "length":
        final["incomplete_details"] = {"reason": "max_output_tokens"}
    yield _resp_ev("response.completed", {"response": final}, seq)


# ---------------- Anthropic <-> Chat ----------------

def _a_text(v):
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    if isinstance(v, (int, float, bool)):
        return str(v)
    return ""


def _a_json_text(v):
    if v is None:
        return "{}"
    if isinstance(v, str):
        return v
    try:
        return json.dumps(v, ensure_ascii=False, separators=(",", ":"))
    except Exception:
        return "{}"


def _a_parse_input(v):
    if v is None:
        return {}
    if isinstance(v, dict):
        return v
    if isinstance(v, str):
        if not v:
            return {}
        try:
            p = json.loads(v)
        except Exception:
            return {"raw": v}
        return p if isinstance(p, dict) else {"value": p}
    return {"value": v}


def _a_content_text(value, is_error=None):
    if isinstance(value, str):
        t = value
    elif isinstance(value, list):
        ps = []
        for it in value:
            if not isinstance(it, dict):
                continue
            typ = str(it.get("type") or "")
            if typ == "image":
                ps.append(_a_text((_a_image_part(it).get("image_url") or {}).get("url")))
            elif typ == "document":
                ps.append(_a_doc_text(it))
            else:
                ps.append(_a_block_text(it))
        t = "".join(ps)
    elif isinstance(value, dict):
        t = _a_block_text(value)
    else:
        t = _a_text(value)
    return ("error: " + t) if is_error is True else t


def _a_block_text(b):
    if not isinstance(b, dict):
        return ""
    if "text" in b:
        return _a_text(b.get("text"))
    if "content" in b:
        return _a_content_text(b.get("content"))
    return ""


def _a_image_part(block):
    src = block.get("source") if isinstance(block, dict) else None
    url = ""
    if isinstance(src, dict):
        st = str(src.get("type") or "")
        if st == "base64":
            url = "data:%s;base64,%s" % (_a_text(src.get("media_type") or "image/png"),
                                         _a_text(src.get("data")))
        elif st == "url":
            url = _a_text(src.get("url"))
        elif st == "file":
            url = _a_text(src.get("file_id") or src.get("url"))
    return {"type": "image_url", "image_url": {"url": url}}


def _a_doc_text(block):
    if not isinstance(block, dict):
        return ""
    title = _a_text(block.get("title"))
    src = block.get("source") if isinstance(block.get("source"), dict) else {}
    st = str(src.get("type") or "")
    body = ""
    if st == "text":
        body = _a_text(src.get("data"))
    elif st == "content":
        body = _a_content_text(src.get("content"))
    elif st == "base64":
        body = "[document %s omitted]" % _a_text(src.get("media_type") or "app/octet-stream")
    elif st in ("url", "file"):
        body = _a_text(src.get("url") or src.get("file_id"))
    return (title + "\n" + body) if title else body


def _a_system_text(v):
    if isinstance(v, str):
        return v
    if isinstance(v, list):
        return "\n".join(_a_text(i.get("text")) for i in v
                         if isinstance(i, dict) and _a_text(i.get("text")))
    return ""


def _a_tool_choice(v):
    if isinstance(v, str):
        return {"auto": "auto", "any": "required", "none": "none",
                "required": "required"}.get(v.strip().lower(), v)
    if isinstance(v, dict):
        t = str(v.get("type") or "").strip().lower()
        if t == "auto":
            return "auto"
        if t == "any":
            return "required"
        if t == "none":
            return "none"
        if t == "tool":
            return {"type": "function", "function": {"name": _a_text(v.get("name"))}}
    return None


def anthropic_tools_to_chat(tools):
    out, skipped = [], []
    if not isinstance(tools, list):
        return out, skipped
    for it in tools:
        if not isinstance(it, dict):
            continue
        if isinstance(it.get("function"), dict):
            out.append(it)
            continue
        name = _a_text(it.get("name") or it.get("type"))
        schema = it.get("input_schema")
        if schema is None:
            schema = it.get("parameters")
        if schema is None:
            if name:
                skipped.append(name)
            continue
        out.append({"type": "function",
                    "function": {"name": name, "description": it.get("description") or "",
                                 "parameters": schema}})
    return out, skipped


def anthropic_effort(payload):
    if not isinstance(payload, dict):
        return None
    oc = payload.get("output_config")
    if isinstance(oc, dict):
        e = _a_text(oc.get("effort")).strip()
        if e:
            return e
    th = payload.get("thinking")
    if not isinstance(th, dict):
        return None
    if _a_text(th.get("type")).strip().lower() != "enabled":
        return None
    try:
        b = int(th.get("budget_tokens") or 0)
    except Exception:
        return None
    if b <= 0:
        return None
    if b >= 32000:
        return "xhigh"
    if b >= 16000:
        return "high"
    if b >= 8000:
        return "medium"
    return "low"


def _a_blocks_to_messages(role, blocks):
    calls, tmsgs, parts, texts = [], [], [], []
    non_text = False
    for b in blocks:
        if not isinstance(b, dict):
            continue
        typ = str(b.get("type") or "")
        if typ in ("thinking", "redacted_thinking"):
            continue
        if typ in ("tool_use", "server_tool_use"):
            calls.append({"id": _a_text(b.get("id")), "type": "function",
                          "function": {"name": _a_text(b.get("name")),
                                       "arguments": _a_json_text(b.get("input"))}})
            continue
        if typ == "tool_result":
            tmsgs.append({"role": "tool", "tool_call_id": _a_text(b.get("tool_use_id")),
                          "content": _a_content_text(b.get("content"), b.get("is_error"))})
            continue
        if typ == "image":
            p = _a_image_part(b)
            if not (p.get("image_url") or {}).get("url"):
                continue
            non_text = True
            parts.append(p)
            continue
        if typ == "document":
            non_text = True
            parts.append({"type": "text", "text": _a_doc_text(b)})
            continue
        t = _a_block_text(b)
        texts.append(t)
        parts.append({"type": "text", "text": t})
    out = list(tmsgs)
    if not calls and not texts and not non_text:
        return out
    m = {"role": role or "user"}
    if calls:
        m["tool_calls"] = calls
    m["content"] = parts if non_text else ("".join(texts) if texts else "")
    out.append(m)
    return out


def messages_to_chat(payload):
    if not isinstance(payload, dict):
        raise ValueError("request body must be a JSON object")
    model = _a_text(payload.get("model")).strip()
    if not model:
        raise ValueError("model is required")
    msgs_in = payload.get("messages")
    if not isinstance(msgs_in, list) or not msgs_in:
        raise ValueError("messages must be a non-empty list")
    chat = {"model": model, "messages": []}
    for k in ("temperature", "top_p"):
        if payload.get(k) is not None:
            chat[k] = payload[k]
    if payload.get("max_tokens") is not None:
        chat["max_tokens"] = payload["max_tokens"]
    if payload.get("stream") is not None:
        chat["stream"] = bool(payload.get("stream"))
    stops = payload.get("stop_sequences")
    if isinstance(stops, list) and stops:
        chat["stop"] = stops
    md = payload.get("metadata")
    if isinstance(md, dict) and md.get("user_id") is not None:
        chat["user"] = md.get("user_id")
    tools, skipped = anthropic_tools_to_chat(payload.get("tools"))
    if tools:
        chat["tools"] = tools
    ch = _a_tool_choice(payload.get("tool_choice"))
    if ch is not None:
        chat["tool_choice"] = ch
    rc = payload.get("tool_choice")
    if isinstance(rc, dict) and rc.get("disable_parallel_tool_use") is True:
        chat["parallel_tool_calls"] = False
    eff = anthropic_effort(payload)
    if eff:
        chat["reasoning_effort"] = eff
    messages = []
    system = _a_system_text(payload.get("system"))
    if skipped:
        note = ("These Anthropic server tools are not available here: "
                + ", ".join(skipped) + ". Do not call them.")
        system = (system + "\n" + note) if system else note
    if system:
        messages.append({"role": "system", "content": system})
    for i, item in enumerate(msgs_in):
        if not isinstance(item, dict):
            continue
        role = _a_text(item.get("role")).strip() or "user"
        content = item.get("content")
        if role in ("system", "developer"):
            t = _a_system_text(content) or _a_content_text(content)
            if t:
                messages.append({"role": "system", "content": t})
            continue
        if role not in ("user", "assistant"):
            raise ValueError("messages[%d].role %r must be user or assistant" % (i, role))
        if isinstance(content, str):
            messages.append({"role": role, "content": content})
        elif isinstance(content, list):
            messages.extend(_a_blocks_to_messages(role, content))
        elif content is not None:
            t = _a_text(content)
            if t:
                messages.append({"role": role, "content": t})
    chat["messages"] = messages
    return chat


def _a_usage(u):
    out = {"input_tokens": 0, "cache_creation_input_tokens": 0,
           "cache_read_input_tokens": 0, "output_tokens": 0}
    if not isinstance(u, dict):
        return out
    try:
        out["input_tokens"] = int(u.get("prompt_tokens") or 0)
    except Exception:
        pass
    try:
        out["output_tokens"] = int(u.get("completion_tokens") or 0)
    except Exception:
        pass
    try:
        out["cache_read_input_tokens"] = int(u.get("prompt_cache_hit_tokens") or 0)
    except Exception:
        pass
    try:
        out["cache_creation_input_tokens"] = int(u.get("prompt_cache_write_tokens") or 0)
    except Exception:
        pass
    if u.get("service_tier"):
        out["service_tier"] = u["service_tier"]
    return out


def _a_stop_reason(fr):
    v = str(fr or "").strip().lower()
    if v == "length":
        return "max_tokens"
    if v in ("tool_calls", "function_call"):
        return "tool_use"
    if v in ("content_filter", "refusal"):
        return "refusal"
    return "end_turn"


def chat_to_messages(obj):
    if not isinstance(obj, dict):
        obj = {}
    choices = obj.get("choices") if isinstance(obj.get("choices"), list) else []
    message, finish = {}, ""
    if choices and isinstance(choices[0], dict):
        message = choices[0].get("message") if isinstance(choices[0].get("message"), dict) else {}
        finish = choices[0].get("finish_reason") or ""
    content = []
    text = message.get("content")
    if isinstance(text, list):
        text = "".join(_a_block_text(p) for p in text if isinstance(p, dict))
    text = _a_text(text)
    if text:
        content.append({"type": "text", "text": text})
    tcs = message.get("tool_calls")
    if isinstance(tcs, list):
        for c in tcs:
            if not isinstance(c, dict):
                continue
            fn = c.get("function") if isinstance(c.get("function"), dict) else {}
            content.append({"type": "tool_use",
                            "id": _a_text(c.get("id")) or _new_id("toolu_"),
                            "name": _a_text(fn.get("name")),
                            "input": _a_parse_input(fn.get("arguments"))})
    cid = _a_text(obj.get("id"))
    return {"id": cid if cid.startswith("msg_") else ("msg_" + cid if cid else _new_id("msg_")),
            "type": "message", "role": "assistant", "model": _a_text(obj.get("model")),
            "content": content, "stop_reason": _a_stop_reason(finish),
            "stop_sequence": None, "usage": _a_usage(obj.get("usage"))}


def _a_frame(event, payload):
    body = dict(payload or {})
    body["type"] = event
    return ("event: %s\ndata: %s\n\n"
            % (event, json.dumps(body, ensure_ascii=False, separators=(",", ":")))).encode()


def stream_messages_events(chat_line_iter, model, holder):
    holder = holder if isinstance(holder, dict) else {}
    st = {"started": False, "done": False, "failed": False, "id": "", "model": model,
          "stop": "end_turn", "text_open": False, "text_idx": None,
          "next": 0, "tools": {}, "order": [], "usage": None}

    def usage_p():
        return _a_usage(st.get("usage"))

    def ensure_start(chunk=None):
        if st["started"]:
            return None
        st["started"] = True
        if isinstance(chunk, dict):
            st["id"] = _a_text(chunk.get("id"))
            st["model"] = _a_text(chunk.get("model")) or st["model"]
        cid = _a_text(st["id"])
        return _a_frame("message_start", {"message": {
            "id": cid if cid.startswith("msg_") else ("msg_" + cid if cid else _new_id("msg_")),
            "type": "message", "role": "assistant", "content": [], "model": st["model"],
            "stop_reason": None, "stop_sequence": None,
            "usage": {"input_tokens": 0, "output_tokens": 0}}})

    def ensure_text():
        if st["text_open"]:
            return None
        st["text_idx"] = st["next"]
        st["next"] += 1
        st["text_open"] = True
        return _a_frame("content_block_start",
                        {"index": st["text_idx"],
                         "content_block": {"type": "text", "text": ""}})

    for raw in chat_line_iter:
        if st["done"] or st["failed"]:
            break
        line = raw.decode("utf-8", "replace") if isinstance(raw, bytes) else raw
        line = line.strip()
        if not line or line.startswith(":"):
            continue
        while line.startswith("data:"):
            line = line[5:].strip()
        if not line or line == "[DONE]":
            continue
        try:
            chunk = json.loads(line)
        except Exception:
            continue
        if isinstance(chunk.get("error"), dict):
            st["failed"] = True
            yield _a_frame("error", {"error": {"type": "api_error",
                                               "message": _a_text(chunk["error"].get("message"))}})
            return
        s = ensure_start(chunk)
        if s:
            yield s
        if isinstance(chunk.get("usage"), dict):
            st["usage"] = chunk["usage"]
        choices = chunk.get("choices") if isinstance(chunk.get("choices"), list) else []
        if not choices:
            continue
        choice = choices[0] if isinstance(choices[0], dict) else {}
        delta = choice.get("delta") if isinstance(choice.get("delta"), dict) else {}
        text = delta.get("content")
        if isinstance(text, str) and text:
            f = ensure_text()
            if f:
                yield f
            yield _a_frame("content_block_delta",
                           {"index": st["text_idx"],
                            "delta": {"type": "text_delta", "text": text}})
        for call in delta.get("tool_calls") or []:
            if not isinstance(call, dict):
                continue
            try:
                idx = int(call.get("index") or 0)
            except Exception:
                idx = 0
            e = st["tools"].get(idx)
            if e is None:
                e = {"idx": idx, "id": "", "name": "", "opened": False}
                st["tools"][idx] = e
                st["order"].append(idx)
            if call.get("id"):
                e["id"] = _a_text(call.get("id"))
            fn = call.get("function") if isinstance(call.get("function"), dict) else {}
            if fn.get("name"):
                e["name"] = _a_text(fn.get("name"))
            args = fn.get("arguments") if fn.get("arguments") is not None else ""
            if not e["opened"] and (e["id"] or e["name"] or args):
                if st["text_open"]:
                    st["text_open"] = False
                    yield _a_frame("content_block_stop", {"index": st["text_idx"]})
                e["opened"] = True
                e["block"] = st["next"]
                st["next"] += 1
                if not e["id"]:
                    e["id"] = "toolu_" + str(idx)
                yield _a_frame("content_block_start",
                               {"index": e["block"],
                                "content_block": {"type": "tool_use", "id": e["id"],
                                                 "name": e["name"], "input": {}}})
            if e["opened"] and args:
                yield _a_frame("content_block_delta",
                               {"index": e["block"],
                                "delta": {"type": "input_json_delta",
                                          "partial_json": _a_text(args)}})
        if choice.get("finish_reason"):
            st["stop"] = _a_stop_reason(choice["finish_reason"])
    if st["failed"]:
        return
    if not st["started"]:
        s = ensure_start(None)
        if s:
            yield s
    if st["text_open"]:
        st["text_open"] = False
        yield _a_frame("content_block_stop", {"index": st["text_idx"]})
    for idx in st["order"]:
        e = st["tools"][idx]
        if e["opened"]:
            yield _a_frame("content_block_stop", {"index": e["block"]})
    st["done"] = True
    holder["usage"] = st.get("usage")
    yield _a_frame("message_delta", {"delta": {"stop_reason": st["stop"],
                                               "stop_sequence": None},
                                     "usage": usage_p()})
    yield _a_frame("message_stop", {})


# ---------------- transport ----------------

def _panel_parts():
    u = urllib.parse.urlparse(PANEL_BASE)
    host = u.hostname or "127.0.0.1"
    port = u.port or (443 if u.scheme == "https" else 80)
    return u.scheme, host, port


def panel_post(path, body_bytes, headers, timeout=600):
    scheme, host, port = _panel_parts()
    conn_cls = http.client.HTTPSConnection if scheme == "https" else http.client.HTTPConnection
    conn = conn_cls(host, port, timeout=timeout)
    conn.request("POST", path, body=body_bytes, headers=headers)
    return conn, conn.getresponse()


def panel_get(path, headers, timeout=30):
    scheme, host, port = _panel_parts()
    conn_cls = http.client.HTTPSConnection if scheme == "https" else http.client.HTTPConnection
    conn = conn_cls(host, port, timeout=timeout)
    conn.request("GET", path, headers=headers)
    resp = conn.getresponse()
    data = resp.read()
    conn.close()
    return resp.status, dict(resp.getheaders()), data


_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_HERE)  # panel repo root (config.json lives here)


def load_adapter_key():
    global PANEL_KEY
    if PANEL_KEY:
        return PANEL_KEY
    # Same host as panel: read panel config.json api_key
    # Server path first (/etc/wb2api/config.json), repo-local fallback
    for cand in ("/etc/wb2api/config.json",
                 os.path.join(_REPO_ROOT, "config.json"),
                 os.path.join(os.getcwd(), "config.json")):
        try:
            with open(cand, "r", encoding="utf-8") as f:
                PANEL_KEY = json.load(f).get("api_key") or ""
                if PANEL_KEY:
                    return PANEL_KEY
        except Exception:
            continue
    return _DEFAULT_PANEL_KEY


def _is_anthropic_route(path):
    p = path.split("?")[0]
    return p in ("/v1/messages", "/messages",
                 "/v1/messages/count_tokens", "/messages/count_tokens")


def _openai_err(status, code, msg):
    return (status, {"Content-Type": "application/json"},
            json.dumps({"error": {"message": msg, "type": "api_error",
                                  "code": code}}, ensure_ascii=False).encode())


def _anthropic_err(status, msg, etype="invalid_request_error"):
    tp = {"invalid_request_error": "invalid_request_error", "authentication_error": "authentication_error",
          "permission_error": "permission_error", "not_found_error": "not_found_error",
          "rate_limit_error": "rate_limit_error", "overloaded_error": "overloaded_error"}.get(etype, etype)
    if status in (400, 422):
        tp = "invalid_request_error" if etype == "invalid_request_error" else tp
    return (status, {"Content-Type": "application/json"},
            json.dumps({"type": "error", "error": {"type": tp, "message": str(msg or "upstream error")}},
                       ensure_ascii=False).encode())


# ---------------- caller key verification ----------------
#
# 两档鉴权（与 zen-gateway 同款，保证「同一把调用密钥走 /v1 与 /adapter 都通」）：
#   1. panel 主密钥 -> 直接放行；
#   2. 其他密钥 -> 拿去向 panel 核实（面板创建的调用密钥只存 sha256，只有 panel 能判）。
# 不这样做的话，面板里创建的 sk-wb2a-... 调用密钥在 /adapter 一律 401——而同样的密钥
# 走 /v1（经 zen）却正常，客户端表现为 provider_auth_failed，极难自查。

_KEY_VERDICTS = {}      # key -> (ok: bool, at: float)
_KEY_VERDICT_TTL = 60.0  # 判定缓存秒数：假密钥最多一次探测/TTL
_KEY_VERDICT_MAX = 512   # 缓存上限（超了整体清空，防无界增长）


def _caller_key(headers):
    """入站请求的调用密钥：Authorization: Bearer 优先，其次 x-api-key（Anthropic 习惯）。"""
    authz = headers.get("Authorization") or ""
    if authz.startswith("Bearer "):
        return authz[7:].strip()
    return (headers.get("x-api-key") or "").strip()


def panel_accepts_key(key):
    """问 panel 这个调用密钥是否有效。

    panel 不可达/超时 -> False（让请求走正常 401，不把 panel 抖动变成放行）。
    TTL 内命中缓存则零额外请求。
    """
    now_t = time.time()
    hit = _KEY_VERDICTS.get(key)
    if hit and now_t - hit[1] < _KEY_VERDICT_TTL:
        return hit[0]
    ok = False
    try:
        st, _, _ = panel_get("/v1/models", {"Authorization": "Bearer " + key}, timeout=8)
        ok = st == 200
    except Exception:
        ok = False
    if len(_KEY_VERDICTS) > _KEY_VERDICT_MAX:
        _KEY_VERDICTS.clear()
    _KEY_VERDICTS[key] = (ok, now_t)
    return ok


class Handler(BaseHTTPRequestHandler):
    server_version = "wk2api-adapter/1.0"

    def log_message(self, *a):
        pass

    def _send(self, status, headers, body):
        data = body if isinstance(body, bytes) else json.dumps(body, ensure_ascii=False).encode()
        self.send_response(status)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        try:
            self.wfile.write(data)
        except Exception:
            pass

    def _auth_ok(self):
        key = load_adapter_key()
        if not key:
            return True  # panel 未设主密钥 = 不鉴权（与 panel 自身口径一致）
        given = _caller_key(self.headers)
        if not given:
            return False
        if hmac.compare_digest(given, key):
            return True
        return panel_accepts_key(given)

    def _upstream_headers(self, extra=None):
        # 调用方已在 _auth_ok 验过，上游一律用 panel 主密钥（与 zen-gateway 的
        # proxyToPanel 同款）：panel 只认主密钥，把客户端的调用密钥转发过去会 401。
        key = load_adapter_key()
        h = {"Content-Type": "application/json"}
        if key:
            h["Authorization"] = "Bearer " + key
        if extra:
            h.update(extra)
        return h

    def do_GET(self):
        path = self.path.split("?")[0]
        if path in ("/", "/healthz"):
            self._send(200, {"Content-Type": "application/json"},
                       {"service": "wk2api-proto-adapter", "panel": PANEL_BASE,
                        "routes": ["POST /v1/chat/completions", "POST /v1/responses",
                                   "POST /v1/messages", "POST /v1/messages/count_tokens",
                                   "GET /v1/models"]})
            return
        if path in ("/v1/models", "/models"):
            if not self._auth_ok():
                s, h, b = _openai_err(401, "invalid_api_key", "missing or invalid API key")
                self._send(s, h, b)
                return
            try:
                st, _, data = panel_get("/v1/models", self._upstream_headers(), timeout=60)
                self._send(st, {"Content-Type": "application/json"}, data)
            except Exception as e:
                s, h, b = _openai_err(502, "upstream_unreachable", str(e))
                self._send(s, h, b)
            return
        s, h, b = _openai_err(404, "not_found", "not found")
        self._send(s, h, b)

    def do_POST(self):
        path = self.path.split("?")[0]
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8") or "{}")
        except Exception:
            if _is_anthropic_route(path):
                s, h, b = _anthropic_err(400, "invalid JSON body")
                self._send(s, h, b)
            else:
                s, h, b = _openai_err(400, "invalid_request", "invalid JSON body")
                self._send(s, h, b)
            return
        if path in ("/v1/messages", "/messages",
                    "/v1/messages/count_tokens", "/messages/count_tokens"):
            if not self._auth_ok():
                s, h, b = _anthropic_err(401, "missing or invalid API key",
                                         "authentication_error")
                self._send(s, h, b)
                return
            if path.endswith("/count_tokens"):
                try:
                    chat = messages_to_chat(payload)
                except ValueError as e:
                    s, h, b = _anthropic_err(400, str(e))
                    self._send(s, h, b)
                    return
                toks = 0
                for m in chat.get("messages") or []:
                    toks += estimate_tokens(json.dumps(m.get("content") or "",
                                                       ensure_ascii=False))
                    for tc in m.get("tool_calls") or []:
                        toks += estimate_tokens(json.dumps(tc, ensure_ascii=False))
                for t in chat.get("tools") or []:
                    toks += estimate_tokens(json.dumps(t, ensure_ascii=False))
                self._send(200, {"Content-Type": "application/json"},
                           {"input_tokens": toks})
                return
            try:
                chat_req = messages_to_chat(payload)
            except ValueError as e:
                s, h, b = _anthropic_err(400, str(e))
                self._send(s, h, b)
                return
            model = chat_req.get("model") or "unknown"
            want_stream = bool(chat_req.get("stream"))
            chat_req["stream"] = want_stream
            self._serve_chat_as_anthropic(chat_req, model, want_stream)
            return
        # OpenAI family: auth first
        if path in ("/v1/chat/completions", "/chat/completions",
                    "/v1/responses", "/responses"):
            if not self._auth_ok():
                s, h, b = _openai_err(401, "invalid_api_key", "missing or invalid API key")
                self._send(s, h, b)
                return
        else:
            s, h, b = _openai_err(404, "not_found", "not found")
            self._send(s, h, b)
            return
        if path in ("/v1/chat/completions", "/chat/completions"):
            self._passthrough_chat(raw)
            return
        # /v1/responses
        if payload.get("previous_response_id"):
            s, h, b = _openai_err(400, "invalid_request",
                                  "previous_response_id is not supported: stateless gateway, "
                                  "send full conversation in 'input' instead")
            self._send(s, h, b)
            return
        try:
            chat_req = responses_to_chat(payload)
        except Exception as e:
            s, h, b = _openai_err(400, "invalid_request", str(e))
            self._send(s, h, b)
            return
        if not chat_req.get("model"):
            s, h, b = _openai_err(400, "invalid_request", "model is required")
            self._send(s, h, b)
            return
        model = payload.get("model")
        want_stream = bool(payload.get("stream"))
        chat_req["stream"] = want_stream
        custom_names = custom_tool_names(payload.get("tools"))
        ns_map = chat_req.pop("_namespace_map", None)
        meta = {"tools": payload.get("tools") or [],
                "tool_choice": payload.get("tool_choice", "auto"),
                "parallel_tool_calls": payload.get("parallel_tool_calls", True)}
        self._serve_chat_as_responses(chat_req, model, want_stream, custom_names, meta, ns_map)

    # ---- upstream plumbing ----
    def _passthrough_chat(self, raw):
        try:
            conn, resp = panel_post("/v1/chat/completions", raw,
                                    self._upstream_headers(), timeout=600)
        except Exception as e:
            s, h, b = _openai_err(502, "upstream_unreachable", str(e))
            self._send(s, h, b)
            return
        ctype = resp.getheader("Content-Type") or "application/json"
        if "text/event-stream" in ctype:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            try:
                while True:
                    chunk = resp.read(8192)
                    if not chunk:
                        break
                    try:
                        self.wfile.write(chunk)
                        self.wfile.flush()
                    except Exception:
                        break
            finally:
                try:
                    conn.close()
                except Exception:
                    pass
            return
        try:
            data = resp.read()
            st = resp.status
            conn.close()
        except Exception as e:
            s, h, b = _openai_err(502, "upstream_unreachable", str(e))
            self._send(s, h, b)
            return
        self._send(st, {"Content-Type": "application/json"}, data)

    def _serve_chat_as_responses(self, chat_req, model, want_stream,
                                 custom_names, meta, ns_map):
        body = json.dumps(chat_req, ensure_ascii=False).encode()
        try:
            conn, resp = panel_post("/v1/chat/completions", body,
                                    self._upstream_headers(), timeout=600)
        except Exception as e:
            s, h, b = _openai_err(502, "upstream_unreachable", str(e))
            self._send(s, h, b)
            return
        ctype = resp.getheader("Content-Type") or ""
        if want_stream:
            if resp.status >= 400:
                data = resp.read()
                conn.close()
                try:
                    err = json.loads(data.decode("utf-8") or "{}")
                    msg = (err.get("error") or {}).get("message") or data.decode()[:500]
                except Exception:
                    msg = data.decode("utf-8", "replace")[:500]
                s, h, b = _openai_err(resp.status if resp.status else 502,
                                      "upstream_error", "upstream %s: %s" % (resp.status, msg))
                self._send(s, h, b)
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()

            def gen():
                buf = b""
                while True:
                    chunk = resp.read(8192)
                    if not chunk:
                        break
                    buf += chunk
                    while b"\n" in buf:
                        ln, buf = buf.split(b"\n", 1)
                        ln = ln.strip()
                        if ln:
                            yield ln + b"\n"
                if buf.strip():
                    yield buf

            holder = {"usage": None, "custom_names": custom_names,
                      "request_meta": meta, "namespace_map": ns_map}
            try:
                for frame in stream_responses_events(gen(), model, holder):
                    try:
                        self.wfile.write(frame)
                        self.wfile.flush()
                    except Exception:
                        break
            finally:
                try:
                    conn.close()
                except Exception:
                    pass
            return
        # Non-stream: panel returns chat.completion JSON
        try:
            data = resp.read()
            st = resp.status
            conn.close()
        except Exception as e:
            s, h, b = _openai_err(502, "upstream_unreachable", str(e))
            self._send(s, h, b)
            return
        if st >= 400:
            self._send(st, {"Content-Type": "application/json"}, data)
            return
        try:
            chat_obj = json.loads(data.decode("utf-8") or "{}")
        except Exception as e:
            s, h, b = _openai_err(502, "upstream_parse", str(e))
            self._send(s, h, b)
            return
        self._send(200, {"Content-Type": "application/json"},
                   json.dumps(chat_to_response(chat_obj, model, custom_names, meta, ns_map),
                              ensure_ascii=False).encode())

    def _serve_chat_as_anthropic(self, chat_req, model, want_stream):
        body = json.dumps(chat_req, ensure_ascii=False).encode()
        try:
            conn, resp = panel_post("/v1/chat/completions", body,
                                    self._upstream_headers(), timeout=600)
        except Exception as e:
            s, h, b = _anthropic_err(502, "upstream unreachable: %s" % e)
            self._send(s, h, b)
            return
        if want_stream:
            if resp.status >= 400:
                data = resp.read()
                conn.close()
                try:
                    err = json.loads(data.decode("utf-8") or "{}")
                    msg = (err.get("error") or {}).get("message") or data.decode()[:500]
                except Exception:
                    msg = data.decode("utf-8", "replace")[:500]
                s, h, b = _anthropic_err(resp.status, "upstream %s: %s" % (resp.status, msg))
                self._send(s, h, b)
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()

            def gen():
                buf = b""
                while True:
                    chunk = resp.read(8192)
                    if not chunk:
                        break
                    buf += chunk
                    while b"\n" in buf:
                        ln, buf = buf.split(b"\n", 1)
                        ln = ln.strip()
                        if ln:
                            yield ln + b"\n"
                if buf.strip():
                    yield buf

            holder = {"usage": None}
            try:
                for frame in stream_messages_events(gen(), model, holder):
                    try:
                        self.wfile.write(frame)
                        self.wfile.flush()
                    except Exception:
                        break
            finally:
                try:
                    conn.close()
                except Exception:
                    pass
            return
        try:
            data = resp.read()
            st = resp.status
            conn.close()
        except Exception as e:
            s, h, b = _anthropic_err(502, "upstream unreachable: %s" % e)
            self._send(s, h, b)
            return
        if st >= 400:
            try:
                err = json.loads(data.decode("utf-8") or "{}")
                msg = (err.get("error") or {}).get("message") or data.decode()[:500]
            except Exception:
                msg = data.decode("utf-8", "replace")[:500]
            s, h, b = _anthropic_err(st, "upstream %s: %s" % (st, msg))
            self._send(s, h, b)
            return
        try:
            chat_obj = json.loads(data.decode("utf-8") or "{}")
        except Exception as e:
            s, h, b = _anthropic_err(502, "upstream stream error: %s" % e)
            self._send(s, h, b)
            return
        self._send(200, {"Content-Type": "application/json"},
                   json.dumps(chat_to_messages(chat_obj), ensure_ascii=False).encode())


def main():
    global PANEL_BASE, PANEL_KEY
    ap = argparse.ArgumentParser(description="wk2api proto adapter (:8788) -> panel (:8888)")
    ap.add_argument("--listen", default=ADAPTER_LISTEN)
    ap.add_argument("--panel", default=PANEL_BASE)
    ap.add_argument("--panel-key", default="")
    args = ap.parse_args()
    PANEL_BASE = args.panel or PANEL_BASE
    if args.panel_key:
        PANEL_KEY = args.panel_key
    if not PANEL_KEY:
        PANEL_KEY = os.environ.get("PANEL_KEY", "") or load_adapter_key()
    host, _, port = args.listen.rpartition(":")
    host = host or "127.0.0.1"
    try:
        port_i = int(port or "8788")
    except ValueError:
        port_i = 8788
    srv = ThreadingHTTPServer((host, port_i), Handler)
    print("adapter listen http://%s:%d -> panel %s" % (host, port_i, PANEL_BASE), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
