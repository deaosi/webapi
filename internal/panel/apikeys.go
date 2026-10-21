package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

const maxAPIKeyBody = 4096

func (p *Panel) apiKeysAvailable(w http.ResponseWriter) bool {
	if p.cfg.APIKeys == nil {
		writeErr(w, http.StatusNotImplemented, "API key management is not available")
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func (p *Panel) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	// keys 保持 5 字段元数据契约不变；调用统计并行挂在 usage（id → 统计）下。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"keys":  p.cfg.APIKeys.List(),
		"usage": p.cfg.APIKeys.UsageMap(),
	})
}

func (p *Panel) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeAPIKeyBody(w, r, &body) {
		return
	}
	key, secret, err := p.cfg.APIKeys.Create(body.Name)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "key": key, "secret": secret})
}

func (p *Panel) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	var body struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if !decodeAPIKeyBody(w, r, &body) {
		return
	}
	if body.Name == nil && body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "provide name or enabled")
		return
	}
	key, err := p.cfg.APIKeys.Update(r.PathValue("id"), body.Name, body.Enabled)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

func (p *Panel) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	if err := p.cfg.APIKeys.Delete(r.PathValue("id")); err != nil {
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requireAdminKey 二次确认：请求体必须带当前管理员密钥。
//
// 面板的每个 /panel/api/* 请求本来就带着管理员 Bearer，这里再要一次不是为了
// 补鉴权缺口，而是让"把明文摊在屏幕上"成为一个需要人明确在场的动作：浏览器
// 停在面板上没人管时，顺手一点看不到任何密钥。
//
// 面板未启用鉴权（apiKey()==""）时拒绝：没有可核对的口令，二次确认没有意义。
func (p *Panel) requireAdminKey(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIKeyBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body struct {
		AdminKey string `json:"admin_key"`
	}
	if err := dec.Decode(&body); err != nil {
		writeAPIKeyDecodeError(w, err)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	admin := p.apiKey()
	if admin == "" {
		writeErr(w, http.StatusConflict, "面板未启用管理员密钥，无法查看调用密钥明文")
		return false
	}
	if !httpauth.VerifySecret(body.AdminKey, admin) {
		writeErr(w, http.StatusForbidden, "管理员密钥不正确")
		return false
	}
	return true
}

// revealAPIKey 返回某个调用密钥的明文（需要管理员密钥二次确认）。
func (p *Panel) revealAPIKey(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	if !p.requireAdminKey(w, r) {
		return
	}
	secret, err := p.cfg.APIKeys.Reveal(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, apikeys.ErrSecretUnavailable) {
			// 单独给一个机器码：前端据此把"只能重置"这条路直接摆出来。
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok":    false,
				"code":  "secret_unavailable",
				"error": "该密钥创建于旧版本（明文未保存）或管理员密钥已更换，无法查看；请用「重置」生成新密钥",
			})
			return
		}
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "secret": secret})
}

// rotateAPIKey 给已有调用密钥换一把新明文：旧的立即失效，其余元数据不变。
func (p *Panel) rotateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !p.apiKeysAvailable(w) {
		return
	}
	if !p.requireAdminKey(w, r) {
		return
	}
	key, secret, err := p.cfg.APIKeys.Rotate(r.PathValue("id"))
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key, "secret": secret})
}

func decodeAPIKeyBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIKeyBody)
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		writeAPIKeyDecodeError(w, err)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			writeErr(w, http.StatusBadRequest, "request body must contain one JSON object")
		} else {
			writeAPIKeyDecodeError(w, err)
		}
		return false
	}
	if len(raw) == 0 || raw[0] != '{' {
		writeErr(w, http.StatusBadRequest, "request body must be a JSON object")
		return false
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		writeAPIKeyDecodeError(w, err)
		return false
	}
	for field, value := range fields {
		if field != "name" && field != "enabled" {
			writeErr(w, http.StatusBadRequest, "unknown request field")
			return false
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeErr(w, http.StatusBadRequest, "request fields must not be null")
			return false
		}
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		writeAPIKeyDecodeError(w, err)
		return false
	}
	return true
}

func writeAPIKeyDecodeError(w http.ResponseWriter, err error) {
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	writeErr(w, http.StatusBadRequest, "invalid JSON request body")
}

func writeAPIKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apikeys.ErrNotFound):
		writeErr(w, http.StatusNotFound, "API key not found")
	case errors.Is(err, apikeys.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, apikeys.ErrInvalidName.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "could not save API key changes")
	}
}
