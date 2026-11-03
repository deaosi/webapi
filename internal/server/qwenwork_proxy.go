// 千问办公（QwenWork）渠道：把 gateway.qwenwork.cn 当成一个上游端点。
//
// 与 OpenAI 兼容渠道的差别：
//   - 没有静态 key，认证是设备授权得到的 access/refresh token（每个渠道可挂多个账号）；
//   - 每个请求都要带 cosy 签名头（见 internal/qwenwork/sign.go）；
//   - 请求体不是标准 OpenAI 结构，必须用 qwenwork.BuildBody 重建；
//   - 响应是「信封套信封」的 SSE，要拆信封再写回客户端（见 internal/qwenwork/sse.go）。
//
// 路由口径与外部渠道完全一致：渠道模型集（ChannelModelSet）命中 → 走这里；
// 未命中 → 回落账号池。所以绑定一个千问办公渠道 = 给该 key 增加一组模型。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/qwenwork"
)

const (
	// QwenworkModelTTL 模型目录缓存时长（上游目录变动不频繁）。
	QwenworkModelTTL = 5 * time.Minute
	// QwenworkModelFailTTL 拉取失败后的冷却，避免每次请求都去打上游。
	QwenworkModelFailTTL = 30 * time.Second
	// QwenworkModelTimeout 拉取模型目录的预算（面板按钮与后台预热共用）。
	QwenworkModelTimeout = 20 * time.Second
	// QwenworkRefreshTimeout 刷新令牌的预算。
	QwenworkRefreshTimeout = 20 * time.Second
)

// qwenworkHTTP 进程级客户端。刻意不设 client 级 Timeout：对话是长 SSE，
// 全局超时会误杀在途流（与账号池 chat 同款取舍）；短请求各自带 context 超时。
var qwenworkHTTP = &qwenwork.Client{HTTP: &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}}

type qwenworkModelEntry struct {
	ids    []string
	at     time.Time
	failed bool
}

var qwenworkModels = struct {
	mu    sync.Mutex
	items map[string]qwenworkModelEntry
}{items: map[string]qwenworkModelEntry{}}

// qwenworkState 运行期账号状态：轮询游标 + 刷新后的令牌（不写回配置，
// 避免请求路径改配置文件；配置里的 refresh_token 才是权威凭据）。
var qwenworkState = struct {
	mu        sync.Mutex
	next      map[string]int
	refreshed map[string]qwenwork.Account
}{next: map[string]int{}, refreshed: map[string]qwenwork.Account{}}

// QwenworkModelIDsCached 只读模型目录缓存（命中且未过期才返回）。
func QwenworkModelIDsCached(ch ChannelInfo) []string {
	key := qwenworkCacheKey(ch)
	qwenworkModels.mu.Lock()
	defer qwenworkModels.mu.Unlock()
	entry, ok := qwenworkModels.items[key]
	if !ok {
		return nil
	}
	ttl := QwenworkModelTTL
	if entry.failed {
		ttl = QwenworkModelFailTTL
	}
	if time.Since(entry.at) >= ttl {
		return nil
	}
	return entry.ids
}

// FetchQwenworkModels 同步拉取某渠道的模型目录（面板"拉模型"与后台预热用）。
// 失败时保留上一次成功的结果（与外部渠道缓存同一取舍：一次抖动不该让渠道空掉）。
func FetchQwenworkModels(ch ChannelInfo, timeout time.Duration) ([]string, error) {
	key := qwenworkCacheKey(ch)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for _, acc := range ch.Accounts {
		if strings.TrimSpace(acc.AccessToken) == "" {
			continue
		}
		models, err := qwenworkHTTP.ListModels(ctx, acc)
		if err != nil {
			lastErr = err
			continue
		}
		ids := make([]string, 0, len(models))
		for _, m := range models {
			if k := strings.TrimSpace(m.Key); k != "" {
				ids = append(ids, k)
			}
		}
		qwenworkModels.mu.Lock()
		qwenworkModels.items[key] = qwenworkModelEntry{ids: ids, at: time.Now()}
		qwenworkModels.mu.Unlock()
		return ids, nil
	}
	if lastErr == nil {
		lastErr = errors.New("该渠道没有可用的千问办公账号")
	}
	qwenworkModels.mu.Lock()
	prev := qwenworkModels.items[key]
	qwenworkModels.items[key] = qwenworkModelEntry{ids: prev.ids, at: time.Now(), failed: true}
	qwenworkModels.mu.Unlock()
	return prev.ids, lastErr
}

// PrimeQwenworkModels 后台预热模型目录（配置热生效时调用，失败无副作用）。
func PrimeQwenworkModels(ch ChannelInfo) {
	if ch.Type != "qwenwork" || len(ch.Accounts) == 0 {
		return
	}
	go func() { _, _ = FetchQwenworkModels(ch, QwenworkModelTimeout) }()
}

func qwenworkCacheKey(ch ChannelInfo) string {
	if ch.Group != "" {
		return ch.Group
	}
	if ch.Label != "" {
		return ch.Label
	}
	return ch.UpstreamURL
}

// qwenworkPickAccount 选一个可用账号：按渠道名轮询，跳过已过期且刷新失败的。
func qwenworkPickAccount(ch ChannelInfo) (qwenwork.Account, error) {
	if len(ch.Accounts) == 0 {
		return qwenwork.Account{}, errors.New("该渠道还没有授权任何千问办公账号")
	}
	key := qwenworkCacheKey(ch)
	qwenworkState.mu.Lock()
	start := qwenworkState.next[key]
	qwenworkState.next[key] = start + 1
	qwenworkState.mu.Unlock()

	now := time.Now()
	var lastErr error
	for i := 0; i < len(ch.Accounts); i++ {
		acc := ch.Accounts[(start+i)%len(ch.Accounts)]
		if acc.UID == "" {
			acc.UID = acc.Label()
		}
		qwenworkState.mu.Lock()
		if live, ok := qwenworkState.refreshed[acc.UID]; ok {
			acc = live
		}
		qwenworkState.mu.Unlock()
		if !acc.Expired(now) {
			return acc, nil
		}
		refreshed, err := qwenworkRefresh(acc)
		if err != nil {
			lastErr = err
			continue
		}
		return refreshed, nil
	}
	if lastErr == nil {
		lastErr = errors.New("千问办公账号全部不可用")
	}
	return qwenwork.Account{}, lastErr
}

// qwenworkRefresh 刷新令牌并记住结果（下次选号直接用新的）。
func qwenworkRefresh(acc qwenwork.Account) (qwenwork.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QwenworkRefreshTimeout)
	defer cancel()
	next, err := qwenworkHTTP.Refresh(ctx, acc)
	if err != nil {
		return acc, err
	}
	qwenworkState.mu.Lock()
	qwenworkState.refreshed[acc.UID] = next
	qwenworkState.mu.Unlock()
	log.Printf("INFO: [qwenwork] 账号 %s 令牌已刷新（expires_at=%d）", acc.Label(), next.ExpiresAt)
	return next, nil
}

// proxyQwenwork 把一次 chat 请求转给千问办公并回写响应。
func proxyQwenwork(w http.ResponseWriter, r *http.Request, ch ChannelInfo, rawBody []byte, bareModel string) {
	req, err := qwenwork.ParseChatRequest(rawBody)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	modelKey := qwenwork.MapModel(bareModel)
	upstreamBody, err := qwenwork.BuildBody(req, modelKey)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "build qwenwork body: "+err.Error())
		return
	}

	acc, err := qwenworkPickAccount(ch)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", err.Error())
		return
	}

	start := time.Now()
	resp, err := qwenworkHTTP.Chat(r.Context(), acc, upstreamBody, modelKey)
	if err != nil {
		log.Printf("WARN: [qwenwork] channel=%q account=%s: %v", ch.logName(), acc.Label(), err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable", "qwenwork request failed: "+err.Error())
		return
	}
	// 令牌失效：刷新后重试一次（401/403 是上游对过期 token 的标准回应）。
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && acc.RefreshToken != "" {
		resp.Body.Close()
		refreshed, rerr := qwenworkRefresh(acc)
		if rerr == nil {
			acc = refreshed
			resp, err = qwenworkHTTP.Chat(r.Context(), acc, upstreamBody, modelKey)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable", "qwenwork request failed: "+err.Error())
				return
			}
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		log.Printf("WARN: [qwenwork] channel=%q account=%s status=%d body=%s",
			ch.logName(), acc.Label(), resp.StatusCode, truncateForLog(msg, 300))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_request_failed", "qwenwork: "+truncateForLog(msg, 400))
		return
	}

	if req.Stream {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		flush()
		_, serr := qwenwork.StreamTo(w, resp.Body, bareModel, flush)
		if serr != nil {
			log.Printf("WARN: [qwenwork] channel=%q account=%s stream: %v", ch.logName(), acc.Label(), serr)
		}
		log.Printf("INFO: [qwenwork] channel=%q account=%s model=%s stream done in %s",
			ch.logName(), acc.Label(), modelKey, time.Since(start).Round(time.Millisecond))
		return
	}

	chunks, serr := collectQwenworkStream(resp.Body)
	if serr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_stream_error", "qwenwork stream: "+serr.Error())
		return
	}
	out := qwenwork.Aggregate(chunks, bareModel)
	log.Printf("INFO: [qwenwork] channel=%q account=%s model=%s sync done in %s",
		ch.logName(), acc.Label(), modelKey, time.Since(start).Round(time.Millisecond))
	writeJSONBody(w, http.StatusOK, out)
}

// collectQwenworkStream 把上游 SSE 收齐并返回清理过的 chunk 文本（非流式聚合用）。
func collectQwenworkStream(body io.Reader) ([]string, error) {
	var chunks []string
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := indexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimRight(string(buf[:idx]), "\r")
				buf = buf[idx+1:]
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				inner, ok, errMsg := qwenwork.UnwrapFrame(line[5:])
				if errMsg != "" {
					return chunks, errors.New(errMsg)
				}
				if !ok {
					continue
				}
				if cleaned := qwenwork.CleanChunk(inner); cleaned != "" {
					chunks = append(chunks, cleaned)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return chunks, err
		}
	}
	return chunks, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// writeJSONBody 写一条 JSON 响应（本文件专用，避免与 handler 的同名工具耦合）。
func writeJSONBody(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// truncateForLog 截断日志里的上游响应体。
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// QwenworkDeviceStart 生成一次千问办公设备授权会话（面板"添加账号"第一步）。
func QwenworkDeviceStart() (qwenwork.DeviceFlow, error) {
	return qwenwork.NewDeviceFlow()
}

// QwenworkPollGrant 轮询授权结果；成功后补一次用户信息（拿昵称/邮箱，失败不影响可用）。
func QwenworkPollGrant(ctx context.Context, flow qwenwork.DeviceFlow) (qwenwork.Account, bool, error) {
	acc, pending, err := qwenworkHTTP.PollGrant(ctx, flow)
	if err != nil || pending {
		return acc, pending, err
	}
	if uid, name, email, ierr := qwenworkHTTP.UserInfo(ctx, acc); ierr == nil {
		if uid != "" {
			acc.UID = uid
		}
		if name != "" {
			acc.Nickname = name
		}
		if email != "" {
			acc.Email = email
		}
	}
	return acc, false, nil
}

// QwenworkChannelModels 按渠道定义拉模型目录（面板"拉模型"按钮用）。
func QwenworkChannelModels(ch ChannelInfo) ([]string, error) {
	return FetchQwenworkModels(ch, QwenworkModelTimeout)
}

// QwenworkAccountFromToken 用一枚手工粘贴的令牌建账号（设备授权的备用通道）。
//
// 为什么需要：设备授权要在浏览器里登录千问办公再等回调，网络或浏览器环境不配合时
// 整条链路就卡在"没有账号"。这里允许直接粘贴 access_token：先真打一次 UserInfo 确认
// 令牌有效（无效就当场报错，而不是存进配置等请求时才炸），再交给调用方落库。
func QwenworkAccountFromToken(ctx context.Context, ch ChannelInfo, access, refresh string) (qwenwork.Account, error) {
	access = strings.TrimSpace(access)
	refresh = strings.TrimSpace(refresh)
	if access == "" {
		return qwenwork.Account{}, fmt.Errorf("access_token 不能为空")
	}
	acc := qwenwork.Account{AccessToken: access, RefreshToken: refresh}
	base := strings.TrimSpace(ch.UpstreamURL)
	if base == "" {
		base = qwenwork.BaseURL
	}
	client := &qwenwork.Client{BaseURL: base, HTTP: &http.Client{Timeout: QwenworkModelTimeout}}
	uid, name, email, err := client.UserInfo(ctx, acc)
	if err != nil {
		return qwenwork.Account{}, fmt.Errorf("令牌校验失败：%w", err)
	}
	acc.UID = strings.TrimSpace(uid)
	acc.Nickname = strings.TrimSpace(name)
	acc.Email = strings.TrimSpace(email)
	return acc, nil
}
