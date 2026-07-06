package secretdlp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const (
	storeMemory = "memory"
	storeFile   = "file"
)

type storedMapping struct {
	Placeholder string
	Secret      []byte
	SessionID   string
	ClientID    string
	ExpiresAt   time.Time
}

type mappingStore interface {
	Put(ctx context.Context, mapping storedMapping) error
	Get(ctx context.Context, placeholder string, clientID string, now time.Time) ([]byte, bool, error)
	CleanupExpired(ctx context.Context, now time.Time) error
	Close() error
}

func newMappingStore(cfg Config) (mappingStore, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Store)) {
	case "", storeMemory:
		return newMemoryMappingStore(), nil
	case storeFile:
		return newFileMappingStore(cfg)
	default:
		return nil, fmt.Errorf("unsupported secret dlp store %q", cfg.Store)
	}
}

type memoryMappingStore struct {
	mu       sync.RWMutex
	mappings map[string]storedMapping
}

func newMemoryMappingStore() *memoryMappingStore {
	return &memoryMappingStore{mappings: make(map[string]storedMapping)}
}

func (s *memoryMappingStore) Put(ctx context.Context, mapping storedMapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mapping.Placeholder == "" || len(mapping.Secret) == 0 {
		return nil
	}
	mapping.Secret = cloneBytes(mapping.Secret)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.mappings[mapping.Placeholder] = mapping
	return nil
}

func (s *memoryMappingStore) Get(ctx context.Context, placeholder string, clientID string, now time.Time) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if placeholder == "" {
		return nil, false, nil
	}

	s.mu.RLock()
	mapping, ok := s.mappings[placeholder]
	s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !mapping.ExpiresAt.IsZero() && now.After(mapping.ExpiresAt) {
		s.mu.Lock()
		delete(s.mappings, placeholder)
		s.mu.Unlock()
		return nil, false, nil
	}
	if clientID == "" || mapping.ClientID == "" || mapping.ClientID != clientID {
		return nil, false, nil
	}
	return cloneBytes(mapping.Secret), true, nil
}

func (s *memoryMappingStore) CleanupExpired(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for placeholder, mapping := range s.mappings {
		if !mapping.ExpiresAt.IsZero() && now.After(mapping.ExpiresAt) {
			delete(s.mappings, placeholder)
		}
	}
	return nil
}

func (s *memoryMappingStore) Close() error {
	return nil
}

type fileMappingStore struct {
	dir     string
	aead    cipher.AEAD
	nameKey []byte
}

type fileMappingRecord struct {
	Version         int    `json:"version"`
	SessionID       string `json:"session_id"`
	ClientID        string `json:"client_id"`
	ExpiresAtUnixNS int64  `json:"expires_at_unix_ns"`
	Nonce           string `json:"nonce"`
	Ciphertext      string `json:"ciphertext"`
}

func newFileMappingStore(cfg Config) (*fileMappingStore, error) {
	if strings.TrimSpace(cfg.FileDir) == "" {
		return nil, fmt.Errorf("SECRET_DLP_FILE_DIR is required when SECRET_DLP_STORE=file and RAILWAY_VOLUME_MOUNT_PATH is unavailable")
	}
	if len(cfg.MasterKey) == 0 || cfg.MasterKeyGenerated {
		return nil, fmt.Errorf("SECRET_DLP_MASTER_KEY must be set to use SECRET_DLP_STORE=file")
	}

	block, err := aes.NewCipher(deriveStoreKey(cfg.MasterKey, "cliproxy-secret-dlp-file-encryption-v1"))
	if err != nil {
		return nil, fmt.Errorf("create secret dlp file cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create secret dlp file aead: %w", err)
	}

	dir := filepath.Clean(cfg.FileDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secret dlp file store directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod secret dlp file store directory: %w", err)
	}

	return &fileMappingStore{
		dir:     dir,
		aead:    aead,
		nameKey: deriveStoreKey(cfg.MasterKey, "cliproxy-secret-dlp-file-name-v1"),
	}, nil
}

func (s *fileMappingStore) Put(ctx context.Context, mapping storedMapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mapping.Placeholder == "" || len(mapping.Secret) == 0 {
		return nil
	}

	nonce := randomBytes(s.aead.NonceSize())
	ciphertext := s.aead.Seal(nil, nonce, mapping.Secret, []byte(mapping.Placeholder))
	record := fileMappingRecord{
		Version:         1,
		SessionID:       mapping.SessionID,
		ClientID:        mapping.ClientID,
		ExpiresAtUnixNS: mapping.ExpiresAt.UnixNano(),
		Nonce:           base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext:      base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal secret dlp file mapping: %w", err)
	}

	if err := util.AtomicWriteFile(s.pathForPlaceholder(mapping.Placeholder), data, 0o600); err != nil {
		return fmt.Errorf("write secret dlp file mapping: %w", err)
	}
	return nil
}

func (s *fileMappingStore) Get(ctx context.Context, placeholder string, clientID string, now time.Time) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if placeholder == "" {
		return nil, false, nil
	}

	path := s.pathForPlaceholder(placeholder)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read secret dlp file mapping: %w", err)
	}

	var record fileMappingRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, false, fmt.Errorf("decode secret dlp file mapping: %w", err)
	}
	if record.Version != 1 {
		return nil, false, fmt.Errorf("unsupported secret dlp file mapping version %d", record.Version)
	}
	if record.ExpiresAtUnixNS > 0 && now.After(time.Unix(0, record.ExpiresAtUnixNS)) {
		_ = os.Remove(path)
		return nil, false, nil
	}
	if clientID == "" || record.ClientID == "" || record.ClientID != clientID {
		return nil, false, nil
	}

	nonce, err := base64.RawURLEncoding.DecodeString(record.Nonce)
	if err != nil {
		return nil, false, fmt.Errorf("decode secret dlp file mapping nonce: %w", err)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(record.Ciphertext)
	if err != nil {
		return nil, false, fmt.Errorf("decode secret dlp file mapping ciphertext: %w", err)
	}
	secret, err := s.aead.Open(nil, nonce, ciphertext, []byte(placeholder))
	if err != nil {
		return nil, false, fmt.Errorf("decrypt secret dlp file mapping: %w", err)
	}
	return secret, true, nil
}

func (s *fileMappingStore) CleanupExpired(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := filepath.WalkDir(s.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record fileMappingRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil
		}
		if record.ExpiresAtUnixNS > 0 && now.After(time.Unix(0, record.ExpiresAtUnixNS)) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *fileMappingStore) Close() error {
	return nil
}

func (s *fileMappingStore) pathForPlaceholder(placeholder string) string {
	sum := hmacSHA256(s.nameKey, []byte(placeholder))
	encoded := base64.RawURLEncoding.EncodeToString(sum)
	return filepath.Join(s.dir, encoded[:2], encoded+".json")
}

func deriveStoreKey(masterKey []byte, domain string) []byte {
	return hmacSHA256(masterKey, []byte(domain))
}

func hmacSHA256(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func placeholderLogID(placeholder string) string {
	sum := sha256.Sum256([]byte(placeholder))
	encoded := base64.RawURLEncoding.EncodeToString(sum[:])
	if len(encoded) > 12 {
		return encoded[:12]
	}
	return encoded
}
