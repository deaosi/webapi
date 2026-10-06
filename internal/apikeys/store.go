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

type storedKey struct {
	Key
	SHA256 string `json:"sha256"`
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

// VerifyBearer checks an active client key. An empty store never grants access.
func (s *Store) VerifyBearer(r *http.Request) bool {
	if s == nil || r == nil {
		return false
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer "+secretPrefix) {
		return false
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if len(token) != len(secretPrefix)+43 {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	matched := 0
	for _, key := range s.keys {
		expected, _ := hex.DecodeString(key.SHA256)
		match := subtle.ConstantTimeCompare(digest[:], expected)
		if key.Enabled {
			matched |= match
		}
	}
	return matched == 1
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
