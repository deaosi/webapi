package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
)

func TestManagedAPIKeyScopeAndLifecycle(t *testing.T) {
	dynamicModelsCache.Lock()
	previousIDs, previousFetched, previousFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = previousIDs
		dynamicModelsCache.fetched = previousFetched
		dynamicModelsCache.lastFail = previousFail
		dynamicModelsCache.Unlock()
	})
	store, err := apikeys.New(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, secret, err := store.Create("local client")
	if err != nil {
		t.Fatal(err)
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "upstream-token", ExpiresAt: 9999999999})
	t.Cleanup(p.Close)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	live := livecfg.New(livecfg.Snapshot{APIKey: "admin-secret"})
	pn := panel.New(panel.Config{Pool: p, APIKey: "admin-secret", Live: live, APIKeys: store})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "admin-secret", Live: live, APIKeys: store, Panel: pn})

	request := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`)
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	assertStatus := func(method, path, token string, want int) {
		t.Helper()
		w := request(method, path, token)
		if w.Code != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, want, w.Body)
		}
	}

	assertStatus("POST", "/v1/chat/completions", secret, http.StatusOK)
	// 调用密钥的聊天调用必须在存储里留下统计（面板「API 密钥」页的用量与最近调用时间）。
	called := store.UsageMap()[key.ID]
	if called.Requests != 1 || called.LastUsedAt == nil || called.LastModel == "" {
		t.Fatalf("client key chat call was not counted: %#v", called)
	}
	assertStatus("GET", "/v1/models", secret, http.StatusOK)
	// 模型列表只刷新「最近调用」，不抬高调用次数。
	if listed := store.UsageMap()[key.ID]; listed.Requests != 1 || listed.LastUsedAt == nil {
		t.Fatalf("model listing must only touch last-used: %#v", listed)
	}
	for _, path := range []string{"/status", "/panel/api/config", "/panel/api/api_keys"} {
		assertStatus("GET", path, secret, http.StatusUnauthorized)
	}
	assertStatus("GET", "/panel/api/api_keys", "admin-secret", http.StatusOK)
	assertStatus("GET", "/v1/models", "", http.StatusUnauthorized)
	assertStatus("GET", "/v1/models", "invalid-secret", http.StatusUnauthorized)
	// 鉴权失败的调用不得计入任何密钥的统计。
	if rejected := store.UsageMap()[key.ID]; rejected.Requests != 1 {
		t.Fatalf("unauthorized call leaked into usage: %#v", rejected)
	}

	// A primary-key rotation must preserve client keys and invalidate the old
	// primary key for both API calls and administration.
	live.Store(livecfg.Snapshot{APIKey: "new-admin-secret"})
	assertStatus("GET", "/v1/models", "admin-secret", http.StatusUnauthorized)
	assertStatus("GET", "/v1/models", "new-admin-secret", http.StatusOK)
	assertStatus("GET", "/v1/models", secret, http.StatusOK)
	assertStatus("GET", "/panel/api/api_keys", "new-admin-secret", http.StatusOK)

	enabled := false
	if _, err := store.Update(key.ID, nil, &enabled); err != nil {
		t.Fatal(err)
	}
	assertStatus("POST", "/v1/chat/completions", secret, http.StatusUnauthorized)
	assertStatus("GET", "/v1/models", secret, http.StatusUnauthorized)
	// 停用后即便有人拿着旧密钥打过来，也不许再累加统计。
	if stopped := store.UsageMap()[key.ID]; stopped.Requests != 1 {
		t.Fatalf("disabled key still accumulated usage: %#v", stopped)
	}
	enabled = true
	if _, err := store.Update(key.ID, nil, &enabled); err != nil {
		t.Fatal(err)
	}
	assertStatus("GET", "/v1/models", secret, http.StatusOK)
	if err := store.Delete(key.ID); err != nil {
		t.Fatal(err)
	}
	assertStatus("GET", "/v1/models", secret, http.StatusUnauthorized)
}
