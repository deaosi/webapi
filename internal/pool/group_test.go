// 上游渠道分组隔离（pool 侧）：严格不跨组、兜底不跨组、粘性兜底校验。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 两号分属两组：绑组请求只在本组选号。
func TestGroupScopedPickIsolation(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-a"})
	p.Add(&auth.Auth{UID: "u-b"})
	p.SetCredits("u-a", 100, 0)
	p.SetCredits("u-b", 100, 0)
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted, map[string]AccountRoute{
		"u-a": {Group: "g1"},
		"u-b": {Group: "g2"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got := p.PickExcludingForRealmGroup(nil, "", "", "g1"); got == nil || got.UID != "u-a" {
			t.Fatalf("g1 只应选到 u-a: %+v", got)
		}
	}
	for i := 0; i < 20; i++ {
		if got := p.PickExcludingForRealmGroup(nil, "", "", "g2"); got == nil || got.UID != "u-b" {
			t.Fatalf("g2 只应选到 u-b: %+v", got)
		}
	}
	// group="" 不限制：两号都可能被选中。
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if got := p.PickExcludingForRealmGroup(nil, "", "", ""); got != nil {
			seen[got.UID] = true
		}
	}
	if !seen["u-a"] || !seen["u-b"] {
		t.Errorf("group=\"\" 应不限组: %v", seen)
	}
}

// 严格语义：本组无 healthy 号时返回 nil，绝不借用他组账号。
func TestGroupScopedStrictNoFallback(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-a"})
	p.Add(&auth.Auth{UID: "u-b"})
	p.SetCredits("u-a", 100, 0)
	p.SetCredits("u-b", 100, 0)
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted, map[string]AccountRoute{
		"u-a": {Group: "g1"},
		"u-b": {Group: "g2"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	p.Disable("u-a", "test") // g1 唯一成员出池
	if got := p.PickExcludingForRealmGroup(nil, "", "", "g1"); got != nil {
		t.Fatalf("g1 无可用应返回 nil（不得跨组借 u-b）: %+v", got)
	}
	// 兜底同样只在组内选：把 u-a 改成软冷却（会进全冷却兜底候选），
	// 兜底应捞组内 u-a，绝不能跨组借健康的 u-b——这才是 strict 锁住的漏口。
	p.Revive("u-a")
	p.Cooldown("u-a", CoolSoft, 3600*1e9*24, "test")
	if got := p.PickExcludingForRealmGroup(nil, "", "", "g1"); got == nil || got.UID != "u-a" {
		t.Fatalf("g1 全冷却时兜底应捞组内 u-a（不得跨组）: %+v", got)
	}
	// 对照组：g2 仍可用。
	if got := p.PickExcludingForRealmGroup(nil, "", "", "g2"); got == nil || got.UID != "u-b" {
		t.Fatalf("g2 应可用: %+v", got)
	}
}

// 未配置分组的账号不属于任何命名分组：绑组请求看不到它。
func TestGroupScopedUngroupedInvisible(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-none"}) // 未配 group（""）
	p.SetCredits("u-none", 100, 0)
	if got := p.PickExcludingForRealmGroup(nil, "", "", "g1"); got != nil {
		t.Fatalf("无分组账号不应出现在绑定 g1 的请求里: %+v", got)
	}
	if got := p.PickExcludingForRealmGroup(nil, "", "", ""); got == nil {
		t.Fatal("不限组时应能选中无分组账号")
	}
}

// UIDInGroup：粘性命中路径的隔离兜底校验。
func TestUIDInGroup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u-a"})
	p.Add(&auth.Auth{UID: "u-b"})
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted, map[string]AccountRoute{
		"u-a": {Group: "g1"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if !p.UIDInGroup("u-a", "g1") {
		t.Error("u-a 属于 g1")
	}
	if p.UIDInGroup("u-a", "g2") {
		t.Error("u-a 不属于 g2")
	}
	if p.UIDInGroup("u-b", "g1") {
		t.Error("未配置分组的 u-b 不属于 g1")
	}
	if !p.UIDInGroup("u-a", "") {
		t.Error("group=\"\" 恒 true")
	}
	if p.UIDInGroup("nope", "g1") {
		t.Error("池内不存在的 uid 应 false")
	}
}

// realm 与 group 叠加：两个维度同时生效。
func TestGroupAndRealmCombined(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// realm 字段未导出（包外只能经 Realm() 读）：用 Domain 触发 global 回落，
	// 与生产 auth 文件同口径（.workbuddy.ai 后缀 → global）。
	p.Add(&auth.Auth{UID: "cn-g1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl-g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("cn-g1", 100, 0)
	p.SetCredits("gl-g1", 100, 0)
	if err := p.SetPickPolicy(PickWeighted, PickInnerWeighted, map[string]AccountRoute{
		"cn-g1": {Group: "g1"},
		"gl-g1": {Group: "g1"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if got := p.PickExcludingForRealmGroup(nil, "", "cn", "g1"); got == nil || got.UID != "cn-g1" {
			t.Fatalf("cn+g1 应只选 cn-g1: %+v", got)
		}
		if got := p.PickExcludingForRealmGroup(nil, "", "global", "g1"); got == nil || got.UID != "gl-g1" {
			t.Fatalf("global+g1 应只选 gl-g1: %+v", got)
		}
	}
}
