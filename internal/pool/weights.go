// 权重选号：swrr 平滑加权轮询 + priority 主备分档 + 静态路由解析 + 分布计数。
//
// 与 NewAPI 的对应关系：静态权重（运营意图）× 动态健康（池子现状）正交叠加；
// priority 只做分组、不参与加权。weighted 缺省模式一字不动（w_final = w_base × weight_cfg）。
package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PickMode 权重选择模式。缺省 weighted（现状：Top5 + 加权随机）。
type PickMode string

const (
	PickWeighted PickMode = "weighted" // 现状 Top5 + 加权随机（缺省）
	PickSWRR     PickMode = "swrr"     // 平滑加权轮询（nginx 口径，全候选）
	PickPriority PickMode = "priority" // 主备分档：最低 priority 档内按 PickInner 选
)

// PickInner priority 模式下同档内的选择算法。
type PickInner string

const (
	PickInnerWeighted PickInner = "weighted" // 同档内 Top5 + 加权随机
	PickInnerSWRR     PickInner = "swrr"     // 同档内 SWRR
)

// AccountRoute 单账号静态路由：运营意图层。
//
// Weight 用指针而非 float64：必须区分「未设置」（nil → 缺省 1）与「显式设 0」
// （软摘除）。float64 零值无法表达这个三态——只配了 group/priority 的账号会被
// 零值误判成软摘除，整池候选被清空（实测踩过：TestPriorityGroupSWRR）。
type AccountRoute struct {
	Weight   *float64 `json:"weight,omitempty"`   // nil = 缺省 1；0 = 软摘除；>0 生效
	Priority int      `json:"priority"`           // 越小越优先（缺省 0 同级）
	Group    string   `json:"group,omitempty"`    // 缺省 ""（顶层账号直比）
}

// GroupRoute 分组静态路由。
type GroupRoute struct {
	Priority int      `json:"priority"`         // 越小越先消耗；缺省 0
	Weight   *float64 `json:"weight,omitempty"` // nil = 缺省 1；必须 >0（0 会让该组饿死）
}

// 有效性：AccountRoute.Weight nil 合法（缺省）；NaN/负数非法。
func (r AccountRoute) valid() error {
	if r.Weight == nil {
		return nil
	}
	w := *r.Weight
	if w != w {
		return fmt.Errorf("weight 是 NaN")
	}
	if w < 0 {
		return fmt.Errorf("weight %v 非法（必须 >=0，0=软摘除）", w)
	}
	return nil
}

// 有效性：GroupRoute.Weight nil 合法（缺省 1）；NaN/<=0 非法。
func (r GroupRoute) valid() error {
	if r.Weight == nil {
		return nil
	}
	w := *r.Weight
	if w != w {
		return fmt.Errorf("weight 是 NaN")
	}
	if w <= 0 {
		return fmt.Errorf("weight %v 非法（必须 >0）", w)
	}
	return nil
}

// resolveAccountRoute 最长前缀匹配：全 UID > 8 位前缀 > 更短前缀。
// 未配置返回零值（Weight 0 在调用方按“未配置→1”处理，见 routeWeight）。
func resolveAccountRoute(routes map[string]AccountRoute, uid string) (AccountRoute, bool) {
	best := -1
	var out AccountRoute
	var ok bool
	for k, r := range routes {
		if k == "" {
			continue
		}
		if uid != k && !strings.HasPrefix(uid, k) {
			continue
		}
		// 全等优先于前缀；前缀之间取最长。
		matchLen := len(k)
		if uid == k {
			matchLen += 1 << 20
		}
		if matchLen > best {
			best = matchLen
			out = r
			ok = true
		}
	}
	return out, ok
}

// routeWeight 返回账号的生效权重：未配置或 Weight=nil → 1；显式 0 = 软摘除。
func routeWeight(routes map[string]AccountRoute, uid string) float64 {
	if r, ok := resolveAccountRoute(routes, uid); ok && r.Weight != nil {
		return *r.Weight
	}
	return 1
}

// groupWeight 返回组生效权重：未配置或 Weight=nil → 1。
func groupWeight(groups map[string]GroupRoute, name string) float64 {
	if g, ok := groups[name]; ok && g.Weight != nil {
		return *g.Weight
	}
	return 1
}

// routePriority 返回账号优先级：未配置 → 0。
func routePriority(routes map[string]AccountRoute, uid string) int {
	if r, ok := resolveAccountRoute(routes, uid); ok {
		return r.Priority
	}
	return 0
}

// routeGroup 返回账号所属分组：未配置 → ""。
func routeGroup(routes map[string]AccountRoute, uid string) string {
	if r, ok := resolveAccountRoute(routes, uid); ok {
		return strings.TrimSpace(r.Group)
	}
	return ""
}

// ValidateRoutes 校验整套路由配置：非法值 fail-fast（启动/保存时报错）。
// 要求：账号 Weight >=0（NaN/负数报错）；组 Weight >0；组名非空；
// 同一 UID 被多个前缀命中时只告警不报错（最长匹配兜底，见 resolveAccountRoute）。
func ValidateRoutes(accounts map[string]AccountRoute, groups map[string]GroupRoute) error {
	for k, r := range accounts {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("pool.accounts: 键不能为空")
		}
		if err := r.valid(); err != nil {
			return fmt.Errorf("pool.accounts[%q]: %v", k, err)
		}
	}
	for name, g := range groups {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("pool.groups: 组名不能为空")
		}
		if err := g.valid(); err != nil {
			return fmt.Errorf("pool.groups[%q]: %v", name, err)
		}
	}
	// 组内至少要有一个账号可落位？不强制：空组只是永远选不中（priority 档位计算
	// 只看有 healthy 成员的组），配错组名在 /status 的 pick 段可见（成员 0）。
	return nil
}

// NormalizePickMode 归一 pick_mode/pick_inner：空回落缺省；非法报错。
func NormalizePickMode(mode, inner string) (PickMode, PickInner, error) {
	var m PickMode
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "weighted":
		m = PickWeighted
	case "swrr":
		m = PickSWRR
	case "priority":
		m = PickPriority
	default:
		return "", "", fmt.Errorf("pool.pick_mode: %q 非法（weighted / swrr / priority）", mode)
	}
	var n PickInner
	switch strings.ToLower(strings.TrimSpace(inner)) {
	case "", "weighted":
		n = PickInnerWeighted
	case "swrr":
		n = PickInnerSWRR
	default:
		return "", "", fmt.Errorf("pool.pick_inner: %q 非法（weighted / swrr）", inner)
	}
	return m, n, nil
}

// swrrPick 平滑加权轮询（nginx smooth WRR 口径）：
// 每候选 current += w，选 current 最大者，选中者 current -= total。
// curr map 缺键按 0 起步；w <= 0 的候选不应传入（调用方已剔除 weight=0）。
// 返回选中下标。权重全 1 时退化为严格 RR。
func swrrPick(weights []float64, curr []float64) int {
	best := 0
	total := 0.0
	for i, w := range weights {
		curr[i] += w
		total += w
		if curr[i] > curr[best] {
			best = i
		}
	}
	curr[best] -= total
	return best
}

// weightedCand 同成本层过滤后的候选行。
// w = w_final = w_base × weight_cfg（静态权重乘子）；cost1k = 该模型实测单价（层内排序用）。
type weightedCand struct {
	e      *entry
	w      float64 // w_final（含静态权重乘子；lastW 同源）
	cost1k float64 // 实测单价（tier 2 层内低者优先；tier 0/1 恒 0）
}

// selectByMode 按 pick.mode 在同层候选中选择（调用方已持 pick 写锁）。
// cands: weighted 模式 Top5 / 其他模式全量（权重降序）；candsAll: 全量（权重降序）。
// now: 本次 pick 时刻（minPickGap 判定用）。w_final 随 weightedCand 携带，无需另传表。
func (p *Pool) selectByMode(ws []weightedCand, cands, candsAll []*entry, now time.Time) *entry {
	switch p.pick.mode {
	case PickSWRR:
		return p.selectSWRR(ws)
	case PickPriority:
		return p.selectPriority(ws, now)
	default:
		return p.selectWeightedLegacy(cands, candsAll, now)
	}
}

// selectWeightedLegacy 现状选择：minPickGap 过滤 Top5 → pickWeighted；
// 全被刚用过 → 全量 LRU（usedSeq）。与改前 pick.go 选择段逐行同义。
func (p *Pool) selectWeightedLegacy(cands, candsAll []*entry, now time.Time) *entry {
	eligible := make([]*entry, 0, len(cands))
	for _, c := range cands {
		if now.Sub(c.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		e := candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
		return e
	}
	return p.pickWeighted(eligible)
}

// selectSWRR 全候选平滑加权轮询：不截断、不看 minPickGap（SWRR 自带打散；
// 并发撞号由 pickLocked 末尾的 lastUsed 标记 + 上游在途租约承接）。
// swCurr 缺键按 0 起步。
func (p *Pool) selectSWRR(ws []weightedCand) *entry {
	weights := make([]float64, len(ws))
	curr := make([]float64, len(ws))
	for i, c := range ws {
		weights[i] = c.w
		curr[i] = p.swCurr[c.e.a.UID]
	}
	idx := swrrPick(weights, curr)
	for i, c := range ws {
		p.swCurr[c.e.a.UID] = curr[i]
	}
	return ws[idx].e
}

// selectPriority priority 模式同档内选择：
//   - 同档有多组 → 组权重 SWRR 选组（grpCurr 运行态），组内按 pick.inner 选号；
//   - 同档单组/无组（group="" 顶层账号）→ 按 pick.inner 直接选号。
// inner=weighted 时沿用 Top5+pickWeighted（含 minPickGap/LRU）；inner=swrr 时 SWRR。
// ws 已是同 priority 档成员（pickLocked 内按 effectivePriorityLocked 取最小档过滤）。
func (p *Pool) selectPriority(ws []weightedCand, now time.Time) *entry {
	// 按组聚类（保持 ws 权重降序内稳定）。
	groups := map[string][]weightedCand{}
	order := []string{}
	for _, c := range ws {
		g := routeGroup(p.pick.accounts, c.e.a.UID)
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	// 单组（或全无组）→ 组内直选。
	if len(order) == 1 {
		return p.selectInner(groups[order[0]], now)
	}
	// 多组：组权重 SWRR 选组。组权重 = groups 表 weight（缺省 1；ValidateRoutes
	// 已保证 >0）。组内成员全 healthy（ws 前置过滤），故选中组必有可用号。
	sort.Strings(order) // 稳定组序（SWRR current 按序索引）
	gw := make([]float64, len(order))
	gc := make([]float64, len(order))
	for i, g := range order {
		gw[i] = groupWeight(p.pick.groups, g)
		gc[i] = p.grpCurr[g]
	}
	gi := swrrPick(gw, gc)
	for i, g := range order {
		p.grpCurr[g] = gc[i]
	}
	return p.selectInner(groups[order[gi]], now)
}

// selectInner 同档/同组内按 pick.inner 选择。
func (p *Pool) selectInner(members []weightedCand, now time.Time) *entry {
	if p.pick.inner == PickInnerSWRR {
		return p.selectSWRR(members)
	}
	// weighted：成员按 w_final 降序取 Top5（成员不足 5 全取），minPickGap + pickWeighted + LRU。
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].w != members[j].w {
			return members[i].w > members[j].w
		}
		return members[i].e.a.UID < members[j].e.a.UID
	})
	top := members
	if len(top) > 5 {
		top = top[:5]
	}
	el := make([]*entry, 0, len(top))
	for _, m := range top {
		el = append(el, m.e)
	}
	eligible := make([]*entry, 0, len(el))
	for _, c := range el {
		if now.Sub(c.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		// 组内全被刚用过：组内 LRU（usedSeq），不跨组抢（跨组=越级，违背 priority）。
		e := members[0].e
		for _, m := range members[1:] {
			if m.e.usedSeq < e.usedSeq {
				e = m.e
			}
		}
		return e
	}
	return p.pickWeighted(eligible)
}

// --- Pool 上的策略状态（运行态，不持久化）---

// pickPolicy 快照：SetPickPolicy 整体替换（写锁），读路径只读。
type pickPolicy struct {
	mode     PickMode
	inner    PickInner
	accounts map[string]AccountRoute
	groups   map[string]GroupRoute
}

// SetPickPolicy 注入选号策略（main 从 config 解析后调用 + 面板热应用）。
// mode/inner 空串 = 保持现值（与 SetWeights 的“非正值保留”同风格）；
// accounts/groups 为 nil = 保持现值（面板深合并保存天然只提交改了的键）。
// 非法值返回 error（调用方落盘前校验：此刻已是校验后值，理论不可达，防呆）。
func (p *Pool) SetPickPolicy(mode PickMode, inner PickInner, accounts map[string]AccountRoute, groups map[string]GroupRoute) error {
	if mode != "" && mode != PickWeighted && mode != PickSWRR && mode != PickPriority {
		return fmt.Errorf("pick_mode %q 非法", mode)
	}
	if inner != "" && inner != PickInnerWeighted && inner != PickInnerSWRR {
		return fmt.Errorf("pick_inner %q 非法", inner)
	}
	if accounts != nil || groups != nil {
		mergedA := p.pick.accounts
		mergedG := p.pick.groups
		if accounts != nil {
			mergedA = accounts
		}
		if groups != nil {
			mergedG = groups
		}
		if err := ValidateRoutes(mergedA, mergedG); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode != "" {
		p.pick.mode = mode
	}
	if inner != "" {
		p.pick.inner = inner
	}
	if accounts != nil {
		p.pick.accounts = accounts
		// 路由表变更 → SWRR current 与末次 w_final 清零重学（旧值按旧权重累积，新权重下失真）。
		p.swCurr = map[string]float64{}
		p.grpCurr = map[string]float64{}
		p.lastW = map[string]float64{}
	}
	if groups != nil {
		p.pick.groups = groups
		p.grpCurr = map[string]float64{}
	}
	return nil
}

// PickPolicy 返回当前策略快照（/status pick 段用；RLock）。
func (p *Pool) PickPolicy() (mode PickMode, inner PickInner) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pick.mode, p.pick.inner
}

// groupPriority 组优先级：未在 groups 表 → 0（与账号缺省对齐）。
func (p *Pool) groupPriorityLocked(name string) int {
	if g, ok := p.pick.groups[name]; ok {
		return g.Priority
	}
	return 0
}

// effectivePriority 账号生效优先级 = 组优先级（有名组）与账号优先级的……取组优先。
// 口径：有分组 → 组 priority 决定档位（组是消耗单位）；无分组（group=""）→ 账号 priority。
// 不做两者相加（相加会让“组内调序”意外跨组，违背分组语义）。
func (p *Pool) effectivePriorityLocked(uid string) int {
	g := routeGroup(p.pick.accounts, uid)
	if g != "" {
		return p.groupPriorityLocked(g)
	}
	return routePriority(p.pick.accounts, uid)
}

// recordPick 记一次 pick 分布（成功选中路径唯一调用点；调用方已持写锁）。
func (p *Pool) recordPickLocked(uid string) {
	if p.pickDist == nil {
		p.pickDist = map[string]uint64{}
	}
	p.pickDist[uid]++
}

// PickDistribution 返回进程内 pick 分布计数快照（uid → 次数；RLock）。
func (p *Pool) PickDistribution() map[string]uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]uint64, len(p.pickDist))
	for k, v := range p.pickDist {
		out[k] = v
	}
	return out
}

// ResetPickStats 清零 SWRR current + 分布计数（面板“清零”按钮；写锁）。
func (p *Pool) ResetPickStats() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.swCurr = map[string]float64{}
	p.grpCurr = map[string]float64{}
	p.pickDist = map[string]uint64{}
}

// PickPolicyStatus /status pick 段载荷：模式 + 组表（含成员数）+ 账号行 + 分布。
type PickGroupStatus struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	Weight   float64 `json:"weight"`
	Members  int    `json:"members"`
}

type PickAccountStatus struct {
	UID      string  `json:"uid"`
	Weight   float64 `json:"weight_cfg"` // 静态权重（0=软摘除）
	Priority int     `json:"priority"`   // 生效优先级（组优先）
	Group    string  `json:"group"`
	WFinal   float64 `json:"w_final"`  // 末次 pick 的 w_base × weight（无 pick 为 0）
	Current  float64 `json:"sw_current"` // SWRR current（weighted 模式恒 0）
	Picks    uint64  `json:"picks"`
}

type PickPolicyView struct {
	Mode         string              `json:"mode"`
	Inner        string              `json:"inner"`
	Groups       []PickGroupStatus   `json:"groups"`
	Accounts     []PickAccountStatus `json:"accounts"`
	Distribution map[string]uint64   `json:"distribution"`
}

// PickPolicyStatus 组装 pick 段视图（RLock；成员数按当前池账号归属计算）。
func (p *Pool) PickPolicyStatus() PickPolicyView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v := PickPolicyView{
		Mode:         string(p.pick.mode),
		Inner:        string(p.pick.inner),
		Distribution: map[string]uint64{},
	}
	for k, n := range p.pickDist {
		v.Distribution[k] = n
	}
	members := map[string]int{}
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	v.Accounts = make([]PickAccountStatus, 0, len(uids))
	for _, uid := range uids {
		w := routeWeight(p.pick.accounts, uid)
		pri := p.effectivePriorityLocked(uid)
		g := routeGroup(p.pick.accounts, uid)
		if g != "" {
			members[g]++
		}
		var cur float64
		if p.pick.mode == PickSWRR {
			cur = p.swCurr[uid]
		}
		v.Accounts = append(v.Accounts, PickAccountStatus{
			UID: uid, Weight: w, Priority: pri, Group: g,
			WFinal: p.lastW[uid], Current: cur, Picks: p.pickDist[uid],
		})
	}
	names := make([]string, 0, len(p.pick.groups))
	for n := range p.pick.groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := p.pick.groups[n]
		v.Groups = append(v.Groups, PickGroupStatus{
			Name: n, Priority: g.Priority, Weight: groupWeight(p.pick.groups, n), Members: members[n],
		})
	}
	return v
}
