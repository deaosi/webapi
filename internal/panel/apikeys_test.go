package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

func newAPIKeyTestPanel(t *testing.T) (*Panel, *apikeys.Store) {
	t.Helper()
	store, err := apikeys.New(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{APIKey: "administrator-secret", APIKeys: store}), store
}

func apiKeyRequest(p *Panel, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestAPIKeyRoutesLifecycle(t *testing.T) {
	p, store := newAPIKeyTestPanel(t)
	const admin = "administrator-secret"
	const base = "/panel/api/api_keys"
	listed := apiKeyRequest(p, "GET", base, "", admin)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"keys":[]`) {
		t.Fatalf("empty listing: code=%d body=%s", listed.Code, listed.Body)
	}
	created := apiKeyRequest(p, "POST", base, `{"name":"  SDK 客户端  "}`, admin)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: code=%d body=%s", created.Code, created.Body)
	}
	var creation struct {
		OK     bool        `json:"ok"`
		Key    apikeys.Key `json:"key"`
		Secret string      `json:"secret"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &creation); err != nil {
		t.Fatal(err)
	}
	if !creation.OK || creation.Key.Name != "SDK 客户端" || creation.Secret == "" {
		t.Fatalf("incorrect create result: %#v", creation)
	}
	verify := httptest.NewRequest("GET", "/v1/models", nil)
	verify.Header.Set("Authorization", "Bearer "+creation.Secret)
	if !store.VerifyBearer(verify) {
		t.Fatal("issued key should verify")
	}
	listed = apiKeyRequest(p, "GET", base, "", admin)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), creation.Secret) || strings.Contains(listed.Body.String(), "sha256") || strings.Contains(listed.Body.String(), "secret") {
		t.Fatalf("listing exposed a secret: %s", listed.Body)
	}
	var listing struct {
		OK   bool                         `json:"ok"`
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listing); err != nil || !listing.OK || len(listing.Keys) != 1 || len(listing.Keys[0]) != 5 {
		t.Fatalf("metadata response did not match contract: %#v, %v", listing, err)
	}
	path := base + "/" + creation.Key.ID
	updated := apiKeyRequest(p, "PATCH", path, `{"name":"renamed","enabled":false}`, admin)
	if updated.Code != http.StatusOK || store.VerifyBearer(verify) || store.List()[0].Name != "renamed" {
		t.Fatalf("disable/rename: code=%d body=%s", updated.Code, updated.Body)
	}
	updated = apiKeyRequest(p, "PATCH", path, `{"enabled":true}`, admin)
	if updated.Code != http.StatusOK || !store.VerifyBearer(verify) {
		t.Fatalf("enable: code=%d body=%s", updated.Code, updated.Body)
	}
	deleted := apiKeyRequest(p, "DELETE", path, "", admin)
	if deleted.Code != http.StatusOK || store.VerifyBearer(verify) || len(store.List()) != 0 {
		t.Fatalf("delete: code=%d body=%s", deleted.Code, deleted.Body)
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		missing := apiKeyRequest(p, method, path, `{"enabled":true}`, admin)
		if missing.Code != http.StatusNotFound {
			t.Errorf("%s missing key code=%d", method, missing.Code)
		}
	}
}

func TestAPIClientKeyCannotManagePanel(t *testing.T) {
	p, store := newAPIKeyTestPanel(t)
	key, secret, err := store.Create("client")
	if err != nil {
		t.Fatal(err)
	}
	base := "/panel/api/api_keys"
	for _, route := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base, `{"name":"unauthorized"}`},
		{"PATCH", base + "/" + key.ID, `{"enabled":false}`},
		{"DELETE", base + "/" + key.ID, ""},
		{"GET", "/panel/api/config", ""},
		{"GET", "/panel/api/logs", ""},
	} {
		for _, token := range []string{"", "wrong", secret} {
			rec := apiKeyRequest(p, route.method, route.path, route.body, token)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s accepted unauthorized key: %d", route.method, route.path, rec.Code)
			}
		}
	}
	if len(store.List()) != 1 || !store.List()[0].Enabled {
		t.Fatal("unauthorized client changed stored keys")
	}
}

func TestAPIKeyRoutesRejectInvalidBodies(t *testing.T) {
	p, store := newAPIKeyTestPanel(t)
	key, _, err := store.Create("original")
	if err != nil {
		t.Fatal(err)
	}
	base := "/panel/api/api_keys"
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", base, "", 400},
		{"POST", base, "null", 400},
		{"POST", base, "[]", 400},
		{"POST", base, `{"name":`, 400},
		{"POST", base, `{"name":""}`, 400},
		{"POST", base, `{"name":null}`, 400},
		{"POST", base, `{"name":123}`, 400},
		{"POST", base, `{"name":"ok","secret":"chosen-by-client"}`, 400},
		{"POST", base, `{"name":"ok"} {}`, 400},
		{"POST", base, `{"name":"` + strings.Repeat("x", 5000) + `"}`, 413},
		{"POST", base, `{"name":"line\nname"}`, 400},
		{"PATCH", base + "/" + key.ID, `{}`, 400},
		{"PATCH", base + "/" + key.ID, `{"enabled":"false"}`, 400},
		{"PATCH", base + "/" + key.ID, `{"name":"ok","enabled":null}`, 400},
		{"PATCH", base + "/" + key.ID, `{"created_at":"changed"}`, 400},
	} {
		rec := apiKeyRequest(p, c.method, c.path, c.body, "administrator-secret")
		if rec.Code != c.status {
			t.Errorf("%s body=%q code=%d want=%d response=%s", c.method, c.body, rec.Code, c.status, rec.Body)
		}
	}
	if len(store.List()) != 1 || store.List()[0].Name != "original" || !store.List()[0].Enabled {
		t.Fatal("invalid requests mutated stored keys")
	}
}

func TestAPIKeyRoutesUnavailableWithoutStore(t *testing.T) {
	p := New(Config{APIKey: "administrator-secret"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/api_keys"},
		{"POST", "/panel/api/api_keys"},
		{"PATCH", "/panel/api/api_keys/missing"},
		{"DELETE", "/panel/api/api_keys/missing"},
	} {
		rec := apiKeyRequest(p, c.method, c.path, `{"name":"test"}`, "administrator-secret")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s unavailable store code=%d", c.method, rec.Code)
		}
	}
}
