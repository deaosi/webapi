// 上游渠道（NewAPI 口径）：调用密钥 → 分组 → 渠道账号。
//
// NewAPI 里「渠道」是一个上游端点、token 属于分组、渠道也属于分组，请求按分组匹配到
// 渠道。本网关上游只有 WorkBuddy 一家，渠道因此重新解释为：**一个账号分组 + 该组的上游
// 属性**（realm / 模型白名单 / 启停），成员账号由 pool.accounts[uid].group 决定——
// 与「账号池·分组」列、pool.groups 的 priority/weight 共用同一套分组名，不另建体系。
//
// 隔离口径（已与运营确认）：**严格**。绑定了分组的 key 只走同组账号，组内无可用即失败，
// 不跨组借号（含全冷却兜底与粘性命中两条路径，见 pool.pickLocked / pool.UIDInGroup）。
package server

import "strings"

// ChannelInfo 调用密钥绑定的上游渠道。零值 = 未绑定（不限制，现状行为）。
//
// 两种渠道形态由 UpstreamURL 是否为空决定：
//   - 空：账号池渠道。请求在 Group 指定的账号分组内选号，走 WorkBuddy 上游（既有语义）；
//   - 非空：外部上游渠道。请求**转发**到该 OpenAI 兼容端点，用 UpstreamKey 鉴权，
//     不进账号池、不选号、不消耗账号积分。
type ChannelInfo struct {
	// Group 账号分组名：渠道成员 = pool.accounts[u].group == Group 的账号。
	Group string
	// Label 展示名（面板与错误文案用；不参与路由）。
	Label string
	// Realm 上游域（"" = 不限制；"cn"/"global" = 强制）。裸模型名采用该值，
	// 显式前缀与之冲突则拒绝。
	Realm string
	// Models 模型白名单（空 = 全放开）。支持裸名精确匹配与尾缀 "*" 前缀匹配。
	Models []string
	// Enabled 渠道启停：false 时该 key 的请求直接 403（不静默回落全池——
	// 静默回落会让"停用渠道"变成"越权用别组账号"）。
	Enabled bool
	// UpstreamURL 外部上游基址（如 "https://api.deepseek.com/v1"）。非空 = 外部渠道，
	// 请求转发到它，不再走账号池。
	UpstreamURL string
	// UpstreamKey 外部上游鉴权密钥（转发时替换 Authorization）。
	UpstreamKey string
}

// IsExternal 该渠道是否为外部上游端点（填了 UpstreamURL）。
func (c ChannelInfo) IsExternal() bool { return strings.TrimSpace(c.UpstreamURL) != "" }

// ChannelResolver 解析调用密钥所属渠道。ok=false 表示该 key 未绑定渠道
// （不限制，主密钥与未绑定 key 走这条）。nil = 渠道功能整体关闭。
type ChannelResolver func(keyID string) (ChannelInfo, bool)

// FilterChannelModels 按渠道约束（realm + 白名单）裁剪模型列表条目。
//
// 与 chatCompletions 的拒绝规则同源：列表里只留该 key 真正能调的模型，客户端就不会
// 选到一个必然 403 的名字。条目的 id 形如 "cn:glm-5.2"（realm 前缀是网关侧路由协议），
// 故按 ResolveModel 剥前缀后再做白名单匹配，与 chat 侧同一口径。
func FilterChannelModels(list []map[string]any, ch ChannelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		id, _ := entry["id"].(string)
		realm, bare := ResolveModel(id)
		if ch.Realm != "" && realm != ch.Realm {
			continue
		}
		if !ChannelAllowedModel(ch.Models, bare) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ChannelModelSet 返回该外部渠道**承接**的模型清单——路由判定与 /v1/models 列表共用
// 这一个口径，避免"列表里看得到、请求却被拒"或反过来的错配。
//
//   - 白名单非空：就是白名单（`*` 与 `glm-5*` 这类通配按 ChannelAllowedModel 的规则生效）；
//   - 白名单留空：**自动取上游模型表**（渠道的模型 = 上游提供的模型）。这样绑定之后
//     列表里立刻能看到渠道模型，且"渠道的模型走上游、其余走账号池"两条同时成立。
//
// 留空且拉不到上游模型表时返回 nil（= 暂不承接任何模型）：请求安全回落账号池。
// 上游不可达时宁可不用它，也不能把所有请求都往一个联系不上的上游上导。
func ChannelModelSet(ch ChannelInfo) []string {
	if len(ch.Models) > 0 {
		return ch.Models
	}
	if !ch.IsExternal() {
		return nil
	}
	ids, err := FetchUpstreamModelIDs(ch.UpstreamURL, ch.UpstreamKey, ch.Label, UpstreamModelTimeoutInline)
	if err != nil || len(ids) == 0 {
		return nil
	}
	return ids
}

// ChannelServesModel 报告**外部上游渠道**是否承接这个模型（决定走上游还是回落账号池）。
//
// 与 ChannelAllowedModel 的区别在语义而非实现：后者是"是否放行"（不匹配即拒绝），
// 前者是"归谁服务"（不匹配就交给账号池）。绑定外部渠道的 key 因此可以同时用到
// 两类模型：白名单内的走外部上游，其余继续走 WorkBuddy 账号池。
//
// 判定依据是 ChannelModelSet 解析出的渠道模型集，而不是白名单本身——留空时它等于
// 上游模型表，所以"上游有的模型走上游、只有池子有的模型走池子"天然成立，
// 且与 /v1/models 列出来的东西完全一致。
func ChannelServesModel(ch ChannelInfo, bare string) bool {
	if !ch.IsExternal() {
		return false
	}
	set := ChannelModelSet(ch)
	if len(set) == 0 {
		return false
	}
	return ChannelAllowedModel(set, bare)
}

// ChannelAllowedModel 报告裸模型名是否落在渠道白名单内（空名单 = 全放开）。
//
// 匹配口径（大小写敏感，与模型名一致）：
//   - `*` 结尾 = 前缀匹配（"glm-5*" 命中 "glm-5.2"、"glm-5.3-flash"）；
//   - 其余 = 精确相等。
//
// 只匹配**裸名**：模型名的 realm 前缀是网关侧路由协议，渠道级 realm 单独校验，
// 不混进白名单（否则同一模型要按前缀写两遍）。
func ChannelAllowedModel(models []string, bare string) bool {
	if len(models) == 0 {
		return true
	}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.HasSuffix(m, "*") {
			if strings.HasPrefix(bare, strings.TrimSuffix(m, "*")) {
				return true
			}
			continue
		}
		if m == bare {
			return true
		}
	}
	return false
}
