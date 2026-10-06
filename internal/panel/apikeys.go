package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": p.cfg.APIKeys.List()})
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
