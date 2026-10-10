#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""proto_adapter 纯函数单测: Responses/Anthropic 双向翻译 + 流事件."""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import proto_adapter as P

FAILS = []


def check(name, cond, extra=""):
    print(("PASS " if cond else "FAIL ") + name + (" | " + str(extra) if extra and not cond else ""))
    if not cond:
        FAILS.append(name)


# ---- Responses -> chat ----
c = P.responses_to_chat({"model": "cn:glm-5.2", "input": "hi",
                         "max_output_tokens": 100,
                         "reasoning": {"effort": "high"},
                         "temperature": 0.2})
check("resp-str-input", c["messages"] == [{"role": "user", "content": "hi"}]
      and c["max_tokens"] == 100 and c["reasoning_effort"] == "high"
      and c["temperature"] == 0.2, c)

c = P.responses_to_chat({"model": "m", "instructions": "sys",
                         "input": [{"type": "message", "role": "user",
                                    "content": [{"type": "input_text", "text": "q"}]},
                                   {"type": "function_call", "call_id": "call_1",
                                    "name": "get_w", "arguments": "{}"},
                                   {"type": "function_call_output", "call_id": "call_1",
                                    "output": "sunny"}]})
check("resp-items", c["messages"][0] == {"role": "system", "content": "sys"}
      and c["messages"][1]["content"] == "q"
      and c["messages"][2]["tool_calls"][0]["id"] == "call_1"
      and c["messages"][3] == {"role": "tool", "tool_call_id": "call_1",
                               "content": "sunny"}, c)

c = P.responses_to_chat({"model": "m", "input": "hi",
                         "tools": [{"type": "custom", "name": "apply_patch"}]})
check("resp-custom-downgrade",
      c["tools"][0]["function"]["name"] == "apply_patch"
      and "input" in c["tools"][0]["function"]["parameters"]["properties"], c)

c = P.responses_to_chat({"model": "m", "input": "hi",
                         "tools": [{"type": "namespace", "name": "ns1",
                                    "tools": [{"name": "do_t",
                                               "description": "d",
                                               "parameters": {"type": "object",
                                                              "properties": {}}}]},
                                   {"type": "function",
                                    "function": {"name": "flat_f", "parameters": {}}}]})
check("resp-namespace", c["_namespace_map"] == {"do_t": "ns1"}
      and {t["function"]["name"] for t in c["tools"]} == {"do_t", "flat_f"}, c)

# ---- chat -> Responses ----
chat = {"id": "chatcmpl-1", "model": "m",
        "choices": [{"finish_reason": "stop",
                     "message": {"role": "assistant", "content": "hello",
                                 "reasoning_content": "think",
                                 "tool_calls": [{"id": "call_1", "type": "function",
                                                 "function": {"name": "get_w",
                                                              "arguments": "{}"}}]}}],
        "usage": {"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8}}
r = P.chat_to_response(chat, "m", custom_names=set(),
                       request_meta={"tools": [], "tool_choice": "auto",
                                     "parallel_tool_calls": True}, namespace_map=None)
types = [o["type"] for o in r["output"]]
check("chat->resp", r["object"] == "response" and r["status"] == "completed"
      and r["output_text"] == "hello" and types == ["reasoning", "function_call", "message"]
      and r["usage"]["total_tokens"] == 8, types)

# custom 回胀
chat2 = {"id": "x", "model": "m",
         "choices": [{"finish_reason": "stop",
                      "message": {"role": "assistant", "content": "",
                                  "tool_calls": [{"id": "call_9", "type": "function",
                                                  "function": {"name": "apply_patch",
                                                               "arguments": '{"input":"abc"}'}}]}}]}
r2 = P.chat_to_response(chat2, "m", custom_names={"apply_patch"}, request_meta={})
check("chat->resp-custom", r2["output"][0]["type"] == "custom_tool_call"
      and r2["output"][0]["input"] == "abc", r2["output"][0])

# ---- Responses 流 ----
sse = ('data: {"id":"c1","model":"m","choices":[{"delta":{"content":"he"}}]}\n\n'
       'data: {"id":"c1","model":"m","choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}],'
       '"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}\n\n'
       'data: [DONE]\n\n')
frames = list(P.stream_responses_events(
    [l.encode() + b"\n" for l in sse.split("\n")], "m",
    {"usage": None, "custom_names": set(), "request_meta": {}, "namespace_map": {}}))
evts = []
for f in frames:
    for line in f.decode().split("\n"):
        if line.startswith("data:"):
            evts.append(json.loads(line[5:].strip())["type"])
check("resp-stream", evts[0] == "response.created" and evts[-1] == "response.completed"
      and "response.output_text.delta" in evts, evts)

# ---- Anthropic -> chat ----
a = P.messages_to_chat({"model": "cn:glm-5.2", "max_tokens": 50, "system": "sys",
                        "messages": [{"role": "user",
                                      "content": [{"type": "text", "text": "hi"},
                                                  {"type": "image",
                                                   "source": {"type": "url",
                                                             "url": "https://x/y.png"}}]}],
                        "tools": [{"name": "get_w",
                                   "input_schema": {"type": "object"}}],
                        "tool_choice": {"type": "any"}})
check("msg->chat", a["messages"][0] == {"role": "system", "content": "sys"}
      and a["messages"][1]["role"] == "user"
      and any(p.get("type") == "image_url" for p in a["messages"][1]["content"])
      and a["tools"][0]["function"]["name"] == "get_w"
      and a["tool_choice"] == "required" and a["max_tokens"] == 50, a)

# tool_result + thinking丢弃 + effort映射
a = P.messages_to_chat({"model": "m", "max_tokens": 10,
                        "thinking": {"type": "enabled", "budget_tokens": 20000},
                        "messages": [{"role": "assistant",
                                      "content": [{"type": "thinking", "thinking": "t"},
                                                  {"type": "tool_use", "id": "toolu_1",
                                                   "name": "get_w", "input": {"a": 1}}]},
                                     {"role": "user",
                                      "content": [{"type": "tool_result",
                                                   "tool_use_id": "toolu_1",
                                                   "content": "ok"}]}]})
check("msg-blocks", a["reasoning_effort"] == "high"
      and a["messages"][0]["tool_calls"][0]["id"] == "toolu_1"
      and a["messages"][1] == {"role": "tool", "tool_call_id": "toolu_1",
                               "content": "ok"}, a)

# 非法 role 400
try:
    P.messages_to_chat({"model": "m", "messages": [{"role": "oops", "content": "x"}]})
    check("msg-badrole", False)
except ValueError:
    check("msg-badrole", True)

# ---- chat -> Anthropic ----
m = P.chat_to_messages({"id": "chatcmpl-1", "model": "m",
                        "choices": [{"finish_reason": "tool_calls",
                                     "message": {"role": "assistant", "content": "t",
                                                 "tool_calls": [{"id": "call_1",
                                                                 "type": "function",
                                                                 "function": {"name": "get_w",
                                                                              "arguments": '{"a":1}'}}]}}],
                        "usage": {"prompt_tokens": 4, "completion_tokens": 6,
                                  "total_tokens": 10}})
check("chat->msg", m["stop_reason"] == "tool_use"
      and m["content"][0] == {"type": "text", "text": "t"}
      and m["content"][1] == {"type": "tool_use", "id": "call_1",
                              "name": "get_w", "input": {"a": 1}}
      and m["usage"]["input_tokens"] == 4 and m["usage"]["output_tokens"] == 6, m)

# ---- Anthropic 流 ----
sse2 = ('data: {"id":"c1","model":"m","choices":[{"delta":{"content":"hi"}}]}\n\n'
        'data: {"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1",'
        '"function":{"name":"get_w","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}\n\n'
        'data: [DONE]\n\n')
fr2 = list(P.stream_messages_events(
    [l.encode() + b"\n" for l in sse2.split("\n")], "m", {}))
ev2 = []
for f in fr2:
    for line in f.decode().split("\n"):
        if line.startswith("event:"):
            ev2.append(line.split(":", 1)[1].strip())
check("msg-stream", ev2[0] == "message_start" and ev2[-1] == "message_stop"
      and "content_block_delta" in ev2, ev2)

# ---- count_tokens 估算 ----
check("cjk-est", P.estimate_tokens("hello") >= 1 and P.estimate_tokens("你好") == 3
      and P.estimate_tokens("") == 0, "")

print()
if FAILS:
    print("FAILED:", FAILS)
    sys.exit(1)
print("ALL PASS")
