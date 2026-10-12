// pick.go 面板轮询策略接口：策略快照查询 + 分布计数清零。
//
// 读路径：GET /panel/api/pick → PickPolicyStatus（模式/组表/账号行/分布）。
// 写路径：轮询策略走既有 /panel/api/config 深合并保存（热生效），本文件不设
// 独立保存端点——避免两套写入口漂移。
// 清零：POST /panel/api/pick/stats/reset → ResetPickStats（SWRR current +
// 分布计数归零，用于验证比例后重新计时）。
package panel

import (
	"net/http"
)

// pickPolicy 返回当前 pick 策略快照（只读）。
func (p *Panel) pickPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"pick": p.cfg.Pool.PickPolicyStatus(),
	})
}

// resetPickStats 清零 SWRR current 与分布计数。
func (p *Panel) resetPickStats(w http.ResponseWriter, r *http.Request) {
	p.cfg.Pool.ResetPickStats()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
