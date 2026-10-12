package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelExplicit(model)
	return realm, bare
}

// resolveModelExplicit 同 resolveModel，额外返回前缀是否**显式**给出。
//
// 为什么需要这个信号：裸名缺省按 cn（零回归），但渠道声明了 realm=global 时，
// 裸名应当采用渠道的域而不是被缺省 cn 顶掉；而显式 `cn:` 与渠道 global 冲突才是
// 真正的越权请求、必须拒绝。两者只有"是否显式"这一个差别。
func resolveModelExplicit(model string) (realm, bare string, explicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
