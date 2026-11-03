package qwenwork

// 本文件是「千问办公」的 HTTP 客户端：设备授权、令牌刷新、模型目录、对话调用。
// 协议与签名见 sign.go 顶部注释；除刷新令牌与用户信息外，所有请求都要带
// SignHeaders 生成的 cosy 签名头。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout 单次上游请求超时（对话是 SSE，给足时间）。
	DefaultTimeout = 180 * time.Second
	// maxBodyBytes 读取上游响应体的上限，防止异常大包打爆内存。
	maxBodyBytes = 4 << 20
)

// Model 上游模型目录条目（字段与 /algo/api/v2/model/list 对齐）。
type Model struct {
	// Key 上游 model_config.key，也是对外暴露的模型名。
	Key string `json:"key"`
	// Name 展示名。
	Name string `json:"name,omitempty"`
	// Reasoning 是否为推理模型。
	Reasoning bool `json:"reasoning,omitempty"`
	// VL 是否支持图片。
	VL bool `json:"vision,omitempty"`
	// MaxInput 输入上限（token）。
	MaxInput int `json:"max_input_tokens,omitempty"`
	// MaxOutput 输出上限（token）。
	MaxOutput int `json:"max_output_tokens,omitempty"`
	// Credits 计费说明（上游给的字符串）。
	Credits string `json:"credits,omitempty"`
	// Description 描述。
	Description string `json:"description,omitempty"`
}

// Client 千问办公上游客户端。零值可用（默认 BaseURL + 默认超时）。
type Client struct {
	// BaseURL 上游基址，留空用 BaseURL 常量。
	BaseURL string
	// HTTP 自定义 http.Client，留空用默认（DefaultTimeout）。
	HTTP *http.Client
}

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(orDefault(c.BaseURL, BaseURL), "/") + path
}

func (c *Client) hc() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c *Client) signedRequest(ctx context.Context, acc Account, method, path string, body []byte, modelKey, accept string) (*http.Request, error) {
	raw := c.endpoint(path)
	headers, err := SignHeaders(acc, body, raw, modelKey, accept)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// Chat 发起一次对话请求，返回未读取的 SSE 响应。调用方负责关闭 Body。
func (c *Client) Chat(ctx context.Context, acc Account, body []byte, modelKey string) (*http.Response, error) {
	req, err := c.signedRequest(ctx, acc, http.MethodPost, ChatPath, body, modelKey, "text/event-stream")
	if err != nil {
		return nil, err
	}
	return c.hc().Do(req)
}

// ListModels 拉取上游模型目录。
func (c *Client) ListModels(ctx context.Context, acc Account) ([]Model, error) {
	req, err := c.signedRequest(ctx, acc, http.MethodGet, ModelListPath, nil, "", "application/json")
	if err != nil {
		return nil, err
	}
	resp, err := c.hc().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	return ParseModels(raw)
}

// ParseModels 解析模型目录响应。上游把列表放在顶层 qwork，也见过包一层 data。
func ParseModels(raw []byte) ([]Model, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("模型目录不是合法 JSON: %w", err)
	}
	list, _ := top["qwork"].([]any)
	if list == nil {
		if d, ok := top["data"].(map[string]any); ok {
			list, _ = d["qwork"].([]any)
		}
	}
	out := make([]Model, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key := strings.TrimSpace(str(m["key"]))
		if key == "" {
			continue
		}
		if en, ok := m["enable"].(bool); ok && !en {
			continue
		}
		out = append(out, Model{
			Key:         key,
			Name:        str(m["display_name"]),
			Reasoning:   boolOf(m["is_reasoning"]),
			VL:          boolOf(m["is_vl"]),
			MaxInput:    intOf(m["max_input_tokens"]),
			MaxOutput:   intOf(m["max_output_tokens"]),
			Credits:     str(m["credits"]),
			Description: str(m["description"]),
		})
	}
	return out, nil
}

// UserInfo 读取账号身份（昵称/邮箱），用于面板展示。失败不影响可用性。
func (c *Client) UserInfo(ctx context.Context, acc Account) (uid, name, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(UserInfoPath), nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("authorization", "Bearer "+acc.AccessToken)
	resp, err := c.hc().Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("用户信息 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", "", err
	}
	if d, ok := body["data"].(map[string]any); ok {
		body = d
	}
	return str(body["id"]), str(body["name"]), str(body["email"]), nil
}

// Refresh 用 refresh_token 换新令牌。成功后调用方应把返回值覆盖旧账号。
func (c *Client) Refresh(ctx context.Context, acc Account) (Account, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return acc, fmt.Errorf("账号没有 refresh_token，无法续期")
	}
	payload, _ := json.Marshal(map[string]string{"refresh_token": acc.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(RefreshPath), bytes.NewReader(payload))
	if err != nil {
		return acc, err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", userAgent)
	resp, err := c.hc().Do(req)
	if err != nil {
		return acc, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return acc, fmt.Errorf("刷新令牌 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var g grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return acc, fmt.Errorf("刷新令牌响应非法: %w", err)
	}
	token := firstOf(g.Token, g.DeviceToken)
	if token == "" {
		return acc, fmt.Errorf("刷新令牌响应里没有 token")
	}
	out := acc
	out.AccessToken = token
	if g.RefreshToken != "" {
		out.RefreshToken = g.RefreshToken
	}
	out.ExpiresAt = expiryUnix(g)
	InvalidateSession(acc)
	return out, nil
}

// DeviceFlow 一次设备授权会话（面板把 URL 显示给运维点，然后轮询换令牌）。
type DeviceFlow struct {
	// Verifier PKCE 校验串，轮询时要带。
	Verifier string `json:"verifier"`
	// Nonce 本次授权的随机串。
	Nonce string `json:"nonce"`
	// MachineID 设备 id（同一账号复用有利于上游风控）。
	MachineID string `json:"machine_id"`
	// URL 授权页地址。
	URL string `json:"url"`
}

// NewDeviceFlow 生成一次设备授权会话。
func NewDeviceFlow() (DeviceFlow, error) {
	verifier, err := randomVerifier(64)
	if err != nil {
		return DeviceFlow{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	nonce, err := uuidV4()
	if err != nil {
		return DeviceFlow{}, err
	}
	machine, err := uuidV4()
	if err != nil {
		return DeviceFlow{}, err
	}
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machine)
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	return DeviceFlow{
		Verifier:  verifier,
		Nonce:     nonce,
		MachineID: machine,
		URL:       DeviceAuthURL + "?" + q.Encode(),
	}, nil
}

// PollGrant 轮询授权结果。pending=true 表示还没确认（上游回 404/202）。
func (c *Client) PollGrant(ctx context.Context, flow DeviceFlow) (acc Account, pending bool, err error) {
	q := url.Values{}
	q.Set("nonce", flow.Nonce)
	q.Set("verifier", flow.Verifier)
	q.Set("challenge_method", "S256")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(PollPath)+"?"+q.Encode(), nil)
	if err != nil {
		return Account{}, false, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", userAgent)
	resp, err := c.hc().Do(req)
	if err != nil {
		return Account{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return Account{}, true, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return Account{}, false, fmt.Errorf("授权轮询 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var g grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return Account{}, false, fmt.Errorf("授权轮询响应非法: %w", err)
	}
	token := firstOf(g.Token, g.DeviceToken)
	if token == "" {
		return Account{}, true, nil
	}
	return Account{
		UID:          g.UserID,
		Nickname:     g.UserName,
		AccessToken:  token,
		RefreshToken: g.RefreshToken,
		ExpiresAt:    expiryUnix(g),
	}, false, nil
}

// grant 设备授权/刷新的响应体。
type grant struct {
	Token        string `json:"token"`
	DeviceToken  string `json:"device_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
}

// expiryUnix 从 expires_at（ISO）或 expires_in（秒/毫秒）算出 unix 秒。
func expiryUnix(g grant) int64 {
	if g.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, g.ExpiresAt); err == nil {
			return t.Unix()
		}
	}
	if g.ExpiresIn > 0 {
		in := g.ExpiresIn
		if in > 10_000_000 { // 毫秒
			in /= 1000
		}
		return time.Now().Unix() + in
	}
	return time.Now().Add(30 * 24 * time.Hour).Unix()
}

func randomVerifier(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
