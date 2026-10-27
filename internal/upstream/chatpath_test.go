package upstream

import (
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatPathsByRealm 锁定 realm → chat 路径候选序列：
// global 必须 /console 优先（2026-10 起 /v2 强制官方客户端渠道校验，直接 11-128），
// cn 必须单路径 /v2（现状零回归）；逃生门 GlobalEnabled=false 时 global 账号也回落 /v2。
func TestChatPathsByRealm(t *testing.T) {
	cn := &auth.Auth{AccessToken: "at", UID: "u1"}
	gl := &auth.Auth{AccessToken: "at", UID: "u2", Domain: "www.workbuddy.ai"}
	if gl.Realm() != "global" {
		t.Fatalf("test auth realm = %q, want global", gl.Realm())
	}

	c := &Client{GlobalEnabled: true}
	if got, want := c.chatPaths(cn), []string{chatCompletionsPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("cn paths = %v, want %v", got, want)
	}
	if got, want := c.chatPaths(gl), []string{consoleChatPath, chatCompletionsPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("global paths = %v, want %v", got, want)
	}

	off := &Client{GlobalEnabled: false}
	if got, want := off.chatPaths(gl), []string{chatCompletionsPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("global paths with GlobalEnabled=false = %v, want %v", got, want)
	}
}

// TestClassifyConsoleChannelGateNotAccountFault 上游 console 渠道校验的 11-128 是
// 路径/客户端形态层面的拒斥，绝不能判成账号级故障——否则一次失败就把健康账号禁用，
// 症状是整个 realm 后续全量 503 no_healthy_account。
func TestClassifyConsoleChannelGateNotAccountFault(t *testing.T) {
	gate := `{"code":11-128,"msg":"request illegal","requestId":"x","displayMsg":{"en":"Request blocked. Please send it again"},"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client"}}`
	if got := Classify(400, gate); got != ErrClient {
		t.Errorf("Classify(400, console gate) = %v, want ErrClient", got)
	}
	// 紧凑 JSON 变体（无空格）同样要被识别。
	compact := `{"code":11-128,"msg":"request illegal"}`
	if got := Classify(400, compact); got != ErrClient {
		t.Errorf("Classify(400, compact gate) = %v, want ErrClient", got)
	}
	// 真正的账号级授权风控（11140 request illegal）语义不得被削弱。
	if got := Classify(403, `{"code":11140,"msg":"request illegal"}`); got != ErrAccountFault {
		t.Errorf("Classify(403, 11140 request illegal) = %v, want ErrAccountFault", got)
	}
}

// TestRetryPathReject 路径级拒斥判定：只有 404/405 与 400+11-128 触发换路径。
func TestRetryPathReject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"404 endpoint missing", 404, `not found`, true},
		{"405 method not allowed", 405, ``, true},
		{"400 console gate", 400, `{"code":11-128,"msg":"request illegal"}`, true},
		{"400 other business error", 400, `{"code":11101,"msg":"bad params"}`, false},
		{"401 unauthorized", 401, `{"code":12153}`, false},
		{"500 server error", 500, `oops`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryPathReject(tc.status, tc.body); got != tc.want {
				t.Errorf("retryPathReject(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
