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

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/qwenwork"
)

// ChannelInfo 调用密钥绑定的上游渠道。零值 = 未绑定（不限制，现状行为）。
//
// 两种渠道形态由 UpstreamURL 是否为空决定：
//   - 空：账号池渠道。请求在 Group 指定的账号分组内选号，走 WorkBuddy 上游（既有语义）；
//   - 非空：外部上游渠道。请求**转发**到该 OpenAI 兼容端点，用 UpstreamKey 鉴权，
//     不进账号池、不选号、不消耗账号积分。
type ChannelInfo struct {
	// Group 账号分组名：渠道成员 = pool.accounts[u].group == Group 的账号。
	Group string
	// Type 渠道类型："pool"（账号池分组）/ "openai"（OpenAI 兼容上游）。
	// 空值按 UpstreamURL 推断，兼容早期只写了地址的配置。
	Type string
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
	// Accounts 千问办公账号（仅 Type == "qwenwork" 时有效）：设备授权得到的
	// access/refresh token。没有静态密钥，鉴权全靠这套 token。
	Accounts []qwenwork.Account
}

// IsExternal 该渠道是否为外部上游端点（填了 UpstreamURL）。
func (c ChannelInfo) IsExternal() bool { return strings.TrimSpace(c.UpstreamURL) != "" }

// KindLabel 渠道类型的中文名（错误文案用）。
func (c ChannelInfo) KindLabel() string {
	switch strings.TrimSpace(c.Type) {
	case "openai":
		return "OpenAI 兼容上游"
	case "qwenwork":
		return "千问办公"
	}
	if c.IsExternal() {
		return "上游渠道"
	}
	return "账号池渠道"
}

// ChannelResolver 解析调用密钥所属渠道。ok=false 表示该 key 未绑定渠道
// （不限制，主密钥与未绑定 key 走这条）。nil = 渠道功能整体关闭。
type ChannelResolver func(keyID string) (ChannelInfo, bool)

// ChannelListResolver 解析调用密钥绑定的**全部**渠道（一个 key 可以绑多个渠道）。
// ok=false / 空列表 = 未绑定任何渠道，走账号池。nil = 未启用多渠道解析。
type ChannelListResolver func(keyID string) ([]ChannelInfo, bool)

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
// poolBare 是账号池目录里的**裸模型名**（剥掉 realm 前缀），用于自动模式下判断
// "这个模型池子里是不是已经有了"。
func ChannelModelSet(ch ChannelInfo, poolBare []string) []string {
	if len(ch.Models) > 0 {
		// 显式白名单：原样生效，**可以**覆盖池子里的同名模型
		//（"我就要这个模型走外部上游"是合法意图，不该被拦）。
		return ch.Models
	}
	// 千问办公：模型目录来自它自己的 /algo/api/v2/model/list（不是 OpenAI 的 /v1/models），
	// 走独立缓存。命中判定与外部渠道同口径：只接管池子里没有的模型。
	if ch.Type == "qwenwork" {
		ids := QwenworkModelIDsCached(ch)
		if len(ids) == 0 {
			return nil
		}
		return withQwenworkAliases(excludePoolModels(ids, poolBare))
	}
	if !ch.IsExternal() {
		return nil
	}
	// 只读缓存（+ 后台补齐），绝不在这里同步等上游：本函数在 chat 判定与 /v1/models
	// 两个热路径上被调用，而 /v1/models 又是 zen 的鉴权依据，阻塞会演变成
	// "合法密钥被判无效"。详见 UpstreamModelIDsCached 的注释。
	ids := UpstreamModelIDsCached(ch.UpstreamURL, ch.UpstreamKey, ch.Label)
	if len(ids) == 0 {
		return nil
	}
	// 自动模式：只接管**池子里没有的**上游模型（纯新增）。
	//
	// 为什么把重名的排除掉：绑定外部渠道的语义是"**增加**一组可用模型"，而不是
	// "把原来好用的模型改道"。若把与池子同名的（glm-5.3-flash 这类）也收过来，
	// 上游一旦欠费/宕机，这些本来正常工作的模型会跟着一起挂——实测踩过这个坑。
	// 要让同名模型改走外部上游，就在白名单里显式写出来。
	return excludePoolModels(ids, poolBare)
}

// excludePoolModels 去掉账号池里已有的同名模型（自动模式只做"纯新增"）。
func excludePoolModels(ids, poolBare []string) []string {
	have := make(map[string]bool, len(poolBare))
	for _, p := range poolBare {
		if p = strings.TrimSpace(p); p != "" {
			have[p] = true
		}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !have[id] {
			out = append(out, id)
		}
	}
	return out
}

// qwenworkAliasOrder 是「客户端常用名 → 千问办公真实模型键」的固定顺序表，
// 与 qwenwork.MapModel 的归一化规则保持一致。
//
// 为什么要并进模型集：上游模型键只有 pro / flash / qwen3.8-max-preview，但 DSH
// 等客户端习惯写 qwen3.8-max、max、auto。不做别名并集，这些名字就会被判成
// 「该渠道不提供此模型」，请求回落账号池后账号侧回 11102 model_blocked，
// 最终对外变成 503 no_healthy_account——看起来像上游宕机，实际只是名字没对上。
var qwenworkAliasOrder = []struct{ alias, target string }{
	{"qwen3.8-max", "qwen3.8-max-preview"},
	{"max", "qwen3.8-max-preview"},
	{"qwen3.8-flash", "flash"},
	{"auto", "pro"},
	{"advanced", "pro"},
	{"lite", "flash"},
}

// withQwenworkAliases 在已发现的模型集上补别名（目标模型存在时才补），顺序稳定。
func withQwenworkAliases(ids []string) []string {
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	out := append([]string(nil), ids...)
	for _, a := range qwenworkAliasOrder {
		if have[a.target] && !have[a.alias] {
			out = append(out, a.alias)
			have[a.alias] = true
		}
	}
	return out
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
func ChannelServesModel(ch ChannelInfo, bare string, poolBare []string) bool {
	if !ch.IsExternal() {
		return false
	}
	set := ChannelModelSet(ch, poolBare)
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
