package durablestate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// EnvelopeCryptoVersion identifies the AES-256-GCM blob framing.
	EnvelopeCryptoVersion byte = 1
	stateMasterKeyEnv          = "CLIPROXY_STATE_MASTER_KEY"
)

// EncryptedBlob is ciphertext plus key metadata. Nonce is embedded in Blob.
type EncryptedBlob struct {
	KeyID   string
	Version byte
	Blob    []byte // version(1) || nonce(12) || ciphertext+tag
}

// MasterKeyProvider resolves the active state encryption key.
type MasterKeyProvider interface {
	CurrentKeyID() string
	KeyByID(keyID string) ([]byte, error)
}

// EnvMasterKeyProvider loads CLIPROXY_STATE_MASTER_KEY (32-byte raw or 64-hex).
type EnvMasterKeyProvider struct {
	KeyID string
	Key   []byte
}

// LoadEnvMasterKeyProvider reads CLIPROXY_STATE_MASTER_KEY.
// When required is true, a missing/invalid key is an error.
func LoadEnvMasterKeyProvider(required bool) (*EnvMasterKeyProvider, error) {
	raw := strings.TrimSpace(os.Getenv(stateMasterKeyEnv))
	if raw == "" {
		if required {
			return nil, errors.New("CLIPROXY_STATE_MASTER_KEY is required but missing")
		}
		return nil, nil
	}
	key, err := parseMasterKey(raw)
	if err != nil {
		return nil, err
	}
	return &EnvMasterKeyProvider{KeyID: "env-v1", Key: key}, nil
}

func parseMasterKey(raw string) ([]byte, error) {
	if len(raw) == 64 {
		out, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("CLIPROXY_STATE_MASTER_KEY hex decode: %w", err)
		}
		if len(out) != 32 {
			return nil, errors.New("CLIPROXY_STATE_MASTER_KEY hex must decode to 32 bytes")
		}
		return out, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, errors.New("CLIPROXY_STATE_MASTER_KEY must be 32 raw bytes or 64 hex chars")
}

func (p *EnvMasterKeyProvider) CurrentKeyID() string {
	if p == nil {
		return ""
	}
	return p.KeyID
}

func (p *EnvMasterKeyProvider) KeyByID(keyID string) ([]byte, error) {
	if p == nil || len(p.Key) != 32 {
		return nil, errors.New("state master key unavailable")
	}
	if keyID != "" && keyID != p.KeyID {
		return nil, fmt.Errorf("unknown state key id %q", keyID)
	}
	out := make([]byte, 32)
	copy(out, p.Key)
	return out, nil
}

// BuildAAD constructs authenticated associated data for envelope blobs.
func BuildAAD(tenantID, conversationID, invocationID string, schemaVersion uint32) []byte {
	buf := make([]byte, 0, 64+len(tenantID)+len(conversationID)+len(invocationID))
	buf = append(buf, []byte("cliproxy-state-aad-v1\x00")...)
	var ver [4]byte
	binary.BigEndian.PutUint32(ver[:], schemaVersion)
	buf = append(buf, ver[:]...)
	buf = append(buf, []byte(tenantID)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(conversationID)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(invocationID)...)
	return buf
}

// EncryptEnvelope seals plaintext with AES-256-GCM.
// Callers should compress before encryption when storing large envelopes.
func EncryptEnvelope(provider MasterKeyProvider, plaintext, aad []byte) (*EncryptedBlob, error) {
	if provider == nil {
		return nil, errors.New("state master key provider required")
	}
	keyID := provider.CurrentKeyID()
	key, err := provider.KeyByID(keyID)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("state master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, aad)
	blob := make([]byte, 1+len(nonce)+len(sealed))
	blob[0] = EnvelopeCryptoVersion
	copy(blob[1:], nonce)
	copy(blob[1+len(nonce):], sealed)
	return &EncryptedBlob{KeyID: keyID, Version: EnvelopeCryptoVersion, Blob: blob}, nil
}

// DecryptEnvelope opens an EncryptedBlob and authenticates AAD.
func DecryptEnvelope(provider MasterKeyProvider, enc *EncryptedBlob, aad []byte) ([]byte, error) {
	if provider == nil || enc == nil {
		return nil, errors.New("encrypted blob and key provider required")
	}
	if rot, ok := provider.(*RotatingMasterKeyProvider); ok {
		return DecryptEnvelopeDual(rot, enc, aad)
	}
	if len(enc.Blob) < 1+12+16 {
		return nil, errors.New("encrypted blob truncated")
	}
	if enc.Blob[0] != EnvelopeCryptoVersion {
		return nil, fmt.Errorf("unsupported envelope crypto version %d", enc.Blob[0])
	}
	key, err := provider.KeyByID(enc.KeyID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(enc.Blob) < 1+nonceSize+gcm.Overhead() {
		return nil, errors.New("encrypted blob truncated")
	}
	nonce := enc.Blob[1 : 1+nonceSize]
	ciphertext := enc.Blob[1+nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("envelope authentication failed")
	}
	return plain, nil
}
