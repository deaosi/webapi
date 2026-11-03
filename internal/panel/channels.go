// 渠道页辅助端点：拉取外部上游端点的模型表。
//
// 存在理由：外部上游的可用模型只有上游自己知道（网关静态配置里没有这份名单），
// 而渠道白名单决定"哪些模型走外部上游、哪些回落账号池"。让运维去翻上游文档手抄
// 模型名，必然出现拼错/漏项，然后表现为"明明配了却不生效"。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"
)

// channelUpstreamModels 处理 POST /panel/api/channels/upstream_models。
//
// 请求体 {"channel": "<渠道名>"} → 响应 {"channel": ..., "models": [...]}。
// 只按渠道名间接取上游信息（地址与密钥都留在 main 侧），请求体里的地址/密钥一律不采信
// ——否则这就成了一个人人可用的任意 URL 抓取器。
func (p *Panel) channelUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if p.cfg.FetchUpstreamModels == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "当前进程未启用上游模型拉取",
		})
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Channel)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel 不能为空"})
		return
	}
	models, err := p.cfg.FetchUpstreamModels(name)
	if err != nil {
		// 原样带出上游错误：这里最常见的失败是地址写错（404）或密钥无效（401），
		// 面板把它直接显示给运维，比"拉取失败"四个字有用得多。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "拉取上游模型失败：" + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": name, "models": models})
}

// channelTestUpstream 处理 POST /panel/api/channels/test_upstream。
//
// 请求体 {"channel": "<渠道名>"}；响应 = 上游探测结果（ok/status/elapsed/model/body）。
// 与拉取模型同一个安全口径：只按渠道名间接取地址与密钥，请求体里的 URL/key 一律不采信。
//
// 响应一律 200（探测本身失败也是"成功的探测"）：运维要看的是上游回了什么，
// 而不是网关把上游的 402 又包成了一个 502。
func (p *Panel) channelTestUpstream(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TestUpstream == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "当前进程未启用上游探测",
		})
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Channel)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel 不能为空"})
		return
	}
	res := p.cfg.TestUpstream(name)
	if res == nil {
		res = map[string]any{"ok": false, "error": "探测未返回结果"}
	}
	res["channel"] = name
	writeJSON(w, http.StatusOK, res)
}

// qwenworkDeviceStart 处理 POST /panel/api/channels/qwenwork/device_start。
//
// 千问办公没有静态 API key：加账号靠设备授权（生成授权页链接 → 运维在浏览器里确认 →
// 轮询换到 access/refresh token）。本端点只生成链接与会话参数，不碰任何已有凭据。
func (p *Panel) qwenworkDeviceStart(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QwenworkDeviceStart == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前进程未启用千问办公授权"})
		return
	}
	info, err := p.cfg.QwenworkDeviceStart()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "生成授权链接失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// qwenworkDevicePoll 处理 POST /panel/api/channels/qwenwork/device_poll。
//
// 请求体 {"nonce": "...", "verifier": "..."}；响应 {"pending": true} 或
// {"pending": false, "account": {...}}。上游还在等确认时返回 pending，
// 前端按 2 秒间隔轮询即可。
func (p *Panel) qwenworkDevicePoll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QwenworkDevicePoll == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前进程未启用千问办公授权"})
		return
	}
	var req struct {
		Channel  string `json:"channel"`
		Nonce    string `json:"nonce"`
		Verifier string `json:"verifier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if strings.TrimSpace(req.Nonce) == "" || strings.TrimSpace(req.Verifier) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nonce / verifier 不能为空"})
		return
	}
	// channel 非空 = 拿到令牌后立刻写进该渠道（面板自己落盘，前端不需要、
	// 也不允许回传令牌）。空 = 只换令牌不落库（"先看看能不能授权"）。
	res, err := p.cfg.QwenworkDevicePoll(strings.TrimSpace(req.Channel), req.Nonce, req.Verifier)
	if err != nil {
		// 轮询期的网络抖动很常见，用 200 + error 让前端继续轮询而不是弹红。
		writeJSON(w, http.StatusOK, map[string]any{"pending": true, "error": err.Error()})
		return
	}
	if res == nil {
		res = map[string]any{"pending": true}
	}
	writeJSON(w, http.StatusOK, res)
}

// qwenworkModels 处理 POST /panel/api/channels/qwenwork/models。
//
// 请求体 {"channel": "<渠道名>"}：按渠道名取账号去拉上游模型目录（与外部渠道的
// upstream_models 同口径——请求体里的地址/token 一律不采信）。
func (p *Panel) qwenworkModels(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QwenworkModels == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前进程未启用千问办公模型拉取"})
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Channel)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel 不能为空"})
		return
	}
	models, err := p.cfg.QwenworkModels(name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "拉取千问办公模型失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": name, "models": models})
}

// qwenworkRemoveAccount 处理 POST /panel/api/channels/qwenwork/account_remove。
//
// 请求体 {"channel": "...", "index": N}：按序号删除一个已授权账号。序号来自 /status
// 的 accounts[].index（面板只持有脱敏令牌，删除动作必须由服务端按序号完成）。
func (p *Panel) qwenworkRemoveAccount(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QwenworkRemoveAccount == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前进程未启用千问办公账号管理"})
		return
	}
	var req struct {
		Channel string `json:"channel"`
		Index   int    `json:"index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Channel)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel 不能为空"})
		return
	}
	left, err := p.cfg.QwenworkRemoveAccount(name, req.Index)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": name, "left": left})
}

// qwenworkAddAccount 处理 POST /panel/api/channels/qwenwork/account_add。
//
// 请求体 {"channel","access_token","refresh_token"}；响应 {ok,channel,account}。
// 令牌只在这一跳出现：面板不落日志、不回显，落库后由服务端持有。
func (p *Panel) qwenworkAddAccount(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QwenworkAddAccount == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前进程未启用千问办公账号写入"})
		return
	}
	var req struct {
		Channel      string `json:"channel"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Channel)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel 不能为空"})
		return
	}
	acc, err := p.cfg.QwenworkAddAccount(name, req.AccessToken, req.RefreshToken)
	if err != nil {
		// 上游原话直接透出：粘错令牌时用户要看到"Invalid credential"而不是"添加失败"。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": name, "account": acc})
}
