package durablestate

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// BlobStore is an encrypted content-addressed blob store for envelopes/stream blocks.
type BlobStore struct {
	root     string
	provider MasterKeyProvider
	mu       sync.Mutex
}

// NewBlobStore creates (or opens) a directory-backed encrypted blob store.
func NewBlobStore(root string, provider MasterKeyProvider) (*BlobStore, error) {
	if root == "" {
		return nil, errors.New("blob store root required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &BlobStore{root: root, provider: provider}, nil
}

// CompressForStorage gzip-compresses plaintext before encryption.
func CompressForStorage(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plaintext); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecompressForStorage gunzips storage payloads.
func DecompressForStorage(compressed []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Put compresses+encrypts plaintext under AAD and returns a content hash ref.
// The content address is sha256(plaintext) so identical envelopes dedupe despite
// randomized AES-GCM nonces on the ciphertext.
func (s *BlobStore) Put(plaintext, aad []byte) (ref string, digest string, err error) {
	if s == nil {
		return "", "", errors.New("blob store required")
	}
	if s.provider == nil {
		return "", "", errors.New("state master key provider required for blob writes")
	}
	sum := sha256.Sum256(plaintext)
	digest = hex.EncodeToString(sum[:])
	ref = "sha256:" + digest
	path := s.pathFor(digest)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return ref, digest, nil
	}
	compressed, err := CompressForStorage(plaintext)
	if err != nil {
		return "", "", err
	}
	enc, err := EncryptEnvelope(s.provider, compressed, aad)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	tmp := path + ".tmp"
	framed := encodeBlobFile(enc)
	if err := os.WriteFile(tmp, framed, 0o600); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	return ref, digest, nil
}

// Get decrypts a blob ref into plaintext (decompressed).
func (s *BlobStore) Get(ref string, aad []byte) ([]byte, error) {
	if s == nil {
		return nil, errors.New("blob store required")
	}
	digest := stringsTrimPrefixSHA(ref)
	if digest == "" {
		return nil, errors.New("invalid blob ref")
	}
	raw, err := os.ReadFile(s.pathFor(digest))
	if err != nil {
		return nil, err
	}
	enc, err := decodeBlobFile(raw)
	if err != nil {
		return nil, err
	}
	plainCompressed, err := DecryptEnvelope(s.provider, enc, aad)
	if err != nil {
		return nil, err
	}
	plain, err := DecompressForStorage(plainCompressed)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(plain)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("blob plaintext digest mismatch")
	}
	return plain, nil
}

// Verify checks that a content-addressed blob file exists and has ciphertext framing.
func (s *BlobStore) Verify(ref string) error {
	digest := stringsTrimPrefixSHA(ref)
	if digest == "" {
		return errors.New("invalid blob ref")
	}
	raw, err := os.ReadFile(s.pathFor(digest))
	if err != nil {
		return err
	}
	enc, err := decodeBlobFile(raw)
	if err != nil {
		return err
	}
	if len(enc.Blob) == 0 {
		return errors.New("empty encrypted blob")
	}
	return nil
}

func (s *BlobStore) pathFor(digest string) string {
	if len(digest) < 4 {
		return filepath.Join(s.root, digest)
	}
	return filepath.Join(s.root, digest[:2], digest[2:4], digest)
}

func encodeBlobFile(enc *EncryptedBlob) []byte {
	out := make([]byte, 0, len(enc.KeyID)+2+len(enc.Blob))
	out = append(out, []byte(enc.KeyID)...)
	out = append(out, 0)
	out = append(out, enc.Version)
	out = append(out, 0)
	out = append(out, enc.Blob...)
	return out
}

func decodeBlobFile(raw []byte) (*EncryptedBlob, error) {
	if len(raw) < 3 {
		return nil, errors.New("blob file too short")
	}
	i := 0
	for i < len(raw) && raw[i] != 0 {
		i++
	}
	if i >= len(raw)-2 || raw[i] != 0 {
		return nil, errors.New("malformed blob key id")
	}
	keyID := string(raw[:i])
	version := raw[i+1]
	if raw[i+2] != 0 {
		return nil, errors.New("malformed blob framing")
	}
	blob := append([]byte(nil), raw[i+3:]...)
	return &EncryptedBlob{KeyID: keyID, Version: version, Blob: blob}, nil
}

func stringsTrimPrefixSHA(ref string) string {
	const p = "sha256:"
	if strings.HasPrefix(ref, p) {
		return strings.TrimPrefix(ref, p)
	}
	if len(ref) == 64 {
		return ref
	}
	return ""
}

// PersistEnvelopeBlob stores ciphertext and returns digest + blob ref for invocation records.
func PersistEnvelopeBlob(store *BlobStore, tenantID, conversationID, invocationID string, plaintext []byte) (digest, ref string, err error) {
	if store == nil {
		sum := sha256.Sum256(plaintext)
		return hex.EncodeToString(sum[:]), "", nil
	}
	aad := BuildAAD(tenantID, conversationID, invocationID, 1)
	ref, digest, err = store.Put(plaintext, aad)
	if err != nil {
		return "", "", err
	}
	return digest, ref, nil
}

// ReencryptAll walks stored blobs and re-seals any that decrypt under the rotating provider
// but were written with previous key material. aad must match the original Put AAD.
func (s *BlobStore) ReencryptAll(provider *RotatingMasterKeyProvider, aad []byte) (rewritten int, err error) {
	if s == nil {
		return 0, errors.New("blob store required")
	}
	if provider == nil || provider.Current == nil {
		return 0, errors.New("rotating key provider required")
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		level1 := filepath.Join(s.root, ent.Name())
		inner, err := os.ReadDir(level1)
		if err != nil {
			return rewritten, err
		}
		for _, mid := range inner {
			if !mid.IsDir() {
				continue
			}
			level2 := filepath.Join(level1, mid.Name())
			files, err := os.ReadDir(level2)
			if err != nil {
				return rewritten, err
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				path := filepath.Join(level2, file.Name())
				raw, err := os.ReadFile(path)
				if err != nil {
					return rewritten, err
				}
				enc, err := decodeBlobFile(raw)
				if err != nil {
					return rewritten, err
				}
				// Skip when current key material already opens the blob.
				if _, err := DecryptEnvelope(provider.Current, enc, aad); err == nil {
					continue
				}
				reenc, err := ReencryptBlob(provider, enc, aad)
				if err != nil {
					return rewritten, err
				}
				tmp := path + ".reenc"
				if err := os.WriteFile(tmp, encodeBlobFile(reenc), 0o600); err != nil {
					return rewritten, err
				}
				if err := os.Rename(tmp, path); err != nil {
					_ = os.Remove(tmp)
					return rewritten, err
				}
				rewritten++
			}
		}
	}
	return rewritten, nil
}
