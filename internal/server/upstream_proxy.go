// 外部上游渠道转发：把绑定到该渠道的调用密钥请求原样转发到渠道配置的上游端点。
//
// NewAPI 里「渠道」就是一个上游端点（base_url + key），请求按渠道配置转发。本网关原本
// 只有 WorkBuddy 账号池一种上游，渠道被解释成"账号分组"；现在补齐第二种：**外部 OpenAI
// 兼容端点**。填了 upstream_url 的渠道，请求走这里，不进账号池、不选号、不消耗账号积分。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// upstreamHTTP 外部上游专用 client：与账号池链路完全隔离（超时/连接策略独立），
// 外部上游抖动不应影响账号池的连接池，反之亦然。
var upstreamHTTP = &http.Client{
	// 不设 Timeout：长思考/长输出的合法时长可达数分钟，全局超时会误杀在途流
	//（与账号池 chat 的 Timeout=0 同款取舍）。空闲由 Transport 的 IdleConnTimeout 兜底。
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	},
}

// 注：刻意**不设** client 级 Timeout——长思考/长输出的合法时长可达数分钟，全局超时会
// 误杀在途流（与账号池 chat 的 Timeout=0 同款取舍）。空闲连接由上面 IdleConnTimeout 回收。

// proxyToUpstream 把请求转发到渠道的上游端点，并把响应（含 SSE 流）原样写回客户端。
//
// 为什么不由账号池链路处理：外部上游不是 WorkBuddy，没有账号/积分/冷却/选号的概念，
// 走池子既无意义又会污染账号统计与冷却状态。
//
// 语义边界（刻意"薄"）：
//   - 只转发 chat 形态（/v1/chat/completions）；responses / messages 由适配层负责转换，
//     外部上游若只支持 OpenAI chat，就不该由网关伪造 Anthropic 语义。
//   - 请求体原样转发（客户端已按 OpenAI 兼容格式组装），只做两处必要改写：
//     1) Authorization 换成渠道的上游密钥（客户端拿的是本网关的调用密钥，上游不认）；
//     2) model 名去掉网关侧的 realm 前缀（"cn:glm-5.2" → "glm-5.2"），
//        前缀是本网关内部的路由协议，外部上游不认。
//   - 上游 ≥400 直接透传其状态码与响应体：不改写、不翻译、不"善意"重试——
//     上游的错误信息比网关编造的更有用。
func proxyToUpstream(w http.ResponseWriter, r *http.Request, ch ChannelInfo, rawBody []byte, bareModel string) {
	base := strings.TrimRight(ch.UpstreamURL, "/")
	target := base + "/chat/completions"

	// 请求体只做一处改写：model 名去掉网关侧 realm 前缀（"cn:glm-5.2" → "glm-5.2"）。
	// 前缀是本网关内部的路由协议，外部上游不认；裸名时 rewriteModel 恒等返回原 body。
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target,
		bytes.NewReader(rewriteModel(rawBody, bareModel)))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_request_failed", "build upstream request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ch.UpstreamKey)
	// 上游若有 Accept 偏好则由其自行处理；不转发客户端的 Accept-Encoding 之外的头，
	// 避免把本网关的内部头（X-Trace-ID 等）泄漏给第三方上游。
	if ae := r.Header.Get("Accept-Encoding"); ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	}

	start := time.Now()
	resp, err := upstreamHTTP.Do(req)
	if err != nil {
		// 上游不可达：与账号池链路的"换号重试"不同，外部渠道只有一个上游，
		// 重试只会放大上游压力；直接 502 并保留原因供排障。
		log.Printf("WARN: [server] upstream channel %q -> %s: %v", ch.Label, target, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable",
			"upstream channel request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// 响应头：只透出与客户端解码相关的少数几个，避免把上游的内部头原样外泄。
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "X-Ratelimit-Limit-Requests",
		"X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests", "X-Ratelimit-Limit-Tokens",
		"X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	// 上游非 2xx：透传其状态码与响应体（上游的错误信息比网关编造的更有用），
	// 但对 404 追加一句排障提示——实测最常见的成因是 base_url 漏了 /v1 前缀
	// （拼出来成了 "<host>/chat/completions"，上游自然 404）。
	if resp.StatusCode == http.StatusNotFound {
		log.Printf("WARN: [server] upstream channel %q 404：请检查 upstream_url 是否含 /v1 前缀（当前 %q → %s）",
			ch.Label, base, target)
	}
	w.WriteHeader(resp.StatusCode)

	// 流式（SSE）要边收边发：客户端等着首字节，不能等上游整条流结束再写。
	// text/event-stream / application/x-ndjson 都按流处理；其余直接拷贝。
	if flusher, ok := w.(http.Flusher); ok && isStreamResponse(resp.Header.Get("Content-Type")) {
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return // 客户端已断连
				}
				flusher.Flush()
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("WARN: [server] upstream stream %q: %v", ch.Label, err)
				}
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
	log.Printf("[server] upstream channel=%q model=%s status=%d %s",
		ch.Label, bareModel, resp.StatusCode, time.Since(start).Round(time.Millisecond))
}

// isStreamResponse 判断上游响应是否按流式处理（SSE 或 ndjson）。
func isStreamResponse(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/event-stream") || strings.Contains(ct, "application/x-ndjson")
}

// ── 上游模型表（带缓存）──
//
// 为什么需要：白名单留空的外部渠道"全部走外部上游"，此时这个 key 到底能调哪些模型
// 只有上游知道；面板也需要它来帮用户填白名单（否则得去翻上游文档抄模型名）。
//
// 为什么缓存：/v1/models 是客户端启动时的高频调用，每个请求都去上游拉一次既慢又会
// 触发上游限流；模型表变化很慢，5 分钟足够新。缓存键是「基址+密钥指纹」，改配置后
// 自动失效（不会拿旧上游的模型表糊弄新上游）。
var (
	upstreamModelMu    sync.Mutex
	upstreamModelCache = map[string]upstreamModelEntry{}
)

type upstreamModelEntry struct {
	ids []string
	at  time.Time
}

// upstreamModelTTL 上游模型表缓存时长。
const upstreamModelTTL = 5 * time.Minute

// FetchUpstreamModelIDs 拉取上游的模型 id 列表（GET <base>/models），带 5 分钟缓存。
//
// 供 /v1/models 与面板"拉取上游模型"共用。任何失败都原样返回 error，由调用方决定
// 降级策略（网关侧退回池子目录，面板侧提示用户手填）——这里不吞错、不返回空成功。
func FetchUpstreamModelIDs(base, key, label string) ([]string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, errors.New("upstream url is empty")
	}
	// 密钥只参与指纹，不进缓存值：避免把上游密钥留在缓存结构里被日志/转储带出。
	sum := sha256.Sum256([]byte(base + "\x1f" + key))
	ck := hex.EncodeToString(sum[:])

	upstreamModelMu.Lock()
	if e, ok := upstreamModelCache[ck]; ok && time.Since(e.at) < upstreamModelTTL {
		ids := e.ids
		upstreamModelMu.Unlock()
		return ids, nil
	}
	upstreamModelMu.Unlock()

	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MiB 上限：模型表不该更大
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %s/modes: HTTP %d: %s", label, resp.StatusCode,
			strings.TrimSpace(string(body))[:min(len(body), 200)])
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("upstream %s/models: 响应不是 OpenAI 模型表格式: %w", label, err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	upstreamModelMu.Lock()
	if len(upstreamModelCache) > 64 { // 渠道数有界，但防御性清理
		upstreamModelCache = map[string]upstreamModelEntry{}
	}
	upstreamModelCache[ck] = upstreamModelEntry{ids: ids, at: time.Now()}
	upstreamModelMu.Unlock()
	return ids, nil
}

// externalChannelModels 为外部上游渠道合成"白名单里的具体模型"条目。
//
// 只列具体名字：通配条目（`glm-5*`）不是某个模型，列出去客户端也无法直接调用；
// 白名单留空时返回空（调用方改为拉上游模型表）。
func externalChannelModels(ch ChannelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(ch.Models))
	for _, m := range ch.Models {
		m = strings.TrimSpace(m)
		if m == "" || strings.HasSuffix(m, "*") {
			continue
		}
		// 外部上游的模型名不带本网关的 realm 前缀：前缀是内部路由协议，
		// 上游与客户端都不认。id 用裸名，与 chat 侧出站口径一致。
		out = append(out, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  1753600000,
			"owned_by": ch.Label,
		})
	}
	return out
}

// logfmt 引用保持：渠道转发日志与账号池日志同款（uid 标签），避免两条链路日志风格漂移。
var _ = logfmt.Label
