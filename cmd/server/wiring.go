package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareAvailableForModelScoped 同上，再叠加渠道分组 scope（上游渠道隔离）。
// session 包的新字段 AvailableForModelScoped 用它装配：粘性可用集与首次分配的
// 选号候选同口径过滤，否则分配校验会把请求引到组外账号。
func realmAwareAvailableForModelScoped(p *pool.Pool) func(model, scope string) []string {
	return func(model, scope string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealmGroup(bare, realm, scope)
	}
}

// realmAwareAllocate 会话粘性首次分配的**策略选号**闭包：把"选哪个号"交回
// pool.pickLocked，而不是让会话路由器自行哈希。
//
// 为什么必须接回调：权重（pool.accounts.weight）/ priority 分档 / swrr / 成本分层 /
// 最早到期优先（prefer_expiring）全部只在 pool.pickLocked 内实现。会话路由器自带的
// hashIndex(key, len(available)) 分配会把整条策略旁路——生产表现为面板上调权重与
// 「快过期积分优先」开关都毫无效果，且 /status 的 pick 分布恒为 0（session_sticky
// 默认开启，ExtractKey 又有派生回退，故几乎每个请求都走分配路径）。
//
// 双段语义原样保留：bound（已被其他会话绑定的 uid）作为第一段的排除集，先在空闲号里
// 按策略选；全池皆绑定则放开 bound 再选一次（第二段）。realm 前缀在此剥掉，与
// realmAwareAvailableForModel 同口径，避免跨 realm 泄漏。
// scope（渠道分组）一路透传到组隔离选号：绑组 key 的粘性分配只在同组账号里选。
func realmAwareAllocate(p *pool.Pool) func(model string, bound []string, scope string) string {
	return func(model string, bound []string, scope string) string {
		realm, bare := server.ResolveModel(model)
		excl := make(map[string]bool, len(bound))
		for _, u := range bound {
			excl[u] = true
		}
		if a := p.PickExcludingForRealmGroup(excl, bare, realm, scope); a != nil {
			return a.UID // 第一段：空闲号内按策略选
		}
		if a := p.PickExcludingForRealmGroup(nil, bare, realm, scope); a != nil {
			return a.UID // 第二段：全池皆绑定 → 放开后重选
		}
		return ""
	}
}
