package upstream

import "testing"

func TestModelVersionExtractsUpstreamVersion(t *testing.T) {
	cases := []struct {
		name string
		desc string
		want string
	}{
		// 线上真实形态（2026-10 观测）。
		{
			name: "线上 deepseek-v4.1-flash 原样",
			desc: "已更新至 V4.1-Flash-1017最新版，支持1M上下文窗口，原生多模态",
			want: "1017",
		},
		{
			name: "版本号后面直接断句（无「最新版」）",
			desc: "已更新至 V4.1-Flash-1017，支持1M上下文窗口",
			want: "1017",
		},
		{
			name: "英文目录",
			desc: "Updated to GLM-5.3-Flash-0831 latest, 1M context",
			want: "0831",
		},
		{
			name: "「升级至」变体",
			desc: "已升级至 Kimi-K3-2-1104最新版",
			want: "1104",
		},
		// 反例：不能瞎认。
		{
			name: "没有更新说明的普通描述",
			desc: "原生多模态模型，擅长视觉理解与专业任务",
			want: "",
		},
		{
			name: "只有上下文数字，不能当版本号",
			desc: "已更新至最新版，支持1M上下文窗口，x0.11 credit",
			want: "",
		},
		{
			name: "空描述",
			desc: "",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ModelInfo{ID: "x", Name: "X", Description: c.desc}.ModelVersion()
			if got != c.want {
				t.Fatalf("ModelVersion(%q) = %q, want %q", c.desc, got, c.want)
			}
		})
	}
}

func TestDisplayNameAppendsVersionOnlyToName(t *testing.T) {
	mi := ModelInfo{
		ID:          "deepseek-v4.1-flash",
		Name:        "Deepseek-V4.1-Flash",
		Description: "已更新至 V4.1-Flash-1017最新版，支持1M上下文窗口，原生多模态",
	}
	if got, want := mi.DisplayName(), "Deepseek-V4.1-Flash1017"; got != want {
		t.Fatalf("DisplayName() = %q, want %q", got, want)
	}
	// 关键约束：展示名变了，id 必须一个字都不变（路由/白名单/客户端调用都用 id）。
	if mi.ID != "deepseek-v4.1-flash" {
		t.Fatalf("ID mutated to %q", mi.ID)
	}
}

func TestDisplayNameWithoutVersionIsUnchanged(t *testing.T) {
	mi := ModelInfo{ID: "glm-5.3", Name: "GLM-5.3", Description: "旗舰模型，擅长复杂软件工程"}
	if got := mi.DisplayName(); got != "GLM-5.3" {
		t.Fatalf("DisplayName() = %q, want unchanged %q", got, "GLM-5.3")
	}
}

func TestDisplayNameNoDoubleSuffix(t *testing.T) {
	// 上游哪天把版本号直接写进 name 了，也不能拼成 "X10171017"。
	mi := ModelInfo{
		ID:          "m",
		Name:        "Deepseek-V4.1-Flash1017",
		Description: "已更新至 V4.1-Flash-1017最新版",
	}
	if got := mi.DisplayName(); got != "Deepseek-V4.1-Flash1017" {
		t.Fatalf("DisplayName() = %q, want no duplicate suffix", got)
	}
}

func TestDisplayNameEmptyNameStaysEmpty(t *testing.T) {
	// name 缺失时面板/客户端回落显示 id；不能只吐一个裸版本号。
	mi := ModelInfo{ID: "m", Description: "已更新至 V4.1-Flash-1017最新版"}
	if got := mi.DisplayName(); got != "" {
		t.Fatalf("DisplayName() = %q, want empty", got)
	}
}
