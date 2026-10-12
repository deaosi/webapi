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
