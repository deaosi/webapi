// 权重选号单测：swrr 比例 / priority 降级 / 软摘除 / 路由校验 / 最长前缀。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 静态权重在 swrr 下严格按比例分流：3:1 打 400 次，容差带 ±15%。
func TestSWRRRatio(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-high"})
	p.Add(&auth.Auth{UID: "u-low"})
	p.SetCredits("u-high", 100, 0)
	p.SetCredits("u-low", 100, 0)
	// 余额拉平：w_base≈1+1+idle，让静态权重主导（idle 全 0 时 w_base 相等）。
	// 注：lastUsed 零值给满分 idle——两号同为新号，idle 相等，w_base 相等。
	if err := p.SetPickPolicy(PickSWRR, PickInnerWeighted,
		map[string]AccountRoute{"u-high": {Weight: 3}, "u-low": {Weight: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		counts[p.Pick().UID]++
	}
	hi, lo := counts["u-high"], counts["u-low"]
	if hi < 240 || hi > 360 || lo < 40 || lo > 160 {
		t.Errorf("swrr 3:1 偏离容差带: high=%d low=%d", hi, lo)
	}
}

// 权重全 1 的 swrr 退化为严格 RR：ABAB 交替（2 号场景）。
func TestSWRRDegradesToRR(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-a"})
	p.Add(&auth.Auth{UID: "u-b"})
	p.SetCredits("u-a", 100, 0)
	p.SetCredits("u-b", 100, 0)
	if err := p.SetPickPolicy(PickSWRR, PickInnerWeighted, nil, nil); err != nil {
		t.Fatal(err)
	}
	seq := ""
	for i := 0; i < 6; i++ {
		seq += p.Pick().UID + " "
	}
	// 严格交替（起点不定，两种交替都接受）。
	if seq != "u-a u-b u-a u-b u-a u-b " && seq != "u-b u-a u-b u-a u-b u-a " {
		t.Errorf("swrr 等权应严格交替: %q", seq)
	}
}

// priority：主号健康时备号零选中；主号冷却后全走备号。
func TestPriorityFallback(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-main"})
	p.Add(&auth.Auth{UID: "u-back"})
	p.SetCredits("u-main", 100, 0)
	p.SetCredits("u-back", 100, 0)
	if err := p.SetPickPolicy(PickPriority, PickInnerWeighted,
		map[string]AccountRoute{"u-main": {Priority: 0}, "u-back": {Priority: 10}}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u-main" {
			t.Fatalf("主号健康时应全走主号: %+v", got)
		}
	}
	p.Cooldown("u-main", CoolSoft, 3600*1e9*24, "test")
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u-back" {
			t.Fatalf("主号冷却后应全走备号: %+v", got)
		}
	}
}

// priority 同档多组按组权重 SWRR：g1:g2 = 1:1 交替。
func TestPriorityGroupSWRR(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, u := range []string{"u-a1", "u-b1"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
	}
	if err := p.SetPickPolicy(PickPriority, PickInnerSWRR,
		map[string]AccountRoute{
			"u-a1": {Group: "g1"},
			"u-b1": {Group: "g2"},
		},
		map[string]GroupRoute{"g1": {Weight: 1}, "g2": {Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[p.Pick().UID]++
	}
	if seen["u-a1"] != 2 || seen["u-b1"] != 2 {
		t.Errorf("同档双组应 1:1 交替: %v", seen)
	}
}

// weight=0 软摘除：pick 与兜底都不选中，但账号仍在池（List 可见）。
func TestWeightZeroExcludes(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u-keep"})
	p.Add(&auth.Auth{UID: "u-drop"})
	p.SetCredits("u-keep", 100, 0)
	p.SetCredits("u-drop", 100000, 0) // 余额再高也被摘除
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted,
		map[string]AccountRoute{"u-drop": {Weight: 0}}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u-keep" {
			t.Fatalf("摘除号不应被选中: %+v", got)
		}
	}
	// 兜底也不选中：把 keep 冷却，pick 应返回 nil 而非 drop。
	p.Cooldown("u-keep", CoolSoft, 3600*1e9*24, "test")
	if got := p.Pick(); got != nil {
		t.Fatalf("仅剩摘除号时应返回 nil: %+v", got)
	}
	// 但 List 仍可见（不出池）。
	found := false
	for _, s := range p.List() {
		if s.UID == "u-drop" {
			found = true
		}
	}
	if !found {
		t.Errorf("摘除号应仍在 List")
	}
}

// 最长前缀匹配：全 UID 覆盖 8 位前缀。
func TestLongestPrefixMatch(t *testing.T) {
	routes := map[string]AccountRoute{
		"0ba7c446":                  {Weight: 1},
		"0ba7c446-0a15-401e-b0f1-447355efb1e7": {Weight: 5},
	}
	if got := routeWeight(routes, "0ba7c446-0a15-401e-b0f1-447355efb1e7"); got != 5 {
		t.Errorf("全 UID 应命中 5: %v", got)
	}
	if got := routeWeight(routes, "0ba7c446-xxxx"); got != 1 {
		t.Errorf("前缀应命中 1: %v", got)
	}
	if got := routeWeight(routes, "other"); got != 1 {
		t.Errorf("未配置应回落 1: %v", got)
	}
}

// 校验：负权重/组 weight=0/空键/非法 mode 全报错。
func TestValidateRoutes(t *testing.T) {
	if err := ValidateRoutes(map[string]AccountRoute{"u": {Weight: -1}}, nil); err == nil {
		t.Errorf("负 weight 应报错")
	}
	if err := ValidateRoutes(nil, map[string]GroupRoute{"g": {Weight: 0}}); err == nil {
		t.Errorf("组 weight=0 应报错")
	}
	if err := ValidateRoutes(map[string]AccountRoute{"": {Weight: 1}}, nil); err == nil {
		t.Errorf("空键应报错")
	}
	if _, _, err := NormalizePickMode("rr", ""); err == nil {
		t.Errorf("非法 mode 应报错")
	}
	if _, _, err := NormalizePickMode("swrr", "fifo"); err == nil {
		t.Errorf("非法 inner 应报错")
	}
	if err := ValidateRoutes(map[string]AccountRoute{"u": {Weight: 0}}, nil); err != nil {
		t.Errorf("weight=0 合法（软摘除）: %v", err)
	}
}

// 分布计数：pick 记录 + Reset 清零。
func TestPickDistribution(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 100, 0)
	for i := 0; i < 10; i++ {
		p.Pick()
	}
	d := p.PickDistribution()
	if d["u1"]+d["u2"] != 10 {
		t.Errorf("分布计数应为 10: %v", d)
	}
	p.ResetPickStats()
	if len(p.PickDistribution()) != 0 {
		t.Errorf("Reset 后分布应清空")
	}
}

// weighted 模式叠加静态权重：3:1 乘子应显著偏向高权重号。
func TestWeightedModeStaticMultiplier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-high"})
	p.Add(&auth.Auth{UID: "u-low"})
	p.SetCredits("u-high", 100, 0)
	p.SetCredits("u-low", 100, 0)
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted,
		map[string]AccountRoute{"u-high": {Weight: 9}, "u-low": {Weight: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u-high"] <= counts["u-low"]*2 {
		t.Errorf("9:1 乘子应显著偏向 high: %v", counts)
	}
}
