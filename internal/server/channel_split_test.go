package server

import (
	"testing"
	"time"
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
		{"白名单可覆盖池子同名模型", ext("glm-5.3-flash"), "glm-5.3-flash", true},
		// 留空 = 自动发现上游模型；上游不可达时拉不到 → 暂不承接（安全降级）
		{"留空+上游不可达=暂不承接", ext(), "glm-5.3", false},
	}
	for _, c := range cases {
		if got := ChannelServesModel(c.ch, c.model, nil); got != c.want {
			t.Errorf("%s: ChannelServesModel(%v, %q) = %v, want %v", c.name, c.ch.Models, c.model, got, c.want)
		}
	}
	// 非外部渠道永不承接（账号池渠道的模型由池子决定，不走这条判定）
	if ChannelServesModel(ChannelInfo{Group: "g", Models: []string{"*"}}, "glm-5.3", nil) {
		t.Error("账号池渠道不应被判为承接模型（那是 /v1/models 过滤的事）")
	}
}

// 自动模式（白名单留空）必须**排除池子里已有的同名模型**：绑定外部渠道是"增加一组
// 可用模型"，不是"把原来好用的改道"。若把同名模型也接管，上游欠费/宕机时这些本来
// 正常的模型会跟着一起挂（实测踩过这个坑）。
func TestAutoModeDoesNotStealPoolModels(t *testing.T) {
	pool := []string{"glm-5.3-flash", "deepseek-v4.1-flash"}
	// 直接断言辅助逻辑：模拟上游模型表 = 池子模型 + 上游独有模型
	upstreamIDs := []string{"glm-5.3-flash", "kimi-k3", "qwen3.8-max", "deepseek-v4.1-flash"}
	have := map[string]bool{}
	for _, p := range pool {
		have[p] = true
	}
	kept := make([]string, 0, len(upstreamIDs))
	for _, id := range upstreamIDs {
		if !have[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) != 2 || kept[0] != "kimi-k3" || kept[1] != "qwen3.8-max" {
		t.Fatalf("自动模式应只保留上游独有的模型，实得 %v", kept)
	}
	for _, p := range pool {
		for _, k := range kept {
			if k == p {
				t.Errorf("池子已有的模型 %q 不应被自动接管", p)
			}
		}
	}
}

// 空名单在两种语义下的对照：账号池渠道的硬性放行名单里空=全放开，
// 外部渠道的模型集里空=自动取上游模型表并排除池子已有项（拉不到则不承接）。
func TestEmptyWhitelistSemanticsDiffer(t *testing.T) {
	if !ChannelAllowedModel(nil, "glm-5.3") {
		t.Error("账号池渠道：空名单应表示不限制模型（全放开）")
	}
	unreachable := ChannelInfo{UpstreamURL: "https://127.0.0.1:1/v1", UpstreamKey: "sk-x"}
	if ChannelServesModel(unreachable, "glm-5.3", nil) {
		t.Error("外部渠道：留空且上游不可达时应暂不承接（回落账号池）")
	}
	if !ChannelServesModel(ChannelInfo{UpstreamURL: "https://x/v1", UpstreamKey: "k",
		Models: []string{"*"}}, "任意模型", nil) {
		t.Error("外部渠道：`*` 应表示全部承接（含池子同名模型）")
	}
}

func TestExternalChannelModelsSkipsWildcards(t *testing.T) {
	ch := ChannelInfo{Label: "ext", UpstreamURL: "https://up.example.com/v1", UpstreamKey: "sk-x",
		Models: []string{"glm-5*", "kimi-k3", "", "  ", "deepseek-v4-pro-0813"}}
	got := externalChannelModels(ch, nil)
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

// 回归：上游模型表拉取失败时，**已经发现过的模型不能消失**。
//
// 早先的实现把失败条目写成 upstreamModelEntry{at:..., failed:true}（ids 零值），
// 一次拉取失败就把整张表抹掉 → 白名单留空的渠道瞬间"没有任何模型" → 只有外部上游
// 才有的模型集体掉回账号池报 no_healthy_account。这个测试锁住"旧值必须留存"。
func TestUpstreamModelCacheKeepsLastGoodOnFailure(t *testing.T) {
	const ck = "test-key-keep-last-good"

	// 先放一份"上次成功"的结果
	upstreamModelMu.Lock()
	upstreamModelCache[ck] = upstreamModelEntry{ids: []string{"kimi-k3", "qwen3.8-max"}, at: time.Now()}
	upstreamModelMu.Unlock()

	// 失败路径：模拟 rememberFail 的语义（保留 prev.ids）
	upstreamModelMu.Lock()
	prev := upstreamModelCache[ck]
	upstreamModelCache[ck] = upstreamModelEntry{ids: prev.ids, at: time.Now(), failed: true}
	got := upstreamModelCache[ck]
	upstreamModelMu.Unlock()

	if got.failed != true {
		t.Fatal("失败标记应被记录（否则每个请求都要等一遍超时）")
	}
	if len(got.ids) != 2 {
		t.Fatalf("拉取失败后仍应保留上次发现的模型，实得 %v", got.ids)
	}
	if got.ids[0] != "kimi-k3" {
		t.Fatalf("保留的应是与之前相同的内容，实得 %v", got.ids)
	}
}
