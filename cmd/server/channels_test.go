// 渠道配置校验与解析的单测（cmd/server）。
package main

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

func boolPtr(b bool) *bool { return &b }

// 校验：分组名/realm/绑定分组必须自洽。绑定指向未定义分组一律拒绝（fail closed）。
func TestNormalizeChannels(t *testing.T) {
	ok := Channels{
		Enabled: true,
		Groups: map[string]ChannelDef{
			"main": {Label: "主", Realm: "cn", Models: []string{" glm-5* ", "", "deepseek-v3"}},
		},
		Keys: map[string]string{"sk-wb2a-abcdefgh": "main"},
	}
	if err := normalizeChannels(&ok); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	if got := ok.Groups["main"].Models; len(got) != 2 || got[0] != "deepseek-v3" || got[1] != "glm-5*" {
		t.Errorf("白名单应去空白去空项并排序: %v", got)
	}
	if !ok.Groups["main"].EnabledOn() {
		t.Error("Enabled=nil 应视为启用")
	}

	bad := []struct {
		name string
		ch   Channels
	}{
		{"空分组名", Channels{Groups: map[string]ChannelDef{"": {}}}},
		{"分组名含控制符", Channels{Groups: map[string]ChannelDef{"a\x1fb": {}}}},
		{"realm 非法", Channels{Groups: map[string]ChannelDef{"g": {Realm: "us"}}}},
		{"绑定指向未定义分组", Channels{
			Groups: map[string]ChannelDef{"g": {}},
			Keys:   map[string]string{"k1": "nope"},
		}},
		{"绑定分组为空", Channels{
			Groups: map[string]ChannelDef{"g": {}},
			Keys:   map[string]string{"k1": "  "},
		}},
		{"密钥标识为空", Channels{Keys: map[string]string{"": "g"}}},
	}
	for _, c := range bad {
		ch := c.ch
		if err := normalizeChannels(&ch); err == nil {
			t.Errorf("%s：应报错", c.name)
		}
	}

	// 显式停用是合法值（不是"未配置"）。
	off := Channels{Groups: map[string]ChannelDef{"g": {Enabled: boolPtr(false)}}}
	if err := normalizeChannels(&off); err != nil {
		t.Fatalf("显式停用应合法: %v", err)
	}
	if off.Groups["g"].EnabledOn() {
		t.Error("显式 false 应视为停用")
	}
	// nil map 归一成空 map（避免下游 nil 判空分支）。
	empty := Channels{}
	if err := normalizeChannels(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Groups == nil || empty.Keys == nil {
		t.Error("nil map 应归一为非 nil")
	}
}

// 解析：ID 绑定命中；未绑定/全局关闭/前缀命中各口径。
func TestChannelRouterResolve(t *testing.T) {
	store, err := apikeys.New(filepath.Join(t.TempDir(), "k.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.Create("测试密钥")
	if err != nil {
		t.Fatal(err)
	}

	ch := &Channels{
		Enabled: true,
		Groups: map[string]ChannelDef{
			"main": {Label: "主", Realm: "cn", Models: []string{"glm-5*"}},
		},
		Keys: map[string]string{key.ID: "main"},
	}
	if err := normalizeChannels(ch); err != nil {
		t.Fatal(err)
	}
	r := newChannelRouter(ch, store)

	info, bound := r.Resolve(key.ID)
	if !bound || info.Group != "main" || info.Realm != "cn" || !info.Enabled {
		t.Fatalf("ID 绑定应命中: %+v bound=%v", info, bound)
	}
	if len(info.Models) != 1 || info.Models[0] != "glm-5*" {
		t.Errorf("白名单应透出: %v", info.Models)
	}
	if _, bound := r.Resolve("不存在的密钥"); bound {
		t.Error("未知密钥应视为未绑定")
	}
	if _, bound := r.Resolve(""); bound {
		t.Error("空 keyID 应视为未绑定")
	}

	// 总开关关闭 → 全部视为未绑定（不报错，只是不限制）。
	off := &Channels{Enabled: false, Groups: ch.Groups, Keys: ch.Keys}
	r.Set(off)
	if _, bound := r.Resolve(key.ID); bound {
		t.Error("channels.enabled=false 时应视为未绑定")
	}

	// 前缀写法：配置里写 sk-wb2a-xxxxxxxx，请求只带 ID，需经快照映射。
	pref := &Channels{Enabled: true, Groups: ch.Groups, Keys: map[string]string{key.Prefix: "main"}}
	r.Set(pref)
	info2, bound2 := r.Resolve(key.ID)
	if !bound2 || info2.Group != "main" {
		t.Fatalf("前缀绑定应经 ID→前缀 快照命中: %+v bound=%v", info2, bound2)
	}

	// 渠道停用：仍算绑定（返回 Enabled=false），由 handler 决定 403——
	// 不能当作"未绑定"，否则停用渠道会变成静默全池放行。
	dis := &Channels{
		Enabled: true,
		Groups:  map[string]ChannelDef{"main": {Enabled: boolPtr(false)}},
		Keys:    map[string]string{key.ID: "main"},
	}
	r.Set(dis)
	info3, bound3 := r.Resolve(key.ID)
	if !bound3 || info3.Enabled {
		t.Fatalf("停用渠道应返回 bound 且 Enabled=false: %+v bound=%v", info3, bound3)
	}
}

// 绑定明细：未命中的 ref 也要列出（让运营看见，而不是静默失效）。
func TestChannelRouterBindings(t *testing.T) {
	store, err := apikeys.New(filepath.Join(t.TempDir(), "k.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.Create("真密钥")
	if err != nil {
		t.Fatal(err)
	}
	ch := &Channels{
		Enabled: true,
		Groups:  map[string]ChannelDef{"main": {}},
		Keys:    map[string]string{key.ID: "main", "sk-wb2a-deadbeef": "main"},
	}
	if err := normalizeChannels(ch); err != nil {
		t.Fatal(err)
	}
	r := newChannelRouter(ch, store)
	rows := r.status().Bindings
	if len(rows) != 2 {
		t.Fatalf("应列出全部绑定: %v", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("应列出全部绑定: %v", rows)
	}
	var hit, miss int
	for _, row := range rows {
		if row.Ref == key.ID {
			if !row.Matched || row.KeyName != "真密钥" || row.Kind != "id" {
				t.Errorf("ID 绑定应命中: %+v", row)
			}
			hit++
		} else {
			if row.Matched || row.Kind != "prefix" {
				t.Errorf("无效前缀应标为未命中: %+v", row)
			}
			miss++
		}
	}
	if hit != 1 || miss != 1 {
		t.Errorf("hit/miss=%d/%d want 1/1", hit, miss)
	}
}

// ParseConfig 集成：非法渠道段必须在启动/保存时 fail fast。
func TestParseConfigChannels(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"channels":{"groups":{"g":{}},"keys":{"k1":"nope"}}}`)); err == nil {
		t.Error("绑定未定义分组应被 ParseConfig 拒绝")
	}
	if _, err := ParseConfig([]byte(`{"channels":{"groups":{"g":{"realm":"us"}}}}`)); err == nil {
		t.Error("非法 realm 应被 ParseConfig 拒绝")
	}
	cfg, err := ParseConfig([]byte(`{"channels":{"enabled":true,"groups":{"g":{"realm":"global","models":["gpt-*"]}},"keys":{"k1":"g"}}}`))
	if err != nil {
		t.Fatalf("合法渠道段: %v", err)
	}
	if !cfg.Channels.Enabled || cfg.Channels.Keys["k1"] != "g" {
		t.Errorf("渠道段未正确解析: %+v", cfg.Channels)
	}
	// 缺省（无 channels 键）：功能关闭且无绑定 = 零影响。
	def := Default()
	if err := def.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(def.Channels.Keys) != 0 {
		t.Errorf("缺省不应有绑定: %+v", def.Channels.Keys)
	}
}
