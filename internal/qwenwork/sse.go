package qwenwork

// 上游 SSE 是「信封套信封」：每个 data: 行形如
//
//	{"statusCodeValue":200,"body":"{\"choices\":[...]}"}
//
// 内层才是标准 OpenAI chunk。本文件负责拆信封、清掉噪声字段、聚合成非流式响应。
// 规则来自官方客户端（qwenwork2api 的 sse.js），逐条对应。

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// UnwrapFrame 拆一层信封。ok=false 表示这一帧没有内容（心跳/空对象/[DONE]）。
// errMsg 非空表示上游在帧里报了错（statusCodeValue>=400 或内层 code 400/401/403）。
func UnwrapFrame(payload string) (body string, ok bool, errMsg string) {
	text := strings.TrimSpace(payload)
	if text == "" || text == "[DONE]" || text == "{}" || text == "null" {
		return "", false, ""
	}
	var outer map[string]any
	if err := json.Unmarshal([]byte(text), &outer); err != nil {
		// 不是 JSON：当纯文本透传（有些后端直接发文本增量）。
		return text, true, ""
	}
	if outer == nil {
		return "", false, ""
	}
	if _, has := outer["choices"]; has {
		return text, true, ""
	}
	if obj, _ := outer["object"].(string); obj == "chat.completion.chunk" || obj == "chat.completion" {
		return text, true, ""
	}
	if code, okNum := outer["statusCodeValue"].(float64); okNum && code >= 400 {
		return "", false, bodyMessage(toString(outer["body"]), int(code))
	}
	inner, has := outer["body"]
	if !has {
		return "", false, ""
	}
	switch v := inner.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "[DONE]" || s == "{}" || s == "null" {
			return "", false, ""
		}
		if msg := bodyMessage(s, 0); msg != "" {
			return "", false, msg
		}
		return s, true, ""
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false, ""
		}
		s := string(b)
		if s == "{}" || s == "[]" {
			return "", false, ""
		}
		return s, true, ""
	}
	return "", false, ""
}

// bodyMessage 从内层 body 里取错误文案；code 400/401/403 或 HTTP>=400 才算错。
func bodyMessage(raw string, status int) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(raw), &inner); err != nil {
		if status >= 400 {
			return "上游 HTTP " + itoa(status) + ": " + trim200(raw)
		}
		return ""
	}
	code := toString(inner["code"])
	msg := toString(inner["message"])
	if code == "400" || code == "401" || code == "403" {
		if msg != "" {
			return msg
		}
		return "上游返回 code=" + code
	}
	if status >= 400 {
		if msg != "" {
			return msg
		}
		return "上游 HTTP " + itoa(status) + ": " + trim200(raw)
	}
	return ""
}

// CleanChunk 清掉上游塞进来的噪声：usage.raw_usage/sub_usages、delta 里的空字段。
// 返回空串表示整帧都可以丢掉（例如只有空 delta 的心跳）。
func CleanChunk(text string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return text
	}
	if obj == nil {
		return text
	}
	changed := false
	if usage, ok := obj["usage"].(map[string]any); ok {
		for _, noise := range []string{"raw_usage", "sub_usages"} {
			if _, has := usage[noise]; has {
				delete(usage, noise)
				changed = true
			}
			if _, has := obj[noise]; has {
				delete(obj, noise)
				changed = true
			}
		}
		if changed {
			b, err := json.Marshal(obj)
			if err != nil {
				return text
			}
			return string(b)
		}
		return text
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if tc, ok := delta["tool_calls"].([]any); ok && len(tc) == 0 {
			delete(delta, "tool_calls")
			changed = true
		}
		for _, noise := range []string{"content", "reasoning_content", "function_call", "refusal", "extra_fields"} {
			if v, has := delta[noise]; has && isEmptyValue(v) {
				delete(delta, noise)
				changed = true
			}
		}
		if len(delta) == 0 {
			if fr, _ := choice["finish_reason"].(string); fr == "" {
				return ""
			}
		}
	}
	if !changed {
		return text
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return text
	}
	return string(b)
}

// Aggregate 把清理过的 chunk 合成一条非流式响应。
func Aggregate(chunks []string, model string) map[string]any {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	calls := map[int]map[string]any{}
	var order []int

	for _, text := range chunks {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(text), &chunk); err != nil || chunk == nil {
			continue
		}
		if s, ok := chunk["id"].(string); ok && s != "" {
			respID = s
		}
		if s, ok := chunk["model"].(string); ok && s != "" {
			respModel = s
		}
		if n, ok := chunk["created"].(float64); ok {
			created = int64(n)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if s, ok := delta["role"].(string); ok && s != "" {
					role = s
				}
				if s, ok := delta["content"].(string); ok {
					content += s
				}
				if s, ok := delta["reasoning_content"].(string); ok {
					reasoning += s
				}
				if list, ok := delta["tool_calls"].([]any); ok {
					for _, item := range list {
						call, ok := item.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if n, ok := call["index"].(float64); ok {
							idx = int(n)
						}
						merged, seen := calls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							calls[idx] = merged
							order = append(order, idx)
						}
						mergeToolCall(merged, call)
					}
				}
			}
			if s, ok := choice["finish_reason"].(string); ok && s != "" {
				finish = s
			}
		}
	}

	message := map[string]any{"role": firstOf(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(order) > 0 {
		sort.Ints(order)
		list := make([]any, 0, len(order))
		for _, idx := range order {
			list = append(list, calls[idx])
		}
		message["tool_calls"] = list
	}
	if finish == "" {
		finish = "stop"
	}
	result := map[string]any{
		"id":      firstOf(respID, "chatcmpl-qwenwork"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstOf(respModel, model),
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if created == 0 {
		result["created"] = nowUnix()
	}
	if usage != nil {
		result["usage"] = usage
	}
	return result
}

// StreamTo 逐行拆信封并把干净的 chunk 写给客户端，结束时补 [DONE]。
// flush 每写一帧调用一次（http.Flusher）。
func StreamTo(dst io.Writer, src io.Reader, model string, flush func()) (usage map[string]any, err error) {
	reader := bufio.NewReaderSize(src, 64<<10)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				body, ok, errMsg := UnwrapFrame(trimmed[5:])
				if errMsg != "" {
					return usage, &UpstreamError{Message: errMsg}
				}
				if ok {
					if u := usageOf(body); u != nil {
						usage = u
					}
					if cleaned := CleanChunk(body); cleaned != "" {
						if _, werr := io.WriteString(dst, "data: "+cleaned+"\n\n"); werr != nil {
							return usage, werr
						}
						if flush != nil {
							flush()
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return usage, readErr
		}
	}
	if _, err := io.WriteString(dst, "data: [DONE]\n\n"); err != nil {
		return usage, err
	}
	if flush != nil {
		flush()
	}
	return usage, nil
}

// UpstreamError 上游在流里报的错（已去掉信封）。
type UpstreamError struct {
	// Message 上游错误文案。
	Message string
}

func (e *UpstreamError) Error() string { return e.Message }

func nowUnix() int64 { return time.Now().Unix() }

func usageOf(body string) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return nil
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		return u
	}
	return nil
}

func mergeToolCall(merged, delta map[string]any) {
	for _, key := range []string{"id", "type"} {
		if _, has := merged[key]; has {
			continue
		}
		if s, ok := delta[key].(string); ok && s != "" {
			merged[key] = s
		}
	}
	dfn, ok := delta["function"].(map[string]any)
	if !ok {
		return
	}
	fn, _ := merged["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
		merged["function"] = fn
	}
	if s, ok := dfn["name"].(string); ok && s != "" {
		fn["name"] = toString(fn["name"]) + s
	}
	if s, ok := dfn["arguments"].(string); ok && s != "" {
		fn["arguments"] = toString(fn["arguments"]) + s
	}
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, item := range t {
			if !isEmptyValue(item) {
				return false
			}
		}
		return true
	}
	return false
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trim200(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
