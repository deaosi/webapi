// Package qwenwork 对接「千问办公」（QwenWork，gateway.qwenwork.cn）。
//
// 与 OpenAI 兼容渠道的本质差别：它没有静态 API key，认证是**设备授权拿到的
// access/refresh token**，而且每个请求都要带一套由官方桌面客户端（qoderwork）
// 生成的签名头（cosy-*）。签名逻辑来自对官方客户端的协议还原：
//
//	payload   = {"cosyVersion","ideVersion","info","requestId","version"}（键排序、紧凑 JSON）
//	payloadB64= base64(payload)
//	sig       = md5(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + path)
//	auth      = "Bearer COSY." + payloadB64 + "." + sig
//
// 其中 info 是「身份 JSON」用临时 AES 密钥加密后的 base64，cosyKey 是同一个临时
// 密钥用上游 RSA 公钥加密后的 base64。两者都在一次「会话」里固定，只在 token
// 变化时重建（见 sessionFor）。
package qwenwork

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

// 上游协议常量（与官方客户端 1.0.5 / cosy 1.1.18 对齐）。上游升级需要同步改这里。
const (
	// BaseURL 上游默认地址（面板里可覆盖，便于将来换域名或走代理）。
	BaseURL        = "https://gateway.qwenwork.cn"
	userAgent      = "qoderwork/1.0.5"
	clientID       = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	redirectURI    = "qwenwork-cn://"
	cosyVersion    = "1.1.18"
	ideVersion     = "1.0.5"
	releaseVersion = "1.0.5-26090901"
	build          = "26090901"
	clientType     = "6"
	businessProd   = "qoder_work"
	businessType   = "agent"
	scene          = "qwork"
	machineOS      = "x86_64_win32"

	// ChatPath 对话（SSE）端点。
	ChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	// ModelListPath 模型目录端点。
	ModelListPath = "/algo/api/v2/model/list"
	// UserInfoPath 用户信息端点。
	UserInfoPath = "/api/v1/userinfo"
	// ContextPath 账号上下文（套餐/配额）端点。
	ContextPath = "/api/v1/adapter/user/account-context?include=user,plan,quota"
	// PollPath 设备授权轮询端点。
	PollPath = "/api/v1/deviceToken/poll"
	// RefreshPath 刷新令牌端点。
	RefreshPath = "/api/v1/deviceToken/refresh"
	// DeviceAuthURL 设备授权页（面板把它显示成链接给运维点）。
	DeviceAuthURL = BaseURL + "/device/selectAccounts"
)

// rsaN 上游客户端内置的 RSA 公钥模数（128 字节 / 1024 位）。
var rsaN, _ = new(big.Int).SetString(strings.Join([]string{
	"c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4",
	"a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a",
	"1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24b",
	"e1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf",
}, ""), 16)

var rsaE = 65537

// Account 一个千问办公账号（device 授权得到的凭据）。
type Account struct {
	// UID 上游用户 id（cosy-user 头与账号去重键）。
	UID string `json:"uid"`
	// Nickname 展示名（面板列表用）。
	Nickname string `json:"nickname,omitempty"`
	// Email 邮箱（面板列表用）。
	Email string `json:"email,omitempty"`
	// AccessToken 访问令牌（签名与 bearer 都用它）。
	AccessToken string `json:"access_token"`
	// RefreshToken 刷新令牌（401/403 时自动换新）。
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt 过期时间（unix 秒）。
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// Expired 令牌是否已过期（留 60 秒余量）。
func (a Account) Expired(now time.Time) bool {
	return a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix()+60
}

// Label 面板/日志展示名：昵称优先，退回 uid 后 8 位。
func (a Account) Label() string {
	if n := strings.TrimSpace(a.Nickname); n != "" {
		return n
	}
	if len(a.UID) > 8 {
		return "u" + a.UID[len(a.UID)-8:]
	}
	return "u" + a.UID
}

// session 一次设备会话的固定材料（machineID/cosyKey/info 等）。
type session struct {
	machineID   string
	tempKey     string
	cosyKey     string
	info        string
	accessToken string
}

var (
	sessMu    sync.Mutex
	sessCache = map[string]session{}
)

func sessionKey(acc Account) string {
	if acc.UID != "" {
		return acc.UID
	}
	k := acc.AccessToken
	if len(k) > 16 {
		k = k[:16]
	}
	return k
}

// sessionFor 取（或建立）该账号的签名会话。token 变化时重建——info 里封了
// security_oauth_token，沿用旧会话会签名通过但身份不匹配。
func sessionFor(acc Account) (session, error) {
	key := sessionKey(acc)
	sessMu.Lock()
	defer sessMu.Unlock()
	if s, ok := sessCache[key]; ok && s.accessToken == acc.AccessToken {
		return s, nil
	}
	s, err := newSession(acc)
	if err != nil {
		return session{}, err
	}
	if len(sessCache) > 128 {
		sessCache = map[string]session{}
	}
	sessCache[key] = s
	return s, nil
}

// InvalidateSession 丢掉该账号的会话（token 刷新后必须调用）。
func InvalidateSession(acc Account) {
	sessMu.Lock()
	delete(sessCache, sessionKey(acc))
	sessMu.Unlock()
}

func newSession(acc Account) (session, error) {
	if strings.TrimSpace(acc.AccessToken) == "" {
		return session{}, errors.New("qwenwork: 账号缺少 access_token")
	}
	machineID, err := uuidV4()
	if err != nil {
		return session{}, err
	}
	seed1, err := uuidV4()
	if err != nil {
		return session{}, err
	}
	seed2, err := uuidV4()
	if err != nil {
		return session{}, err
	}
	seed := seed1 + seed2
	if len(seed) > 50 {
		seed = seed[:50]
	}
	tk, err := uuidV4()
	if err != nil {
		return session{}, err
	}
	tempKey := strings.ReplaceAll(tk, "-", "")
	if len(tempKey) > 16 {
		tempKey = tempKey[:16]
	}
	cosyKey, err := rsaEncrypt([]byte(tempKey))
	if err != nil {
		return session{}, err
	}
	identity := compactJSON(map[string]string{
		"uid":                  acc.UID,
		"aid":                  acc.UID,
		"name":                 acc.Nickname,
		"email":                acc.Email,
		"security_oauth_token": acc.AccessToken,
	})
	enc, err := aesCBCEncrypt([]byte(tempKey), []byte(identity))
	if err != nil {
		return session{}, err
	}
	return session{
		machineID:   machineID,
		tempKey:     tempKey,
		cosyKey:     cosyKey,
		info:        base64.StdEncoding.EncodeToString(enc),
		accessToken: acc.AccessToken,
	}, nil
}

// SignHeaders 生成一次请求的全部签名头。body 必须与实际发送的字节完全一致。
func SignHeaders(acc Account, body []byte, rawURL, modelKey, accept string) (map[string]string, error) {
	sess, err := sessionFor(acc)
	if err != nil {
		return nil, err
	}
	reqID, err := uuidV4()
	if err != nil {
		return nil, err
	}
	payload := compactJSON(map[string]string{
		"cosyVersion": cosyVersion,
		"ideVersion":  ideVersion,
		"info":        sess.info,
		"requestId":   reqID,
		"version":     "v1",
	})
	payloadB64 := base64.StdEncoding.EncodeToString([]byte(payload))
	date := fmt.Sprintf("%d", time.Now().Unix())
	sig := md5Hex(payloadB64 + "\n" + sess.cosyKey + "\n" + date + "\n" + string(body) + "\n" + pathSig(rawURL))

	return map[string]string{
		"accept":                     orDefault(accept, "text/event-stream"),
		"content-type":               "application/json",
		"user-agent":                 userAgent,
		"x-request-id":               reqID,
		"x-qwenwork-version":         ideVersion,
		"x-qwenwork-release-version": releaseVersion,
		"x-qwenwork-build":           build,
		"x-qwenwork-platform":        "win32",
		"x-qwenwork-arch":            "x64",
		"x-qwenwork-channel":         "stable",
		"cosy-version":               cosyVersion,
		"cosy-clienttype":            clientType,
		"cosy-business-product":      businessProd,
		"cosy-business-type":         businessType,
		"cosy-scene":                 scene,
		"cosy-machineos":             machineOS,
		"login-version":              "v2",
		"authorization":              "Bearer COSY." + payloadB64 + "." + sig,
		"cosy-key":                   sess.cosyKey,
		"cosy-user":                  acc.UID,
		"cosy-date":                  date,
		"cosy-machineid":             sess.machineID,
		"accept-encoding":            "identity",
		"connection":                 "keep-alive",
		"cache-control":              "no-cache",
		"x-model-key":                modelKey,
		"x-model-source":             "system",
	}, nil
}

// pathSig 签名里的路径：去掉 /algo 前缀（官方客户端就是这么算的）。
func pathSig(rawURL string) string {
	p := rawURL
	if i := strings.Index(p, "://"); i >= 0 {
		p = p[i+3:]
		if j := strings.IndexByte(p, '/'); j >= 0 {
			p = p[j:]
		} else {
			p = "/"
		}
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.TrimPrefix(p, "/algo")
}

// compactJSON 键排序、无空格的紧凑 JSON（签名要求字节级可复现）。
func compactJSON(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// aesCBCEncrypt PKCS#7 + CBC，IV 取密钥本身（官方客户端如此，非随机 IV）。
func aesCBCEncrypt(key, plain []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("qwenwork: AES 密钥长度 %d，需 16", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := 16 - len(plain)%16
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, buf)
	return out, nil
}

// rsaEncrypt PKCS#1 v1.5 加密后 base64。
func rsaEncrypt(data []byte) (string, error) {
	pub := &rsa.PublicKey{N: rsaN, E: rsaE}
	out, err := rsa.EncryptPKCS1v15(rand.Reader, pub, data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
