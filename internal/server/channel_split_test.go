package server

import (
	"testing"
)

// 外部渠道的模型分流：命中白名单走上游，未命中回落账号池，白名单留空=全部走上游。
//
// 这个语义是"绑定渠道后池子原有模型仍可用"的实现基础，改动它会直接让用户的
// 存量模型集体失效，所以用测试钉住。
func TestChannelServesModelSplit(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		model  string
		want   bool
	}{
		// 安全默认：空名单 = 不接管任何模型（绑定本身不改变现有行为）。
		// 曾经是"空=全部走上游"，效果是绑一下就把整条 key 打废（实测踩过两次），
		// 所以语义翻转为不接管；要独占就显式写 `*`。
		{"留空=不接管任何模型", nil, "anything", false},
		{"留空(空切片)=不接管", []string{}, "anything", false},
		{"星号=全部走上游", []string{"*"}, "anything", true},
		{"星号=全部走上游(带前缀模型名)", []string{"*"}, "cn:glm-5.3", true},
		{"精确命中", []string{"glm-5.3"}, "glm-5.3", true},
		{"未命中=回落账号池", []string{"glm-5.3"}, "deepseek-v4", false},
		{"前缀通配命中", []string{"glm-5*"}, "glm-5.3-flash", true},
		{"前缀通配未命中", []string{"glm-5*"}, "kimi-k3", false},
		{"多条目任一命中", []string{"kimi-k3", "glm-5.2"}, "glm-5.2", true},
		{"大小写敏感(不误判)", []string{"GLM-5.3"}, "glm-5.3", false},
		{"空白条目被跳过", []string{"", "  "}, "glm-5.3", false},
	}
	for _, c := range cases {
		if got := ChannelServesModel(c.models, c.model); got != c.want {
			t.Errorf("%s: ChannelServesModel(%v, %q) = %v, want %v", c.name, c.models, c.model, got, c.want)
		}
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
// 外部渠道的分流名单里空=不接管。二者刻意不同，改动任一个都会影响生产路由。
func TestEmptyWhitelistSemanticsDiffer(t *testing.T) {
	if !ChannelAllowedModel(nil, "glm-5.3") {
		t.Error("账号池渠道：空名单应表示不限制模型（全放开）")
	}
	if ChannelServesModel(nil, "glm-5.3") {
		t.Error("外部渠道：空名单应表示不接管任何模型（安全默认）")
	}
	// `*` 是"我明确要全部走外部上游"的写法，必须真的全承接。
	if !ChannelServesModel([]string{"*"}, "任意模型名") {
		t.Error("外部渠道：`*` 应表示全部承接")
	}
}

// 外部渠道的模型条目只列具体名字：通配写法不是可调用的模型名，列出去客户端会照抄失败。
func TestExternalChannelModelsSkipsWildcards(t *testing.T) {
	ch := ChannelInfo{Label: "ext", Models: []string{"glm-5*", "kimi-k3", "", "  ", "deepseek-v4-pro-0813"}}
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
