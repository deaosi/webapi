package server

import (
	"testing"
)

// 外部渠道的模型分流：命中渠道模型集走上游，未命中回落账号池。
//
// 这个语义是"绑定渠道后池子原有模型仍可用"的实现基础，改动它会直接让用户的
// 存量模型集体失效，所以用测试钉住。
//
// 注意 ChannelServesModel 现在吃 ChannelInfo：白名单非空就按白名单判，
// 留空则去取上游模型表（测试里用不可达地址构造"拉不到"的情形）。
func TestChannelServesModelSplit(t *testing.T) {
	ext := func(models ...string) ChannelInfo {
		return ChannelInfo{Group: "g", UpstreamURL: "https://up.example.com/v1",
			UpstreamKey: "sk-x", Models: models}
	}
	cases := []struct {
		name  string
		ch    ChannelInfo
		model string
		want  bool
	}{
		{"精确命中", ext("glm-5.3"), "glm-5.3", true},
		{"未命中=回落账号池", ext("glm-5.3"), "deepseek-v4", false},
		{"前缀通配命中", ext("glm-5*"), "glm-5.3-flash", true},
		{"前缀通配未命中", ext("glm-5*"), "kimi-k3", false},
		{"多条目任一命中", ext("kimi-k3", "glm-5.2"), "glm-5.2", true},
		{"大小写敏感(不误判)", ext("GLM-5.3"), "glm-5.3", false},
		{"空白条目被跳过", ext("", "  "), "glm-5.3", false},
		{"星号=全部承接", ext("*"), "任意模型名", true},
		// 留空 = 自动发现上游模型。上游不可达时拉不到 → 暂不承接（安全降级），
		// 请求回落账号池，而不是把所有请求导到一个联系不上的上游。
		{"留空+上游不可达=暂不承接", ext(), "glm-5.3", false},
	}
	for _, c := range cases {
		if got := ChannelServesModel(c.ch, c.model); got != c.want {
			t.Errorf("%s: ChannelServesModel(%v, %q) = %v, want %v", c.name, c.ch.Models, c.model, got, c.want)
		}
	}
	// 非外部渠道永不承接（账号池渠道的模型由池子决定，不走这条判定）
	if ChannelServesModel(ChannelInfo{Group: "g", Models: []string{"*"}}, "glm-5.3") {
		t.Error("账号池渠道不应被判为承接模型（那是 /v1/models 过滤的事）")
	}
}

// ChannelInfo.IsExternal 决定"转发还是选号"，判空必须严（首尾空白不算地址）。
func TestChannelInfoIsExternal(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"https://api.example.com/v1", true},
		{" http://127.0.0.1:7863/v1 ", true},
	}
	for _, c := range cases {
		if got := (ChannelInfo{UpstreamURL: c.url}).IsExternal(); got != c.want {
			t.Errorf("IsExternal(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// 空名单在两种语义下的对照：账号池渠道的硬性放行名单里空=全放开，
// 外部渠道的模型集里空=自动取上游模型表（拉不到则不承接）。二者刻意不同。
func TestEmptyWhitelistSemanticsDiffer(t *testing.T) {
	if !ChannelAllowedModel(nil, "glm-5.3") {
		t.Error("账号池渠道：空名单应表示不限制模型（全放开）")
	}
	// 外部渠道留空且上游不可达：不承接，请求回落账号池
	unreachable := ChannelInfo{UpstreamURL: "https://127.0.0.1:1/v1", UpstreamKey: "sk-x"}
	if ChannelServesModel(unreachable, "glm-5.3") {
		t.Error("外部渠道：留空且上游不可达时应暂不承接（回落账号池）")
	}
	// 显式 `*` 必须真的全承接（运维明确的独占意图）
	if !ChannelServesModel(ChannelInfo{UpstreamURL: "https://x/v1", UpstreamKey: "k", Models: []string{"*"}}, "任意模型") {
		t.Error("外部渠道：`*` 应表示全部承接")
	}
}

// 外部渠道的模型条目只列具体名字：通配写法不是可调用的模型名，列出去客户端会照抄失败。
func TestExternalChannelModelsSkipsWildcards(t *testing.T) {
	ch := ChannelInfo{Label: "ext", UpstreamURL: "https://up.example.com/v1", UpstreamKey: "sk-x",
		Models: []string{"glm-5*", "kimi-k3", "", "  ", "deepseek-v4-pro-0813"}}
	got := externalChannelModels(ch)
	if len(got) != 2 {
		t.Fatalf("应只列 2 个具体模型，实得 %d: %v", len(got), got)
	}
	ids := map[string]bool{}
	for _, m := range got {
		ids[m["id"].(string)] = true
		if m["owned_by"] != "ext" {
			t.Errorf("owned_by 应为渠道展示名，实得 %v", m["owned_by"])
		}
	}
	if !ids["kimi-k3"] || !ids["deepseek-v4-pro-0813"] {
		t.Errorf("具体模型名应原样列出（不带 realm 前缀），实得 %v", ids)
	}
	if ids["glm-5*"] {
		t.Error("通配条目不应进模型列表")
	}
}
