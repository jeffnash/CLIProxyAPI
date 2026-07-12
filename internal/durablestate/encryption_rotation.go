package durablestate

import (
	"fmt"
	"os"
	"strings"
)

const stateMasterKeyPreviousEnv = "CLIPROXY_STATE_MASTER_KEY_PREVIOUS"

// RotatingMasterKeyProvider supports dual-decrypt during key rotation.
// New writes always use Current; decrypt accepts Current or Previous material.
type RotatingMasterKeyProvider struct {
	Current  *EnvMasterKeyProvider
	Previous *EnvMasterKeyProvider
}

// LoadRotatingMasterKeyProvider loads current + optional previous keys.
func LoadRotatingMasterKeyProvider(required bool) (*RotatingMasterKeyProvider, error) {
	current, err := LoadEnvMasterKeyProvider(required)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, nil
	}
	out := &RotatingMasterKeyProvider{Current: current}
	prevRaw := strings.TrimSpace(os.Getenv(stateMasterKeyPreviousEnv))
	if prevRaw == "" {
		return out, nil
	}
	prevKey, err := parseMasterKey(prevRaw)
	if err != nil {
		return nil, fmt.Errorf("CLIPROXY_STATE_MASTER_KEY_PREVIOUS: %w", err)
	}
	out.Previous = &EnvMasterKeyProvider{KeyID: "env-v0", Key: prevKey}
	return out, nil
}

func (p *RotatingMasterKeyProvider) CurrentKeyID() string {
	if p == nil || p.Current == nil {
		return ""
	}
	return p.Current.CurrentKeyID()
}

func (p *RotatingMasterKeyProvider) KeyByID(keyID string) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("rotating key provider unavailable")
	}
	if p.Current != nil && (keyID == "" || keyID == p.Current.KeyID) {
		return p.Current.KeyByID(p.Current.KeyID)
	}
	if p.Previous != nil && (keyID == "" || keyID == p.Previous.KeyID) {
		return p.Previous.KeyByID(p.Previous.KeyID)
	}
	return nil, fmt.Errorf("unknown state key id %q", keyID)
}

// DecryptEnvelopeDual tries current key material, then previous, ignoring labeled key-id mismatch.
// AES-GCM authentication rejects the wrong key; this is the safe dual-decrypt path for rotation.
func DecryptEnvelopeDual(provider *RotatingMasterKeyProvider, enc *EncryptedBlob, aad []byte) ([]byte, error) {
	if provider == nil || enc == nil {
		return nil, fmt.Errorf("encrypted blob and rotating key provider required")
	}
	var firstErr error
	try := func(src *EnvMasterKeyProvider) ([]byte, error) {
		if src == nil || len(src.Key) != 32 {
			return nil, fmt.Errorf("state master key unavailable")
		}
		aligned := &EnvMasterKeyProvider{KeyID: enc.KeyID, Key: src.Key}
		if aligned.KeyID == "" {
			aligned.KeyID = src.KeyID
		}
		return DecryptEnvelope(aligned, enc, aad)
	}
	if provider.Current != nil {
		plain, err := try(provider.Current)
		if err == nil {
			return plain, nil
		}
		firstErr = err
	}
	if provider.Previous != nil {
		plain, err := try(provider.Previous)
		if err == nil {
			return plain, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("no state master keys available for decrypt")
}

// ReencryptBlob dual-decrypts then re-seals with the current key.
func ReencryptBlob(provider *RotatingMasterKeyProvider, enc *EncryptedBlob, aad []byte) (*EncryptedBlob, error) {
	plain, err := DecryptEnvelopeDual(provider, enc, aad)
	if err != nil {
		return nil, err
	}
	return EncryptEnvelope(provider, plain, aad)
}
