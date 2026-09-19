package codebuddy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseRealm(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		domain   string
		want     Realm
		wantErr  bool
	}{
		{"explicit cn", "cn", "", RealmCN, false},
		{"explicit global padded", "  Global ", "", RealmGlobal, false},
		{"explicit workbuddy", "workbuddy-global", "", RealmWorkBuddyGlobal, false},
		{"explicit invalid", "eu", "", "", true},
		{"codebuddy ai domain", "", "www.codebuddy.ai", RealmGlobal, false},
		{"codebuddy ai bare", "", "codebuddy.ai", RealmGlobal, false},
		{"workbuddy domain", "", "www.workbuddy.ai", RealmWorkBuddyGlobal, false},
		{"workbuddy bare", "", "workbuddy.ai", RealmWorkBuddyGlobal, false},
		{"cn domain", "", "www.codebuddy.cn", RealmCN, false},
		{"cn api domain", "", "copilot.tencent.com", RealmCN, false},
		{"empty legacy", "", "", RealmCN, false},
		{"lookalike rejected", "", "evilworkbuddy.ai", "", true},
		{"suffix attack rejected", "", "workbuddy.ai.evil.test", "", true},
		{"url rejected", "", "https://www.workbuddy.ai", "", true},
		{"host with port rejected", "", "www.workbuddy.ai:443", "", true},
		{"explicit agrees with domain", "global", "www.codebuddy.ai", RealmGlobal, false},
		{"explicit disagrees", "cn", "www.codebuddy.ai", "", true},
		{"global vs workbuddy disagree", "global", "www.workbuddy.ai", "", true},
		{"unfamiliar domain", "", "example.com", "", true},
		{"explicit wins over unfamiliar", "cn", "example.com", RealmCN, false},
	}
	for _, tc := range cases {
		got, err := ParseRealm(tc.explicit, tc.domain)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestImportCredentials(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	readFixture := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return raw
	}

	t.Run("nested official milliseconds", func(t *testing.T) {
		creds, err := ImportCredentials(readFixture("credentials_nested.json"), "", now)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if creds.Realm != RealmGlobal || creds.UID != "fixture-user" || creds.DeviceToken != "SYNTHETIC_DEVICE" {
			t.Fatalf("creds = %+v", creds)
		}
		if !creds.Expired.Equal(time.UnixMilli(2000000000000).UTC()) {
			t.Fatalf("expiry = %v", creds.Expired)
		}
		if len(creds.MachineID) != 32 || len(creds.SessionID) != 32 {
			t.Fatal("device ids not generated")
		}
	})

	t.Run("flat seconds", func(t *testing.T) {
		creds, err := ImportCredentials(readFixture("credentials_flat.json"), "", now)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if creds.Realm != RealmCN || creds.EnterpriseID != "fixture-enterprise" {
			t.Fatalf("creds = %+v", creds)
		}
		if !creds.Expired.Equal(time.Unix(2000000000, 0).UTC()) {
			t.Fatalf("expiry = %v", creds.Expired)
		}
	})

	t.Run("snake case manager output", func(t *testing.T) {
		raw := `{"access_token":"A","refresh_token":"R","expires_in":3600,"domain":"www.codebuddy.cn","realm":"cn","uid":"u","enterprise_id":"e","nickname":"N"}`
		creds, err := ImportCredentials([]byte(raw), "", now)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if creds.UID != "u" || !creds.Expired.IsZero() {
			t.Fatalf("expires_in must leave expiry unknown: %+v", creds)
		}
	})

	t.Run("expires_in without refresh fails", func(t *testing.T) {
		raw := `{"access_token":"A","expires_in":3600,"uid":"u","realm":"cn"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("expected failure")
		}
	})

	t.Run("epoch gap rejected", func(t *testing.T) {
		raw := `{"accessToken":"A","refreshToken":"R","expiresAt":50000000000,"uid":"u","realm":"cn"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("gap epoch accepted")
		}
	})

	t.Run("epoch overflow rejected", func(t *testing.T) {
		raw := `{"accessToken":"A","refreshToken":"R","expiresAt":10000000000000,"uid":"u","realm":"cn"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("overflow epoch accepted")
		}
	})

	t.Run("conflicting duplicates rejected", func(t *testing.T) {
		raw := `{"accessToken":"A","access_token":"B","refreshToken":"R","expiresAt":2000000000,"uid":"u","realm":"cn"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("conflicting duplicates accepted")
		}
	})

	t.Run("conflicting nested flat identity rejected", func(t *testing.T) {
		raw := `{"auth":{"accessToken":"A","refreshToken":"R","expiresAt":2000000000000,"domain":"www.codebuddy.ai"},"account":{"uid":"u1"},"uid":"u2"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("conflicting identity accepted")
		}
	})

	t.Run("realm override wins", func(t *testing.T) {
		creds, err := ImportCredentials(readFixture("credentials_flat.json"), "global", now)
		if err == nil {
			t.Fatalf("override disagreeing with domain accepted: %+v", creds)
		}
		raw := `{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"uid":"u","domain":"www.codebuddy.cn","realm":"cn"}`
		creds, err = ImportCredentials([]byte(raw), "cn", now)
		if err != nil || creds.Realm != RealmCN {
			t.Fatalf("override failed: %+v %v", creds, err)
		}
	})

	t.Run("invalid realm rejected", func(t *testing.T) {
		raw := `{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"uid":"u","realm":"eu"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("invalid realm accepted")
		}
	})

	t.Run("trailing json rejected", func(t *testing.T) {
		raw := `{"accessToken":"A"} {"accessToken":"B"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("trailing JSON accepted")
		}
	})

	t.Run("expired without refresh rejected", func(t *testing.T) {
		raw := `{"accessToken":"A","expiresAt":1000000000,"uid":"u","realm":"cn"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("dead credential accepted")
		}
	})

	t.Run("canonical reimport preserves ids", func(t *testing.T) {
		raw := `{"type":"codebuddy","realm":"global","uid":"u","nickname":"N","domain":"www.codebuddy.ai","enterprise_id":"","access_token":"A","refresh_token":"R","device_token":"D","machine_id":"0123456789abcdef0123456789abcdef","codebuddy_session_id":"fedcba9876543210fedcba9876543210","expired":"2033-05-18T03:33:20Z"}`
		creds, err := ImportCredentials([]byte(raw), "", now)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if creds.MachineID != "0123456789abcdef0123456789abcdef" || creds.SessionID != "fedcba9876543210fedcba9876543210" {
			t.Fatalf("ids not preserved: %+v", creds)
		}
	})

	t.Run("wrong type rejected", func(t *testing.T) {
		raw := `{"type":"claude","access_token":"A","uid":"u"}`
		if _, err := ImportCredentials([]byte(raw), "", now); err == nil {
			t.Fatal("wrong type accepted")
		}
	})
}

func TestMetadataRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	tools := true
	images := false
	reasoning := true
	only := true
	catalog := Catalog{
		Models: []Model{{
			ID: "hy4-preview", Name: "HY4", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
			SupportsTools: &tools, SupportsImages: &images, SupportsReasoning: &reasoning,
			OnlyReasoning: &only, Efforts: []string{"high"}, DefaultEffort: "high",
		}},
		Sources:  []string{"/v3/config"},
		Degraded: false,
	}
	creds := Credentials{
		Realm: RealmGlobal, AccessToken: "A", RefreshToken: "R", UID: "u",
		Nickname: "N", Domain: "www.codebuddy.ai", DeviceToken: "D",
		MachineID: "m", SessionID: "s", Expired: now.Add(time.Hour),
	}
	metadata := Metadata(creds, catalog, now)
	if metadata["type"] != Provider || metadata["codebuddy_session_id"] != "s" {
		t.Fatalf("metadata = %v", metadata)
	}
	if _, ok := metadata["session_id"]; ok {
		t.Fatal("generic session_id key must not be used")
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	roundCreds, err := CredentialsFromMetadata(decoded)
	if err != nil {
		t.Fatalf("credentials round trip: %v", err)
	}
	if roundCreds != creds {
		t.Fatalf("creds = %+v want %+v", roundCreds, creds)
	}
	roundCatalog, err := CatalogFromMetadata(decoded)
	if err != nil {
		t.Fatalf("catalog round trip: %v", err)
	}
	if len(roundCatalog.Models) != 1 || roundCatalog.Models[0].ID != "hy4-preview" {
		t.Fatalf("catalog = %+v", roundCatalog)
	}
	got := roundCatalog.Models[0]
	if got.MaxInputTokens != 1000000 || got.DefaultEffort != "high" || len(got.Efforts) != 1 {
		t.Fatalf("model = %+v", got)
	}
	if got.SupportsTools == nil || !*got.SupportsTools || got.SupportsImages == nil || *got.SupportsImages {
		t.Fatalf("capabilities = %+v", got)
	}
}

func TestUpdatedTokenMetadata(t *testing.T) {
	old := map[string]any{
		"type": "codebuddy", "realm": "cn", "uid": "u", "access_token": "OLD",
		"refresh_token": "OR", "expired": "2020-01-01T00:00:00Z",
		"disabled": true, "priority": 5, "proxy_url": "http://proxy",
		CatalogMetadataKey: map[string]any{"models": []any{}},
		"machine_id":       "m", "codebuddy_session_id": "s",
	}
	updated := UpdatedTokenMetadata(old, Credentials{
		AccessToken: "NEW", RefreshToken: "NR", Domain: "d",
		Expired: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	if updated["access_token"] != "NEW" || updated["refresh_token"] != "NR" {
		t.Fatalf("tokens not replaced: %v", updated)
	}
	for _, key := range []string{"disabled", "priority", "proxy_url", CatalogMetadataKey, "machine_id", "codebuddy_session_id", "realm", "uid"} {
		if _, ok := updated[key]; !ok {
			t.Fatalf("key %s lost", key)
		}
	}
	if old["access_token"] != "OLD" {
		t.Fatal("old map mutated")
	}
}

func TestFilename(t *testing.T) {
	a := Credentials{Realm: RealmCN, UID: "same-user"}
	b := Credentials{Realm: RealmGlobal, UID: "same-user"}
	if Filename(a) == Filename(b) {
		t.Fatal("same UID across realms shares a filename")
	}
	if Filename(a) != Filename(Credentials{Realm: RealmCN, UID: "same-user"}) {
		t.Fatal("filename not deterministic")
	}
	if !strings.HasPrefix(Filename(a), "codebuddy-cn-") || !strings.HasSuffix(Filename(a), ".json") {
		t.Fatalf("filename = %q", Filename(a))
	}
	if strings.Contains(Filename(Credentials{Realm: RealmCN, UID: "../evil"}), "..") {
		t.Fatal("filename not path-safe")
	}
}
