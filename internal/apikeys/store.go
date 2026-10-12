// Package apikeys stores revocable API client keys separately from the panel's
// administrator key. Plaintext keys are returned only when they are created.
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNotFound    = errors.New("API key not found")
	ErrInvalidName = errors.New("API key name must contain 1 to 80 characters and no control characters")
)

const (
	secretPrefix = "sk-wb2a-"
	// SecretPrefix 是调用密钥的明文前缀（导出：渠道绑定要按"ID 还是前缀"分流写法）。
	SecretPrefix = secretPrefix
	maxStoreSize = 16 << 20
)

// Key is public metadata. It never contains the plaintext key or its digest.
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Usage 是单个调用密钥的调用统计，供面板「API 密钥」页展示。
//
// 与 Key 分成两层而不是塞进 Key：Key 的字段集合是面板元数据契约（恰好 5 个字段，
// 前端与测试都按此校验），调用统计是只增不改的观测数据，独立一层就不必动既有契约。
type Usage struct {
	Requests     int64      `json:"requests"`                // 聊天调用尝试次数（失败也计入，重试放大可见）
	Errors       int64      `json:"errors,omitempty"`        // 未取到上游 usage 的尝试次数（传输错误 / >=400 / 解析失败）
	PromptTokens int64      `json:"prompt_tokens,omitempty"` // 累计输入 token
	OutputTokens int64      `json:"output_tokens,omitempty"` // 累计输出 token
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`  // 最近一次调用时刻（含 /v1/models 这类无 token 调用）
	LastModel    string     `json:"last_model,omitempty"`    // 最近一次调用使用的模型
}

type storedKey struct {
	Key
	SHA256 string `json:"sha256"`
	Usage  Usage  `json:"usage"`
}

type diskStore struct {
	Version int         `json:"version"`
	Keys    []storedKey `json:"keys"`
}

// Store serializes mutations and publishes them only after an atomic save.
type Store struct {
	mu   sync.RWMutex
	path string
	keys []storedKey
}

// New loads an existing store. A missing file starts empty, while invalid files
// return an error rather than silently discarding previously issued keys.
func New(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("API key store path is empty")
	}
	s := &Store{path: path, keys: make([]storedKey, 0)}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open API key store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStoreSize {
		return nil, errors.New("API key store is not a regular file or exceeds size limit")
	}
	var disk diskStore
	dec := json.NewDecoder(io.LimitReader(f, maxStoreSize+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&disk); err != nil {
		return nil, fmt.Errorf("decode API key store: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("API key store contains trailing data")
	}
	if disk.Version != 1 || disk.Keys == nil {
		return nil, errors.New("invalid API key store format")
	}
	ids := make(map[string]bool)
	digests := make(map[string]bool)
	for _, key := range disk.Keys {
		name, err := validName(key.Name)
		id, idErr := hex.DecodeString(key.ID)
		digest, digestErr := hex.DecodeString(key.SHA256)
		if err != nil || name != key.Name || idErr != nil || len(id) != 16 ||
			digestErr != nil || len(digest) != sha256.Size || key.CreatedAt.IsZero() ||
			!strings.HasPrefix(key.Prefix, secretPrefix) || len(key.Prefix) != 16 ||
			ids[key.ID] || digests[key.SHA256] {
			return nil, errors.New("invalid API key store entry")
		}
		ids[key.ID] = true
		digests[key.SHA256] = true
	}
	s.keys = disk.Keys
	return s, nil
}

// List returns a metadata snapshot in creation order.
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]Key, 0, len(s.keys))
	for _, key := range s.keys {
		keys = append(keys, key.Key)
	}
	return keys
}

// Create returns the new metadata and its one-time plaintext secret.
func (s *Store) Create(name string) (Key, string, error) {
	name, err := validName(name)
	if err != nil {
		return Key{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entropy [32]byte
	var id [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Key{}, "", fmt.Errorf("generate API key: %w", err)
	}
	if _, err := rand.Read(id[:]); err != nil {
		return Key{}, "", fmt.Errorf("generate API key ID: %w", err)
	}
	secret := secretPrefix + base64.RawURLEncoding.EncodeToString(entropy[:])
	digest := sha256.Sum256([]byte(secret))
	key := Key{
		ID:        hex.EncodeToString(id[:]),
		Name:      name,
		Prefix:    secret[:16],
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	next := append(append([]storedKey{}, s.keys...), storedKey{Key: key, SHA256: hex.EncodeToString(digest[:])})
	if err := s.save(next); err != nil {
		return Key{}, "", err
	}
	s.keys = next
	return key, secret, nil
}

// Update changes only the supplied fields. A disabled key fails verification as
// soon as this call succeeds.
func (s *Store) Update(id string, name *string, enabled *bool) (Key, error) {
	var normalized string
	if name != nil {
		var err error
		normalized, err = validName(*name)
		if err != nil {
			return Key{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, key := range s.keys {
		if key.ID != id {
			continue
		}
		next := append([]storedKey{}, s.keys...)
		if name != nil {
			next[i].Name = normalized
		}
		if enabled != nil {
			next[i].Enabled = *enabled
		}
		if err := s.save(next); err != nil {
			return Key{}, err
		}
		s.keys = next
		return next[i].Key, nil
	}
	return Key{}, ErrNotFound
}

// Delete revokes a key and persists its removal.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, key := range s.keys {
		if key.ID == id {
			next := make([]storedKey, 0, len(s.keys)-1)
			next = append(next, s.keys[:i]...)
			next = append(next, s.keys[i+1:]...)
			if err := s.save(next); err != nil {
				return err
			}
			s.keys = next
			return nil
		}
	}
	return ErrNotFound
}

// MatchBearer 校验一个启用的调用密钥，并返回命中的密钥 ID。
//
// 校验语义与旧的布尔判定完全一致：遍历全部密钥做常量时间比较、不做提前返回，
// 禁用的密钥不参与匹配；命中 ID 只在唯一匹配且启用时给出（空库恒不授权）。
func (s *Store) MatchBearer(r *http.Request) (string, bool) {
	if s == nil || r == nil {
		return "", false
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer "+secretPrefix) {
		return "", false
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if len(token) != len(secretPrefix)+43 {
		return "", false
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	matched := 0
	matchedID := ""
	for _, key := range s.keys {
		expected, _ := hex.DecodeString(key.SHA256)
		match := subtle.ConstantTimeCompare(digest[:], expected)
		if key.Enabled {
			matched |= match
			if match == 1 {
				matchedID = key.ID
			}
		}
	}
	if matched != 1 {
		return "", false
	}
	return matchedID, true
}

// VerifyBearer 只做布尔判定（既有调用方语义不变）。An empty store never grants access.
func (s *Store) VerifyBearer(r *http.Request) bool {
	_, ok := s.MatchBearer(r)
	return ok
}

// RecordUsage 累加一次聊天调用尝试的统计（成功与失败都计入，与用量视图同口径）。
//
// 落盘策略：随密钥存储文件同步原子写。单文件只有几 KB、面板用途下请求频率低，
// 换来的是「重启不丢计数、最近调用时间即刻准确」，无需额外 goroutine 或定时器。
// 落盘失败只记日志、不回滚内存计数（统计不影响鉴权，宁可少写一次盘也不能卡请求）。
func (s *Store) RecordUsage(id, model string, promptTokens, outputTokens int64, ok bool) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		next := append([]storedKey{}, s.keys...)
		used := next[i].Usage
		used.Requests++
		if !ok {
			used.Errors++
		}
		if promptTokens > 0 {
			used.PromptTokens += promptTokens
		}
		if outputTokens > 0 {
			used.OutputTokens += outputTokens
		}
		now := time.Now().UTC()
		used.LastUsedAt = &now
		if model != "" {
			used.LastModel = model
		}
		next[i].Usage = used
		if err := s.save(next); err != nil {
			log.Printf("apikeys: 调用统计落盘失败 id=%s: %v", id, err)
		}
		s.keys = next
		return
	}
}

// TouchUsage 只刷新「最近调用」时刻与模型，不增加调用次数。
// 用于 /v1/models 这类不产生 token 的调用：它在界面上确实是"最近用过这个密钥"，
// 但不该把调用次数抬上去。
func (s *Store) TouchUsage(id, model string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		next := append([]storedKey{}, s.keys...)
		now := time.Now().UTC()
		next[i].Usage.LastUsedAt = &now
		if model != "" {
			next[i].Usage.LastModel = model
		}
		if err := s.save(next); err != nil {
			log.Printf("apikeys: 调用时间落盘失败 id=%s: %v", id, err)
		}
		s.keys = next
		return
	}
}

// UsageMap 返回 id → 调用统计的快照（面板列表接口用；调用方拿到的是副本）。
func (s *Store) UsageMap() map[string]Usage {
	if s == nil {
		return map[string]Usage{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	usage := make(map[string]Usage, len(s.keys))
	for _, key := range s.keys {
		entry := key.Usage
		if key.Usage.LastUsedAt != nil {
			at := *key.Usage.LastUsedAt
			entry.LastUsedAt = &at
		}
		usage[key.ID] = entry
	}
	return usage
}

func validName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", ErrInvalidName
	}
	// Reject controls before trimming so trailing newlines cannot be hidden.
	for _, char := range name {
		if unicode.IsControl(char) {
			return "", ErrInvalidName
		}
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 80 {
		return "", ErrInvalidName
	}
	return name, nil
}

func (s *Store) save(keys []storedKey) error {
	raw, err := json.MarshalIndent(diskStore{Version: 1, Keys: keys}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode API key store: %w", err)
	}
	if len(raw)+1 > maxStoreSize {
		return errors.New("API key store exceeds size limit")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create API key store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".api-keys-*")
	if err != nil {
		return fmt.Errorf("create API key store temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict API key store permissions: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write API key store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync API key store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close API key store: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace API key store: %w", err)
	}
	return nil
}
