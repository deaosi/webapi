// 会话粘性分配的策略选号回调测试（Allocate）。
//
// 背景：权重/priority/swrr/成本分层/最早到期优先只在 pool.pickLocked 里实现，会话
// 路由器若自行 hashIndex 分配会把整条策略旁路（生产表现为面板调权重无效、pick 分布
// 恒为 0）。这些用例锁住「回调优先、异常回落、空闲优先语义保留」三件事。
package session

import (
	"testing"
	"time"
)

func newTestRouter(t *testing.T, avail []string, allocate func(string, []string) string) *Router {
	t.Helper()
	return New(Config{
		TTL:        time.Minute,
		GCInterval: time.Minute,
		Available:  func() []string { return avail },
		AvailableForModel: func(string) []string { return avail },
		Allocate:   allocate,
	})
}

// 回调返回的号必须被直接采用（而不是回落哈希）。
func TestAllocateCallbackUsed(t *testing.T) {
	var gotModel string
	r := newTestRouter(t, []string{"a", "b", "c"}, func(model string, _ []string) string {
		gotModel = model
		return "c"
	})
	uid, ok := r.ResolveForModel("k1", "cn:glm-5.2")
	if !ok || uid != "c" {
		t.Fatalf("应采用回调返回的 c: uid=%q ok=%v", uid, ok)
	}
	if gotModel != "cn:glm-5.2" {
		t.Errorf("回调应收到原始模型名（含 realm 前缀）: %q", gotModel)
	}
	// 再次解析同一 key 走快路径命中，不应再调回调。
	r2calls := 0
	r.cfg.Allocate = func(string, []string) string { r2calls++; return "a" }
	if uid2, ok := r.ResolveForModel("k1", "cn:glm-5.2"); !ok || uid2 != "c" {
		t.Fatalf("粘性命中应保持 c: uid=%q ok=%v", uid2, ok)
	}
	if r2calls != 0 {
		t.Errorf("快路径命中不应再调 Allocate（实际 %d 次）", r2calls)
	}
}

// 回调返回空串 → 回落内置哈希分配（行为与无回调一致：结果必在可用集内）。
func TestAllocateEmptyFallsBack(t *testing.T) {
	r := newTestRouter(t, []string{"a", "b"}, func(string, []string) string { return "" })
	uid, ok := r.ResolveForModel("k1", "")
	if !ok {
		t.Fatal("空回调应回落哈希分配而非失败")
	}
	if uid != "a" && uid != "b" {
		t.Fatalf("回落结果必须来自可用集: %q", uid)
	}
}

// 回调返回不在可用集内的号 → 拒绝并回落（防回调与 available 口径不一致时粘到坏号）。
func TestAllocateRejectsUnavailable(t *testing.T) {
	r := newTestRouter(t, []string{"a", "b"}, func(string, []string) string { return "zzz" })
	uid, ok := r.ResolveForModel("k1", "")
	if !ok {
		t.Fatal("越界号应回落而非失败")
	}
	if uid == "zzz" {
		t.Fatal("不在可用集内的号不得被采用")
	}
	if uid != "a" && uid != "b" {
		t.Fatalf("回落结果必须来自可用集: %q", uid)
	}
}

// bound 必须携带"已被其他会话绑定"的 uid（回调据此保留空闲优先双段语义）。
func TestAllocateReceivesBound(t *testing.T) {
	var seen []string
	pick := "a"
	r := newTestRouter(t, []string{"a", "b"}, func(_ string, bound []string) string {
		seen = append([]string(nil), bound...)
		return pick
	})
	if _, ok := r.ResolveForModel("k1", ""); !ok {
		t.Fatal("k1 分配失败")
	}
	if len(seen) != 0 {
		t.Errorf("首次分配时无其他绑定，bound 应为空: %v", seen)
	}
	// 把 k1 绑到 a，再分配 k2：bound 应含 a。
	seen = nil
	if _, ok := r.ResolveForModel("k2", ""); !ok {
		t.Fatal("k2 分配失败")
	}
	if len(seen) != 1 || seen[0] != "a" {
		t.Errorf("bound 应含已绑定的 a: %v", seen)
	}
}

// 无回调时保持既有哈希分配行为（老调用零回归）。
func TestNoAllocateUsesHash(t *testing.T) {
	r := newTestRouter(t, []string{"a", "b", "c"}, nil)
	got := map[string]bool{}
	for _, k := range []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"} {
		uid, ok := r.ResolveForModel(k, "")
		if !ok {
			t.Fatalf("%s 分配失败", k)
		}
		if uid != "a" && uid != "b" && uid != "c" {
			t.Fatalf("哈希分配必须落在可用集内: %q", uid)
		}
		got[uid] = true
	}
	// 8 个 key 哈希到 3 个号上：不应全部塌到同一个（哈希分散性冒烟）。
	if len(got) < 2 {
		t.Errorf("哈希分配过度集中（%v），检查 hashIndex", got)
	}
}
