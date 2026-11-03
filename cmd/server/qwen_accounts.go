// qwen_accounts.go 千问办公渠道的账号表读写。
//
// 面板只持有脱敏令牌（statusView 只下发尾 6 位），所以"加号/删号"必须由服务端按
// 自己的配置副本完成：读盘 → 改 channels.groups.<name>.accounts → 走与面板同一个
// saveConfig（校验 → 原子落盘 → 热应用 → 重建 channelRouter）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qwenwork"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mutateQwenAccounts 对 qwenwork 渠道的账号表做一次原子改动。
func mutateQwenAccounts(path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client,
	sch *scheduler.Scheduler, chRouter *channelRouter, fn func(groups map[string]any) error) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	ch, _ := root["channels"].(map[string]any)
	if ch == nil {
		return fmt.Errorf("配置里没有 channels 段")
	}
	groups, _ := ch["groups"].(map[string]any)
	if groups == nil {
		return fmt.Errorf("配置里没有 channels.groups 段")
	}
	if err := fn(groups); err != nil {
		return err
	}
	out, err := json.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if _, err := saveConfig(out, path, live, p, up, sch, chRouter); err != nil {
		return err
	}
	return nil
}

// qwenGroup 取出某个千问办公渠道的定义（顺带校验类型，避免把账号写进 OpenAI 渠道）。
func qwenGroup(groups map[string]any, name string) (map[string]any, error) {
	v, ok := groups[name]
	if !ok {
		return nil, fmt.Errorf("渠道 %q 不存在（可能刚被删除，刷新后重试）", name)
	}
	def, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("渠道 %q 结构异常", name)
	}
	if t, _ := def["type"].(string); strings.TrimSpace(t) != "qwenwork" {
		return nil, fmt.Errorf("渠道 %q 不是千问办公渠道", name)
	}
	return def, nil
}

// qwenAccountMap 把账号转成配置里的 JSON 形态（字段名与 qwenwork.Account 的 tag 一致）。
func qwenAccountMap(a qwenwork.Account) map[string]any {
	m := map[string]any{"access_token": a.AccessToken}
	if a.UID != "" {
		m["uid"] = a.UID
	}
	if a.Nickname != "" {
		m["nickname"] = a.Nickname
	}
	if a.Email != "" {
		m["email"] = a.Email
	}
	if a.RefreshToken != "" {
		m["refresh_token"] = a.RefreshToken
	}
	if a.ExpiresAt != 0 {
		m["expires_at"] = a.ExpiresAt
	}
	return m
}

// upsertQwenAccount 追加账号；同 uid（或同令牌）视为同一个号，替换而不是重复追加
// ——设备授权反复点"生成链接"会拿到新令牌，重复追加只会让轮询永远挑到旧的那个。
func upsertQwenAccount(def map[string]any, acc qwenwork.Account) {
	list, _ := def["accounts"].([]any)
	for i, it := range list {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		same := false
		if acc.UID != "" {
			if u, _ := m["uid"].(string); u == acc.UID {
				same = true
			}
		}
		if !same && acc.AccessToken != "" {
			if t, _ := m["access_token"].(string); t == acc.AccessToken {
				same = true
			}
		}
		if same {
			list[i] = qwenAccountMap(acc)
			def["accounts"] = list
			return
		}
	}
	def["accounts"] = append(list, qwenAccountMap(acc))
}
