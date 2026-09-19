package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestFileSynthesizer_CodeBuddyMetadata verifies the existing generic file
// synthesizer preserves canonical CodeBuddy records without a
// provider-specific parser branch.
func TestFileSynthesizer_CodeBuddyMetadata(t *testing.T) {
	dir := t.TempDir()
	creds := codebuddy.Credentials{
		Realm: codebuddy.RealmGlobal, AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		UID: "fixture-user", Nickname: "Fixture", Domain: "www.codebuddy.ai",
		MachineID: "mid", SessionID: "sid", Expired: time.Date(2033, 5, 18, 3, 33, 20, 0, time.UTC),
	}
	tools := true
	catalog := codebuddy.Catalog{
		Models:  []codebuddy.Model{{ID: "hy4-preview", SupportsTools: &tools}},
		Sources: []string{"/v3/config"},
	}
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	metadata := codebuddy.Metadata(creds, catalog, now)
	metadata["proxy_url"] = "http://proxy.test"
	metadata["priority"] = 7
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	name := codebuddy.Filename(creds)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	synth := NewFileSynthesizer()
	auths, err := synth.Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     dir,
		Now:         now,
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths = %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "codebuddy" {
		t.Fatalf("provider = %q", auth.Provider)
	}
	roundCreds, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if roundCreds.UID != "fixture-user" || roundCreds.Realm != codebuddy.RealmGlobal || roundCreds.MachineID != "mid" {
		t.Fatalf("credentials = %+v", roundCreds)
	}
	roundCatalog, err := codebuddy.CatalogFromMetadata(auth.Metadata)
	if err != nil || len(roundCatalog.Models) != 1 {
		t.Fatalf("catalog = %+v %v", roundCatalog, err)
	}
	if auth.Metadata["proxy_url"] != "http://proxy.test" {
		t.Fatalf("proxy lost: %v", auth.Metadata)
	}
}
