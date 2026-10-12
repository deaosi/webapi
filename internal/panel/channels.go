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
