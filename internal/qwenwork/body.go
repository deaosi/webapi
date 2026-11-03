package qwenwork

import (
	"encoding/json"
	"strings"
)

// 默认模型与上下文上限（上游模型目录拉不到时用）。
const (
	// DefaultModel 默认模型 key。
	DefaultModel = "pro"
	// ContextLength 默认上下文窗口。
	ContextLength = 180000
	// MaxCompletionTokens 默认输出上限。
	MaxCompletionTokens = 32000
)

// MapModel 把对外的模型名映射成上游 model_config.key。
//
// 上游只有三个内置 key（pro / flash / qwen3.8-max-preview），其余按原样透传
// ——模型目录是动态的，将来上游加模型不需要改代码。
func MapModel(name string) string {
	switch strings.TrimSpace(name) {
	case "", "auto", "advanced", "pro":
		return "pro"
	case "lite", "flash", "qwen3.8-flash":
		return "flash"
	case "max", "qwen3.8-max", "qwen3.8-max-preview":
		return "qwen3.8-max-preview"
	default:
		return strings.TrimSpace(name)
	}
}

// ChatRequest 从 OpenAI 请求体里取本包需要的字段。
type ChatRequest struct {
	Model               string           `json:"model"`
	Messages            []map[string]any `json:"messages"`
	Tools               []map[string]any `json:"tools"`
	ToolChoice          any              `json:"tool_choice"`
	Temperature         *float64         `json:"temperature"`
	TopP                *float64         `json:"top_p"`
	PresencePenalty     *float64         `json:"presence_penalty"`
	FrequencyPenalty    *float64         `json:"frequency_penalty"`
	MaxTokens           *int             `json:"max_tokens"`
	MaxCompletionTokens *int             `json:"max_completion_tokens"`
	Stream              bool             `json:"stream"`
}

// ParseChatRequest 解析 OpenAI 请求体。
func ParseChatRequest(raw []byte) (ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	return req, nil
}

// contentText 把 OpenAI 的 content（字符串或多段数组）压成纯文本。
func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		bs, _ := json.Marshal(v)
		return string(bs)
	}
}

// BuildBody 组装上游请求体（字段与官方客户端一致）。
//
// 上游只认 chat_context.text（最后一条用户消息）+ messages（历史）+ system，
// 不是标准 OpenAI 结构，所以必须重建而不是转发原 body。
func BuildBody(req ChatRequest, modelKey string) ([]byte, error) {
	key := MapModel(modelKey)
	if key == "" {
		key = DefaultModel
	}

	var sysParts []string
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m == nil {
			continue
		}
		if role, _ := m["role"].(string); role == "system" {
			if txt := contentText(m["content"]); txt != "" {
				sysParts = append(sysParts, txt)
			}
			continue
		}
		msgs = append(msgs, m)
	}

	lastUser := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if role, _ := msgs[i]["role"].(string); role == "user" {
			lastUser = contentText(msgs[i]["content"])
			break
		}
	}
	if lastUser == "" {
		for _, m := range msgs {
			if role, _ := m["role"].(string); role == "user" {
				if txt := contentText(m["content"]); txt != "" {
					lastUser = txt
					break
				}
			}
		}
	}
	if lastUser == "" {
		lastUser = "ping"
	}

	params := map[string]any{}
	for name, v := range map[string]*float64{
		"temperature":       req.Temperature,
		"top_p":             req.TopP,
		"presence_penalty":  req.PresencePenalty,
		"frequency_penalty": req.FrequencyPenalty,
	} {
		if v != nil {
			params[name] = *v
		}
	}
	tokenCap := req.MaxTokens
	if req.MaxCompletionTokens != nil {
		tokenCap = req.MaxCompletionTokens
	}
	if tokenCap != nil && *tokenCap > 0 {
		params["max_tokens"] = *tokenCap
	} else {
		params["max_tokens"] = MaxCompletionTokens
	}

	tools := []map[string]any{}
	if !isToolChoiceNone(req.ToolChoice) && len(req.Tools) > 0 {
		tools = req.Tools
	}

	reqID, err := uuidV4()
	if err != nil {
		return nil, err
	}
	sessionID, err := uuidV4()
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"request_id":     reqID,
		"request_set_id": reqID,
		"chat_record_id": reqID,
		"session_id":     sessionID,
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     lastUser,
			"features": []any{},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": key, "is_reasoning": false},
				"originalContent": lastUser,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":          true,
		"is_retry":          false,
		"source":            1,
		"version":           "3",
		"agent_id":          "agent_common",
		"task_id":           "common",
		"session_type":      "qoder_work",
		"aliyun_user_type":  "",
		"model_config": map[string]any{
			"key":              key,
			"display_name":     key,
			"model":            "",
			"format":           "openai",
			"is_vl":            true,
			"is_reasoning":     false,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": ContextLength,
		},
		"system":   strings.Join(sysParts, "\n\n"),
		"messages": msgs,
		"tools":    tools,
		"parameters": params,
		"business": map[string]any{
			"product":          businessProd,
			"type":             businessType,
			"version":          "1",
			"feature_switches": map[string]any{},
		},
	}
	return json.Marshal(body)
}

func isToolChoiceNone(v any) bool {
	s, ok := v.(string)
	return ok && s == "none"
}
