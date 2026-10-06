package apikeys

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func verifyToken(store *Store, token string) bool {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return store.VerifyBearer(req)
}

func TestLifecyclePersistsAndRevokesImmediately(t *testing.T) {
	store, path := testStore(t)
	if keys := store.List(); keys == nil || len(keys) != 0 {
		t.Fatalf("new store must list [], got %#v", keys)
	}
	key, secret, err := store.Create("  客户端一  ")
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "客户端一" || !key.Enabled || key.CreatedAt.IsZero() || len(key.ID) != 32 {
		t.Fatalf("invalid creation metadata: %#v", key)
	}
	if len(secret) != len(secretPrefix)+43 || key.Prefix != secret[:16] || !verifyToken(store, secret) {
		t.Fatal("new secret or verification invalid")
	}
	reloaded, err := New(path)
	if err != nil || !reflect.DeepEqual(reloaded.List(), store.List()) || !verifyToken(reloaded, secret) {
		t.Fatalf("created key did not survive reload: %v", err)
	}
	name := "  新客户端  "
	enabled := false
	updated, err := store.Update(key.ID, &name, &enabled)
	if err != nil || updated.Name != "新客户端" || updated.Enabled || verifyToken(store, secret) {
		t.Fatalf("disable/rename failed: %#v, %v", updated, err)
	}
	reloaded, err = New(path)
	if err != nil || reloaded.List()[0].Enabled || reloaded.List()[0].Name != "新客户端" || verifyToken(reloaded, secret) {
		t.Fatalf("disabled state did not survive reload: %v", err)
	}
	enabled = true
	if _, err := store.Update(key.ID, nil, &enabled); err != nil || !verifyToken(store, secret) {
		t.Fatalf("reenabling failed: %v", err)
	}
	if err := store.Delete(key.ID); err != nil || verifyToken(store, secret) {
		t.Fatalf("delete did not revoke immediately: %v", err)
	}
	reloaded, err = New(path)
	if err != nil || len(reloaded.List()) != 0 || verifyToken(reloaded, secret) {
		t.Fatalf("deletion did not survive reload: %v", err)
	}
	if err := store.Delete(key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second deletion: got %v", err)
	}
	if _, err := store.Update(key.ID, nil, &enabled); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing key: got %v", err)
	}
}

func TestPlaintextOnlyReturnedOnceAndFileContainsDigest(t *testing.T) {
	store, path := testStore(t)
	_, secret, err := store.Create("Integration")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(secret))
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatal("persisted file must contain digest, never plaintext")
	}
	metadata, err := json.Marshal(store.List())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), secret) || strings.Contains(string(metadata), "sha256") || strings.Contains(string(metadata), "secret") {
		t.Fatal("list leaked secret or digest")
	}
	snapshot := store.List()
	snapshot[0].Name = "changed outside store"
	snapshot[0].Enabled = false
	if store.List()[0].Name != "Integration" || !verifyToken(store, secret) {
		t.Fatal("List must return an isolated metadata snapshot")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("store permissions must be 0600: info=%v err=%v", info, err)
		}
	}
}

func TestWriteFailureRollsBackAllMutations(t *testing.T) {
	store, path := testStore(t)
	key, secret, err := store.Create("original")
	if err != nil {
		t.Fatal(err)
	}
	before := store.List()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	// Use a real filesystem failure without depending on platform permission bits.
	store.path = filepath.Join(blocker, "keys.json")
	if _, leakedSecret, err := store.Create("should fail"); err == nil || leakedSecret != "" {
		t.Fatalf("failed Create returned success or a secret: err=%v", err)
	}
	name := "should not change"
	enabled := false
	if _, err := store.Update(key.ID, &name, &enabled); err == nil {
		t.Fatal("Update should fail to persist")
	}
	if err := store.Delete(key.ID); err == nil {
		t.Fatal("Delete should fail to persist")
	}
	if !reflect.DeepEqual(before, store.List()) || !verifyToken(store, secret) {
		t.Fatal("failed persistence changed in-memory state")
	}
	reloaded, err := New(path)
	if err != nil || !reflect.DeepEqual(before, reloaded.List()) || !verifyToken(reloaded, secret) {
		t.Fatalf("failed mutation changed persisted state: %v", err)
	}
}

func TestUsageRecordingAndPersistence(t *testing.T) {
	store, path := testStore(t)
	key, secret, err := store.Create("client")
	if err != nil {
		t.Fatal(err)
	}
	if usage := store.UsageMap(); len(usage) != 1 || usage[key.ID].Requests != 0 || usage[key.ID].LastUsedAt != nil {
		t.Fatalf("new key must start with empty usage: %#v", usage)
	}
	// 未知 ID / 空 ID 不得凭空建条目（防止面板出现幽灵密钥统计）。
	store.RecordUsage("missing", "glm-5.2", 10, 20, true)
	store.RecordUsage("", "glm-5.2", 10, 20, true)
	store.TouchUsage("missing", "glm-5.2")
	if usage := store.UsageMap(); len(usage) != 1 {
		t.Fatalf("unknown ids must not create usage entries: %#v", usage)
	}
	store.RecordUsage(key.ID, "glm-5.2", 120, 30, true)
	store.RecordUsage(key.ID, "glm-5.2", 0, 0, false)
	store.TouchUsage(key.ID, "")
	used := store.UsageMap()[key.ID]
	if used.Requests != 2 || used.Errors != 1 || used.PromptTokens != 120 || used.OutputTokens != 30 {
		t.Fatalf("usage counters wrong: %#v", used)
	}
	if used.LastUsedAt == nil || used.LastModel != "glm-5.2" {
		t.Fatalf("last-used metadata wrong: %#v", used)
	}
	// UsageMap 必须返回副本：改动快照不得影响存储。
	entry := store.UsageMap()[key.ID]
	entry.Requests = 99
	if store.UsageMap()[key.ID].Requests != 2 {
		t.Fatal("UsageMap must return an isolated snapshot")
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reloaded.UsageMap()[key.ID]
	if !ok || persisted.Requests != used.Requests || persisted.Errors != used.Errors ||
		persisted.PromptTokens != used.PromptTokens || persisted.OutputTokens != used.OutputTokens ||
		persisted.LastModel != used.LastModel || persisted.LastUsedAt == nil ||
		!persisted.LastUsedAt.Equal(*used.LastUsedAt) {
		t.Fatalf("usage did not survive reload: got %#v want %#v", persisted, used)
	}
	// 统计层不得动元数据契约（面板列表仍按 5 字段校验），密钥本身照常可用。
	if keys := reloaded.List(); len(keys) != 1 || keys[0].Name != "client" || keys[0].ID != key.ID {
		t.Fatalf("metadata changed by usage recording: %#v", keys)
	}
	if !verifyToken(reloaded, secret) {
		t.Fatal("usage recording broke key verification")
	}
	if err := store.Delete(key.ID); err != nil {
		t.Fatal(err)
	}
	if usage := store.UsageMap(); len(usage) != 0 {
		t.Fatalf("deleted key left usage behind: %#v", usage)
	}
}

func TestNameValidationCountsRunes(t *testing.T) {
	store, _ := testStore(t)
	invalid := []string{"", "   ", "name\n", "\tname", "a\x00b", strings.Repeat("中", 81), string([]byte{0xff})}
	for _, name := range invalid {
		if _, _, err := store.Create(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) error=%v", name, err)
		}
	}
	if len(store.List()) != 0 {
		t.Fatal("invalid names created keys")
	}
	key, _, err := store.Create(strings.Repeat("中", 80))
	if err != nil {
		t.Fatalf("80 Chinese characters should be accepted: %v", err)
	}
	badName := "\rname"
	if _, err := store.Update(key.ID, &badName, nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("invalid Update name: %v", err)
	}
}

func TestInvalidAuthorizationNeverAccepted(t *testing.T) {
	store, _ := testStore(t)
	_, secret, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", secret, "Basic " + secret, "bearer " + secret, "Bearer " + secret + " ", "Bearer " + secret[:len(secret)-1], "Bearer sk-wb2a-" + strings.Repeat("x", 43)} {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", auth)
		if store.VerifyBearer(req) {
			t.Errorf("accepted invalid authorization %q", auth)
		}
	}
	var absent *Store
	if absent.VerifyBearer(nil) || store.VerifyBearer(nil) {
		t.Fatal("nil store/request must deny access")
	}
}

func TestCorruptExistingStoreFailsClosed(t *testing.T) {
	store, path := testStore(t)
	if _, _, err := store.Create("valid"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk diskStore
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	duplicate := disk
	duplicate.Keys = append(append([]storedKey{}, disk.Keys...), disk.Keys[0])
	duplicateRaw, _ := json.Marshal(duplicate)
	invalidRecord := disk
	invalidRecord.Keys = append([]storedKey{}, disk.Keys...)
	invalidRecord.Keys[0].SHA256 = "invalid"
	invalidRaw, _ := json.Marshal(invalidRecord)
	corrupt := []string{"", "not json", "null", "{}", `{"version":2,"keys":[]}`, `{"version":1,"keys":null}`, `{"version":1,"keys":[],"secret":"unexpected"}`, string(raw) + "{}", string(duplicateRaw), string(invalidRaw)}
	for i, content := range corrupt {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "keys.json")
			if err := os.WriteFile(badPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(badPath); err == nil {
				t.Fatalf("corrupt store accepted: %q", content)
			}
		})
	}
	if _, err := New(""); err == nil {
		t.Fatal("empty store path accepted")
	}
	if _, err := New(t.TempDir()); err == nil {
		t.Fatal("directory accepted as store file")
	}
}

func TestConcurrentMutationsKeepAllKeys(t *testing.T) {
	store, path := testStore(t)
	const count = 12
	var wg sync.WaitGroup
	errorsFound := make(chan error, count)
	secrets := make(chan string, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key, secret, err := store.Create(fmt.Sprintf("client %d", i))
			if err != nil {
				errorsFound <- err
				return
			}
			name := fmt.Sprintf("renamed %d", i)
			if _, err := store.Update(key.ID, &name, nil); err != nil {
				errorsFound <- err
				return
			}
			_ = store.List()
			if !verifyToken(store, secret) {
				errorsFound <- errors.New("created key failed verification")
			}
			secrets <- secret
		}(i)
	}
	wg.Wait()
	close(errorsFound)
	close(secrets)
	for err := range errorsFound {
		t.Error(err)
	}
	reloaded, err := New(path)
	if err != nil || len(store.List()) != count || len(reloaded.List()) != count {
		t.Fatalf("concurrent writes lost keys: err=%v memory=%d", err, len(store.List()))
	}
	unique := make(map[string]bool)
	for secret := range secrets {
		if unique[secret] || !verifyToken(reloaded, secret) {
			t.Fatal("keys collided or were not persisted")
		}
		unique[secret] = true
	}
}
