// 选号：Pick 簇（healthy 成本分层 + 最早到期优先/普通加权 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pickLocked(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pickLocked(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pickLocked(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pickLocked(tried, reqModel, realm)
}

// pickLocked 在 healthy 候选集中按权重策略选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
//
// 命名带 Locked 后缀（同 reviveCoolingLocked/pickEarliestExpiryLocked 惯例）：
// Pool 结构体已有 pick 字段（pickPolicy 策略表），方法不能同名。
func (p *Pool) pickLocked(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		// 惰性清理过期的模型级冷却与成本台账（两者的 map 都不无限膨胀；
		// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now, realm)
	}
	// top5 短名单按权重降序截断（而非 credits 单纯降序）：否则闲置补偿根本进不了
	// 短名单决策，低 credits 但久置的账号会永远排不进 top5。
	// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准，两个阶段权重可比。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 成本分层（reqModel 非空时）：按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	hasTier1 := false
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	// 条件探索（issue #136 方案 a′）：tier 0 垄断层存在（bestTier==0 且 reqModel
	// 非空）且候选含 tier 1（冻结存在）且距上次探索 ≥ 窗口（零值 timer=从未探索
	// →首次满足即探）时，本次 pick 生效层切 tier 1-only——探索=搭车改道，把一个
	// 既有真实用户请求改道给未知号（零新增上游请求；IP 维度零增量，WAF 友好）。
	// 成功 → NoteModelCost 首观测 → 毕业（tier 0/2，下一轮 pick 立即生效）；
	// 失败 → 既有错误策略照常，无探测风暴。
	// hasTier1 复用本循环上方 costTier 的预计算口径（每候选一次的契约不变）。
	// timer 同锁写入：并发 pick 串行进入写锁，只有一个进入者能通过窗口判定
	//（天然防重复探索）。key = realm + "\x1f" + reqModel：同模型名可跨域，
	// 探索节奏按 (域, 模型) 独立；realm==""（Pick 老语义）单独成键。
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	// costTier/modelCostOf 同样每候选只算一次（存入 tier/cost1k），比较器只读缓存字段。
	// 静态权重乘子（w_final = w_base × weight_cfg）：weight_cfg=0 的账号在此剔除
	// （软摘除，三种 pick_mode 通用；不出池，签到/余额/面板照常）。
	ws := make([]weightedCand, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			cfg := routeWeight(p.pick.accounts, e.a.UID)
			if cfg == 0 {
				continue // 软摘除
			}
			wb := p.weightOf(e, maxCredits, now)
			wf := wb * cfg
			p.lastW[e.a.UID] = wf
			ws = append(ws, weightedCand{e: e, w: wf, cost1k: ci})
		}
	}
	if len(ws) == 0 {
		// 成本层内全被软摘除：不回退到摘除号（违背运营意图），直接返回 nil
		// （无可用，与全禁用同口径；兜底路径同样跳过 weight=0，见 pickEarliestExpiryLocked）。
		return nil
	}
	// 等权重洗牌：仅当存在权重相等且候选数超过 top5 时，才对 ws 做 Fisher-Yates
	// 洗牌（且**不消耗 p.randInt64N 注入源**，避免改变 pickWeighted 的确定性语义，
	// 见 TestPickDeterministicViaSetRandomSource）。权重全等或存在并列时，按字典序
	// 截断会让 uid 靠后的账号永远进不了 top5（等权重账号被字典序饿死、LRU 兜底
	// 又只在 top5 内转——惊群集中单号的根因）。洗牌用独立的 time-seeded 源，
	// 只在截断边界制造等权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if ws[i].w == ws[0].w {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	// pick_mode 分发：priority 模式在此按 (组优先级, 账号优先级) 取最小档，
	// 只保留该档成员继续走权重选择；swrr/weighted 全员进入权重选择。
	// 档位是“可用性分组”：该档无 healthy 号（本函数执行到此 ws 非空，故档必有成员）
	// 即命中该档——不存在“跳档”，因为占位的非 healthy 号早在 healthyOf 被过滤。
	if p.pick.mode == PickPriority {
		bestPri := -1
		for _, c := range ws {
			pri := p.effectivePriorityLocked(c.e.a.UID)
			if bestPri < 0 || pri < bestPri {
				bestPri = pri
			}
		}
		kept := ws[:0]
		for _, c := range ws {
			if p.effectivePriorityLocked(c.e.a.UID) == bestPri {
				kept = append(kept, c)
			}
		}
		ws = kept
	}
	// candsAll 同层全候选（权重降序）：供最早到期优先与 LRU 兜底在全量范围选最旧者
	// （避免 top5 字典序截断把等权重靠后账号饿死——惊群根因之一）。
	// Top5 截断已收进各选择函数内部（swrr/priority 不截断，weighted 取前 5）。
	candsAll := make([]*entry, 0, len(ws))
	for _, c := range ws {
		candsAll = append(candsAll, c.e)
	}
	var e *entry
	// 最早到期优先（WorkDaddy 口径）：只在成本层内、配置窗口内存在有效批次的账号中
	// 排序；同到期时间按该批次剩余积分降序。防并发撞号仍优先过滤 minPickGap 内的账号，
	// 优先级候选全部刚被用时才从中选最早者，避免把请求硬撞到同一账号。
	// swrr 模式下同样生效（到期优先是成本/时效维度，与权重维度正交）。
	if p.preferExpiring {
		priority := make([]*entry, 0, len(candsAll))
		for _, c := range candsAll {
			if c.creditsExpiring <= 0 || c.creditsEarliestRemaining <= 0 ||
				c.creditsEarliestExpiry.IsZero() || !c.creditsEarliestExpiry.After(now) {
				continue
			}
			priority = append(priority, c)
		}
		sort.SliceStable(priority, func(i, j int) bool {
			if !priority[i].creditsEarliestExpiry.Equal(priority[j].creditsEarliestExpiry) {
				return priority[i].creditsEarliestExpiry.Before(priority[j].creditsEarliestExpiry)
			}
			if priority[i].creditsEarliestRemaining != priority[j].creditsEarliestRemaining {
				return priority[i].creditsEarliestRemaining > priority[j].creditsEarliestRemaining
			}
			return priority[i].a.UID < priority[j].a.UID
		})
		for _, c := range priority {
			if now.Sub(c.lastUsed) >= minPickGap {
				e = c
				break
			}
		}
		if e == nil && len(priority) > 0 {
			e = priority[0]
		}
	}
	if e == nil {
		e = p.selectByMode(ws, candsAll, now)
	}
	if explored {
		// 探索事件日志（可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost
		// 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	p.recordPickLocked(e.a.UID) // 分布计数（/status pick 段验证比例用）
	return e.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if routeWeight(p.pick.accounts, uid) == 0 {
			continue // 软摘除：兜底也不选中（运营意图优先于可用性）
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	p.recordPickLocked(best.a.UID)
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeightedCands 加权随机抽签：直接使用候选的 w_final（= w_base × weight_cfg），
// 不在内部重算 credits/idle。
//
// 为什么必须用 w_final：静态权重（pool.accounts.weight）若只参与 Top5 截断排序、不参与
// 抽签，则候选数 ≤5 时它完全不影响概率——运营在面板配 9:1，实际分流仍是 50:50（实测踩过）。
// 同时这也修掉旧实现的口径裂缝：旧 pickWeighted 在**截断后的 top5 子集**里重算 maxCredits，
// 与截断排序用的全集口径不同源，两阶段权重不可比（原注释要求"共享同一基准"）。
//
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变：
// 注入源返回 0 时必选 eligible[0]，即层内 (单价, w_final) 最优者）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeightedCands(cands []weightedCand) *entry {
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, c := range cands {
		w := int64(c.w * scale)
		if w < 0 {
			w = 0 // 负权重（配置层已拦）防御：不参与抽签而非倒扣
		}
		weights[i] = w
		total += w
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		// 权重全为 0（理论不可达：w_base ≥1）：退化为均匀随机。
		return cands[int(rnd(int64(len(cands))))].e
	}
	r := rnd(total)
	var acc int64
	for i, c := range cands {
		acc += weights[i]
		if r < acc {
			return c.e
		}
	}
	return cands[len(cands)-1].e
}

// weightOf 计算单个账号的普通加权分值。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	return w
}

// SetCredits 更新账号余额。
