// 上游渠道（NewAPI 口径）：config.channels 的定义、校验与运行期解析。
//
// NewAPI 里「渠道」就是一个**上游端点**（base_url + key + 模型），调用密钥按分组匹配到
// 渠道，请求直接转发给该上游。本网关支持两种渠道：
//   - upstream_url 为空：走账号池（WorkBuddy 账号），按分组隔离选号（原有语义）；
//   - upstream_url 非空：外部 OpenAI 兼容端点，请求**原样转发**到该 URL，
//     Authorization 换成该渠道的 upstream_key，不走账号池、不消耗账号积分。
// 两种都由 channels.keys 的"调用密钥 → 渠道"绑定驱动，同一套隔离与热生效机制。
//
// server 包只认 server.ChannelInfo 与 server.ChannelResolver，本文件负责把 config 映射
// 过去，并处理"配置热生效"（saveConfig 后重建解析表，无需重启）。
package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// ChannelDef 单个渠道定义（config.channels.groups 的值）。
type ChannelDef struct {
	// Label 展示名（面板用，不参与路由）。
	Label string `json:"label"`
	// Realm 上游域："" = 不限制；"cn"/"global" = 该 key 只能走这个域
	// （裸模型名采用该域，显式前缀与之冲突则 403）。
	Realm string `json:"realm"`
	// Models 模型白名单（空 = 全放开）。裸名精确匹配，尾缀 "*" 做前缀匹配。
	Models []string `json:"models"`
	// Enabled 渠道启停。指针以区分"未配置"（nil → 启用）与显式 false（停用，
	// 该组所有 key 直接 403）。缺省即启用——漏配一个键不该把整组 key 断流。
	Enabled *bool `json:"enabled"`

	// ── 上游接入（填了就是外部上游渠道，不填就是账号池分组渠道）──
	// UpstreamURL 上游 OpenAI 兼容端点基址，如 "https://api.deepseek.com/v1"。
	// 非空时该渠道的请求转发到 "<UpstreamURL>/chat/completions"，不再走账号池。
	UpstreamURL string `json:"upstream_url"`
	// UpstreamKey 上游鉴权密钥（转发时替换 Authorization；/status 与面板列表脱敏展示）。
	UpstreamKey string `json:"upstream_key"`
	// UpstreamType 上游协议："" 或 "openai" = OpenAI 兼容（当前唯一实现）。
	// 预留枚举位，避免将来加 Claude/Gemini 原生协议时改结构。
	UpstreamType string `json:"upstream_type"`
}

// IsExternal 该渠道是否为外部上游（填了 upstream_url）。
func (d ChannelDef) IsExternal() bool { return strings.TrimSpace(d.UpstreamURL) != "" }

// EnabledOn 渠道是否启用（nil = 启用）。
func (d ChannelDef) EnabledOn() bool { return d.Enabled == nil || *d.Enabled }

// Channels 上游渠道段（config.channels）。
type Channels struct {
	// Enabled 渠道功能总开关：false 时忽略全部绑定（所有 key 视为未绑定）。
	// 缺省 true——但 keys 为空时没有任何绑定，同样零影响。
	Enabled bool `json:"enabled"`
	// Groups 渠道定义（key = 分组名）。
	Groups map[string]ChannelDef `json:"groups"`
	// Keys key → 分组名。key 可写**调用密钥 ID**（面板写入）或**密钥前缀**
	// （sk-wb2a-xxxxxxxx，便于手改配置）；ID 精确优先、前缀唯一命中。
	Keys map[string]string `json:"keys"`
}

// normalizeChannels 归一 + 校验 channels 段。校验只覆盖"配置自身是否自洽"：
//   - 分组名非空、不含控制分隔符（会破坏会话键命名空间隔离）；
//   - realm 只能是 ""/"cn"/"global"；
//   - 白名单条目去空白去空项并排序（输出稳定，测试与面板可比对）；
//   - keys 的分组名必须**已定义**（fail closed：绑定写错组名一律拒绝保存，
//     否则运行期只能二选一——静默放行=越权，静默 403=难以定位的断流）。
//
// 刻意**不**校验"key 是否真实存在"、"分组是否有成员账号"：密钥可随时增删、账号可随时
// 改组，硬校验会让启动因一次账号轮换而失败。这类未命中在面板与 /status 显式标注。
func normalizeChannels(ch *Channels) error {
	if ch.Groups == nil {
		ch.Groups = map[string]ChannelDef{}
	}
	if ch.Keys == nil {
		ch.Keys = map[string]string{}
	}
	for name, def := range ch.Groups {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("channels.groups: 分组名不能为空")
		}
		if strings.ContainsAny(name, "\x1f\x00") {
			return fmt.Errorf("channels.groups[%q]: 分组名不能含控制字符", name)
		}
		switch def.Realm {
		case "", "cn", "global":
		default:
			return fmt.Errorf("channels.groups[%q].realm: %q 非法（cn / global / 空）", name, def.Realm)
		}
		cleaned := make([]string, 0, len(def.Models))
		for _, m := range def.Models {
			if m = strings.TrimSpace(m); m != "" {
				cleaned = append(cleaned, m)
			}
		}
		sort.Strings(cleaned)
		def.Models = cleaned
		// 上游接入校验：只做"配置自身是否自洽"的检查（与既有风格一致）。
		//   - URL 必须 http/https 开头且能解析（拼路径转发前就发现手误，而不是每次请求才炸）；
		//   - 外部渠道必须有 key（空 key 转发到上游必然 401，属于配置错误，提前拦下）；
		//   - upstream_type 目前只允许 openai / 空（避免填错协议却静默按 openai 转发）。
		def.UpstreamURL = strings.TrimSpace(def.UpstreamURL)
		def.UpstreamType = strings.TrimSpace(def.UpstreamType)
		if def.UpstreamURL != "" {
			if !strings.HasPrefix(def.UpstreamURL, "http://") && !strings.HasPrefix(def.UpstreamURL, "https://") {
				return fmt.Errorf("channels.groups[%q].upstream_url: 必须以 http:// 或 https:// 开头", name)
			}
			if _, err := url.ParseRequestURI(def.UpstreamURL); err != nil {
				return fmt.Errorf("channels.groups[%q].upstream_url: %w", name, err)
			}
			if strings.TrimSpace(def.UpstreamKey) == "" {
				return fmt.Errorf("channels.groups[%q].upstream_key: 外部上游渠道必须填写上游密钥", name)
			}
		}
		switch def.UpstreamType {
		case "", "openai":
		default:
			return fmt.Errorf("channels.groups[%q].upstream_type: %q 非法（openai / 空）", name, def.UpstreamType)
		}
		ch.Groups[name] = def
	}
	for ref, group := range ch.Keys {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("channels.keys: 密钥标识不能为空")
		}
		group = strings.TrimSpace(group)
		if group == "" {
			return fmt.Errorf("channels.keys[%q]: 分组名不能为空（不绑定请删除该键）", ref)
		}
		if _, ok := ch.Groups[group]; !ok {
			return fmt.Errorf("channels.keys[%q]: 分组 %q 未在 channels.groups 中定义", ref, group)
		}
		ch.Keys[ref] = group
	}
	return nil
}

// keyPrefixTTL 密钥 ID→前缀 快照的刷新间隔。
//
// 为什么要快照：前缀写法的绑定需要把请求带来的**密钥 ID** 映射到前缀，而密钥列表
// 存在 apikeys.Store 里。逐请求遍历存储没必要（绑定表本身很少变），缓存足以覆盖
// "新建密钥后改配置"的操作节奏；面板写入的是 ID，走 byID 快路径，根本不碰快照。
const keyPrefixTTL = 30 * time.Second

// channelRouter 渠道绑定的运行期解析器：配置热生效时由 Set 整体替换绑定表。
//
// 为什么不直接在请求路径上读 config：saveConfig 会深合并出**新的** config 对象，
// handler 需要在保存后立即看到新绑定。用一个可替换的内部表 + 指针语义最直接，
// 也避免每请求都做一次 map 深拷贝。
type channelRouter struct {
	mu    sync.Mutex
	store *apikeys.Store

	// 绑定表（Set 时重建，持 mu 读）
	enabled  bool
	byID     map[string]server.ChannelInfo
	byPrefix map[string]server.ChannelInfo
	// hasPrefix 是否存在前缀写法（false 时跳过快照，纯 ID 绑定零额外开销）
	hasPrefix bool

	// last 最近一次 Set 的原样配置（状态透出用：bindings 按它报告）
	last *Channels

	// ID→前缀 惰性快照（仅前缀写法需要）
	id2pfx      map[string]string
	ambiguous   map[string]bool
	refreshedAt time.Time
}

// defByName 按渠道名取定义（渠道页"拉取上游模型"用：拿该渠道的上游地址与密钥）。
//
// 读的是最近一次 Set 的原样配置：与运行期路由同源，避免"面板看到的"和"请求真正走的"
// 因为读了两份配置而不一致。
func (r *channelRouter) defByName(name string) (ChannelDef, bool) {
	if r == nil || name == "" {
		return ChannelDef{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return ChannelDef{}, false
	}
	def, ok := r.last.Groups[name]
	return def, ok
}

func newChannelRouter(ch *Channels, store *apikeys.Store) *channelRouter {
	r := &channelRouter{store: store}
	r.Set(ch)
	return r
}

// Set 用新的 channels 配置替换绑定表（启动与 saveConfig 热生效都走这里）。
// 不 close 任何东西、不阻塞在途请求：旧表的读取者持有的是值拷贝。
func (r *channelRouter) Set(ch *Channels) {
	if r == nil {
		return
	}
	enabled := false
	byID := map[string]server.ChannelInfo{}
	byPrefix := map[string]server.ChannelInfo{}
	hasPrefix := false
	if ch != nil {
		enabled = ch.Enabled
		for ref, group := range ch.Keys {
			def, ok := ch.Groups[group]
			if !ok {
				// normalizeChannels 已拦下；此处防御性跳过（不静默放行成"全池可用"）。
				continue
			}
			info := server.ChannelInfo{
				Group:       group,
				Label:       def.Label,
				Realm:       def.Realm,
				Models:      def.Models,
				Enabled:     def.EnabledOn(),
				UpstreamURL: def.UpstreamURL,
				UpstreamKey: def.UpstreamKey,
			}
			if strings.HasPrefix(ref, apikeys.SecretPrefix) {
				hasPrefix = true
				byPrefix[ref] = info
			} else {
				byID[ref] = info
			}
		}
	}
	r.mu.Lock()
	r.enabled, r.byID, r.byPrefix, r.hasPrefix = enabled, byID, byPrefix, hasPrefix
	r.last = ch
	r.id2pfx = nil // 让下次解析重建快照（绑定表变了，快照可能要重算）
	r.refreshedAt = time.Time{}
	r.mu.Unlock()

	// 预热外部渠道的上游模型表。
	//
	// 请求路径只读缓存（server.UpstreamModelIDsCached），缓存空着的那段时间里，
	// 白名单留空的渠道会判成"没有任何模型"——只有外部上游才有的模型会掉回账号池并报
	// no_healthy_account。启动或配置热生效的瞬间正好落在这个窗口里，所以在这里
	// 主动把缓存捂热，把这个空窗消掉。
	// 后台执行、失败无副作用：它只是让缓存更早填上，不改变任何判定语义。
	primed := map[string]bool{}
	for _, info := range byID {
		if info.IsExternal() && !primed[info.UpstreamURL] {
			primed[info.UpstreamURL] = true
			go func(in server.ChannelInfo) {
				_, _ = server.FetchUpstreamModelIDs(in.UpstreamURL, in.UpstreamKey, in.Label,
					server.UpstreamModelTimeoutInteractive)
			}(info)
		}
	}
	for _, info := range byPrefix {
		if info.IsExternal() && !primed[info.UpstreamURL] {
			primed[info.UpstreamURL] = true
			go func(in server.ChannelInfo) {
				_, _ = server.FetchUpstreamModelIDs(in.UpstreamURL, in.UpstreamKey, in.Label,
					server.UpstreamModelTimeoutInteractive)
			}(info)
		}
	}
}

// Resolve 实现 server.ChannelResolver。id 为请求命中的调用密钥 ID。
func (r *channelRouter) Resolve(id string) (server.ChannelInfo, bool) {
	if r == nil || id == "" {
		return server.ChannelInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return server.ChannelInfo{}, false
	}
	if info, ok := r.byID[id]; ok {
		return info, true // 面板写入的 ID 绑定：命中即返回，零额外开销
	}
	if !r.hasPrefix || r.store == nil {
		return server.ChannelInfo{}, false
	}
	prefix := r.prefixOfLocked(id)
	if prefix == "" {
		return server.ChannelInfo{}, false
	}
	// 前缀撞车（多个密钥同一前缀）：按未绑定处理。歧义绑定比不绑定更危险，
	// 宁可让请求走默认口径（面板 bindings 侧会标红该条配置）。
	if r.ambiguousLocked(prefix) {
		return server.ChannelInfo{}, false
	}
	if info, ok := r.byPrefix[prefix]; ok {
		return info, true
	}
	return server.ChannelInfo{}, false
}

// prefixOfLocked 把密钥 ID 映射到前缀（按 keyPrefixTTL 刷新快照；调用方持 mu）。
func (r *channelRouter) prefixOfLocked(id string) string {
	r.refreshLocked()
	return r.id2pfx[id]
}

// ambiguousLocked 报告某前缀是否被多个密钥共享（调用方持 mu）。
func (r *channelRouter) ambiguousLocked(prefix string) bool {
	r.refreshLocked()
	return r.ambiguous[prefix]
}

// refreshLocked 重建 ID→前缀 快照与撞车表（调用方持 mu，TTL 未到直接跳过）。
func (r *channelRouter) refreshLocked() {
	now := time.Now()
	if r.id2pfx != nil && now.Sub(r.refreshedAt) < keyPrefixTTL {
		return
	}
	snapshot := make(map[string]string)
	seen := make(map[string]int)
	for _, k := range r.store.List() {
		snapshot[k.ID] = k.Prefix
		seen[k.Prefix]++
	}
	amb := make(map[string]bool)
	for pfx, n := range seen {
		if n > 1 {
			amb[pfx] = true
		}
	}
	r.id2pfx = snapshot
	r.ambiguous = amb
	r.refreshedAt = now
}

// channelBindingStatus 面板/状态透出用：每条绑定的解析结果（含未命中标注）。
type channelBindingStatus struct {
	Ref     string `json:"ref"`     // 配置里写的 key（ID 或前缀）
	Group   string `json:"group"`   // 绑定到的分组
	Kind    string `json:"kind"`    // "id" | "prefix"
	Matched bool   `json:"matched"` // 是否命中真实存在的调用密钥
	KeyName string `json:"key_name,omitempty"`
	KeyPfx  string `json:"key_prefix,omitempty"`
}

// channelStatus 是 /status 的 channels 段载荷（main.status 组装）。
type channelStatus struct {
	Enabled  bool                       `json:"enabled"`
	Groups   map[string]channelStatusDef `json:"groups"`
	Bindings []channelBindingStatus     `json:"bindings"`
}

// channelStatusDef 渠道定义的状态透出形态：与 ChannelDef 同构，但**上游密钥脱敏**
// （只留长度与尾 4 位）。/status 是运维观测面，不该成为上游密钥的泄露口。
type channelStatusDef struct {
	Label       string   `json:"label"`
	Realm       string   `json:"realm"`
	Models      []string `json:"models"`
	Enabled     bool     `json:"enabled"`
	UpstreamURL string   `json:"upstream_url,omitempty"`
	UpstreamKey string   `json:"upstream_key,omitempty"` // 已脱敏
}

func (d ChannelDef) statusView() channelStatusDef {
	key := ""
	if d.UpstreamKey != "" {
		// 只留"长度 + 尾 4 位"：足以核对填的是不是预期的那把钥匙，又不足以复用。
		key = "***" + d.UpstreamKey[max(0, len(d.UpstreamKey)-4):] + "(len=" + itoa(len(d.UpstreamKey)) + ")"
	}
	return channelStatusDef{
		Label:       d.Label,
		Realm:       d.Realm,
		Models:      d.Models,
		Enabled:     d.EnabledOn(),
		UpstreamURL: d.UpstreamURL,
		UpstreamKey: key,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 8)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// status 快照当前绑定表（持锁拷贝，调用路径是低频状态端点）。
func (r *channelRouter) status() channelStatus {
	if r == nil {
		return channelStatus{Groups: map[string]channelStatusDef{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := channelStatus{Enabled: r.enabled, Groups: map[string]channelStatusDef{}}
	for name, def := range r.lastGroupsLocked() {
		out.Groups[name] = def.statusView()
	}
	out.Bindings = r.bindingsLocked()
	return out
}

// lastGroupsLocked 返回最近一次 Set 的渠道定义（调用方持 mu）。
func (r *channelRouter) lastGroupsLocked() map[string]ChannelDef {
	if r.last == nil {
		return map[string]ChannelDef{}
	}
	return r.last.Groups
}

// bindingsLocked 按最近一次 Set 的配置返回绑定明细（调用方持 mu；
// 未命中的也列出，让运营看见而不是静默失效）。
func (r *channelRouter) bindingsLocked() []channelBindingStatus {
	var refs map[string]string
	if r.last != nil {
		refs = r.last.Keys
	}
	out := make([]channelBindingStatus, 0, len(refs))
	keys := []apikeys.Key(nil)
	if r.store != nil {
		keys = r.store.List()
	}
	for ref, group := range refs {
		st := channelBindingStatus{Ref: ref, Group: group, Kind: "id"}
		if strings.HasPrefix(ref, apikeys.SecretPrefix) {
			st.Kind = "prefix"
		}
		matches := 0
		for _, k := range keys {
			if k.ID == ref || k.Prefix == ref {
				matches++
				st.Matched = true
				st.KeyName = k.Name
				st.KeyPfx = k.Prefix
			}
		}
		if matches > 1 {
			// 前缀撞车：解析期按未绑定处理（歧义绑定比不绑定更危险）。
			st.Matched = false
			st.KeyName = ""
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}
