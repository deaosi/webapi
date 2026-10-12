// 配置保存的合并语义单测：深合并 vs 整体替换子树。
//
// 锁住的是这次修复的核心：「省略键 = 保留旧值」让删除无法表达——删渠道分组、
// 解绑密钥、删账号权重路由都会静默失效。这些子树改为整体替换。
package main

import "testing"

// 未列入 replacePaths 的子树仍是深合并：面板只提交自己管理的键，
// 手写的未知键与未提交的兄弟键必须保留。
func TestMergeConfigMapsDeepMerge(t *testing.T) {
	cur := map[string]any{
		"pool": map[string]any{
			"prefer_expiring": true,
			"手写未知键":            "keep-me",
		},
		"scheduler": map[string]any{"x": 1},
	}
	incoming := map[string]any{
		"pool": map[string]any{"prefer_expiring": false},
	}
	merged := mergeConfigMaps(cur, incoming)
	poolMap := merged["pool"].(map[string]any)
	if poolMap["prefer_expiring"] != false {
		t.Error("提交的键应被覆盖")
	}
	if poolMap["手写未知键"] != "keep-me" {
		t.Error("未提交的兄弟键（含手写未知键）必须保留")
	}
	if merged["scheduler"] == nil {
		t.Error("未触及的段落必须保留")
	}
}

// replacePaths 子树整体替换：提交的全量状态里没有的键必须真的消失。
func TestReplacePathsWholesaleDelete(t *testing.T) {
	cur := map[string]any{
		"channels": map[string]any{
			"enabled": true,
			"groups":  map[string]any{"main": map[string]any{}, "vip": map[string]any{}},
			"keys":    map[string]any{"key-1": "main", "key-2": "vip"},
		},
		"pool": map[string]any{
			"pick_mode": "swrr",
			"groups":    map[string]any{"main": map[string]any{}, "backup": map[string]any{}},
			"accounts":  map[string]any{"uid-a": map[string]any{"weight": 3}},
		},
	}
	// 面板提交删除后的**全量**状态：vip 渠道没了、key-2 绑定没了、backup 分组没了、uid-a 路由没了。
	incoming := map[string]any{
		"channels": map[string]any{
			"enabled": true,
			"groups":  map[string]any{"main": map[string]any{}},
			"keys":    map[string]any{"key-1": "main"},
		},
		"pool": map[string]any{
			"pick_mode": "swrr",
			"groups":    map[string]any{"main": map[string]any{}},
			"accounts":  map[string]any{},
		},
	}
	replaced := takeReplacePaths(incoming)
	merged := mergeConfigMaps(cur, incoming)
	applyReplacePaths(merged, replaced)

	ch := merged["channels"].(map[string]any)
	if _, ok := ch["groups"].(map[string]any)["vip"]; ok {
		t.Error("删除的渠道分组必须消失（深合并会把它补回来）")
	}
	if _, ok := ch["keys"].(map[string]any)["key-2"]; ok {
		t.Error("解绑的密钥必须消失")
	}
	if ch["groups"].(map[string]any)["main"] == nil {
		t.Error("保留的渠道分组不能丢")
	}

	poolMap := merged["pool"].(map[string]any)
	if _, ok := poolMap["groups"].(map[string]any)["backup"]; ok {
		t.Error("删除的 pool 分组必须消失")
	}
	if len(poolMap["accounts"].(map[string]any)) != 0 {
		t.Error("清空的账号路由表必须真的为空")
	}
	if poolMap["pick_mode"] != "swrr" {
		t.Error("同段内未提交的标量键应保留")
	}
}

// 面板只提交部分路径时，未提交的 replacePaths 子树不受影响（配置页保存不会误清渠道）。
func TestReplacePathsPartialSubmitKeepsOthers(t *testing.T) {
	cur := map[string]any{
		"channels": map[string]any{"enabled": true, "groups": map[string]any{"main": map[string]any{}}},
	}
	incoming := map[string]any{"pool": map[string]any{"prefer_expiring": false}}
	replaced := takeReplacePaths(incoming)
	if len(replaced) != 0 {
		t.Fatalf("未提交 replacePaths 时不应摘出任何子树: %v", replaced)
	}
	merged := mergeConfigMaps(cur, incoming)
	applyReplacePaths(merged, replaced)
	if merged["channels"] == nil {
		t.Error("未提交的 channels 段必须保留")
	}
}

// pathTake/pathSet 的边角：中间层缺失或类型不符时不 panic、不误建。
func TestPathHelpers(t *testing.T) {
	m := map[string]any{"a": map[string]any{"b": 1}}
	if v, ok := pathTake(m, []string{"a", "missing"}); ok || v != nil {
		t.Errorf("缺键应返回 false")
	}
	if _, ok := pathTake(m, []string{"a", "b", "c"}); ok {
		t.Errorf("中间层不是对象应返回 false")
	}
	if _, ok := pathTake(m, nil); ok {
		t.Errorf("空路径应返回 false")
	}
	pathSet(m, []string{"x", "y"}, 7)
	if m["x"].(map[string]any)["y"] != 7 {
		t.Errorf("pathSet 应就地新建中间层: %v", m)
	}
	pathSet(m, nil, 1) // 不应 panic
}
