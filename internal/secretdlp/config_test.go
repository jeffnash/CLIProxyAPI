package secretdlp

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfigFromEnvGeneratesBootScopedMasterKeyWhenMissing(t *testing.T) {
	t.Setenv("SECRET_DLP_ENABLED", "true")
	t.Setenv("SECRET_DLP_MASTER_KEY", "")

	cfg := ConfigFromEnv()
	if !cfg.Enabled {
		t.Fatal("ConfigFromEnv().Enabled = false, want true")
	}
	if len(cfg.MasterKey) == 0 {
		t.Fatal("ConfigFromEnv().MasterKey is empty, want boot-scoped key")
	}
	if cfg.TTL != time.Hour {
		t.Fatalf("ConfigFromEnv().TTL = %s, want 1h", cfg.TTL)
	}
	if cfg.HighEntropy {
		t.Fatal("ConfigFromEnv().HighEntropy = true, want false by default")
	}
	if cfg.RedactThreshold != 0.80 {
		t.Fatalf("ConfigFromEnv().RedactThreshold = %.2f, want 0.80", cfg.RedactThreshold)
	}
}

func TestConfigFromEnvHighEntropyOptIn(t *testing.T) {
	t.Setenv("SECRET_DLP_HIGH_ENTROPY", "true")

	cfg := ConfigFromEnv()
	if !cfg.HighEntropy {
		t.Fatal("ConfigFromEnv().HighEntropy = false, want true")
	}
}

func TestConfigFromEnvRedactThreshold(t *testing.T) {
	t.Setenv("SECRET_DLP_REDACT_THRESHOLD", "0.95")

	cfg := ConfigFromEnv()
	if cfg.RedactThreshold != 0.95 {
		t.Fatalf("ConfigFromEnv().RedactThreshold = %.2f, want 0.95", cfg.RedactThreshold)
	}
}

func TestConfigFromEnvRejectsRedactThresholdAboveOne(t *testing.T) {
	t.Setenv("SECRET_DLP_REDACT_THRESHOLD", "80")

	cfg := ConfigFromEnv()
	if cfg.RedactThreshold != 0.80 {
		t.Fatalf("ConfigFromEnv().RedactThreshold = %.2f, want fallback 0.80", cfg.RedactThreshold)
	}
}

func TestNormalizeConfigRejectsRedactThresholdAboveOne(t *testing.T) {
	cfg := normalizeConfig(Config{RedactThreshold: 80})
	if cfg.RedactThreshold != 0.80 {
		t.Fatalf("normalizeConfig().RedactThreshold = %.2f, want fallback 0.80", cfg.RedactThreshold)
	}
}

func TestConfigFromEnvBetterLeaksConfidence(t *testing.T) {
	t.Setenv("SECRET_DLP_BETTERLEAKS_CONFIDENCE", "betterleaks-custom=0.93,jwt=0.41,bad-entry,zero=0,too_high=1.5")

	cfg := ConfigFromEnv()
	if cfg.BetterLeaksConfidence["betterleaks-custom"] != 0.93 {
		t.Fatalf("BetterLeaksConfidence[betterleaks-custom] = %.2f, want 0.93", cfg.BetterLeaksConfidence["betterleaks-custom"])
	}
	if cfg.BetterLeaksConfidence["jwt"] != 0.41 {
		t.Fatalf("BetterLeaksConfidence[jwt] = %.2f, want 0.41", cfg.BetterLeaksConfidence["jwt"])
	}
	if _, ok := cfg.BetterLeaksConfidence["bad-entry"]; ok {
		t.Fatal("BetterLeaksConfidence contains invalid bad-entry")
	}
	if _, ok := cfg.BetterLeaksConfidence["zero"]; ok {
		t.Fatal("BetterLeaksConfidence contains invalid zero confidence")
	}
	if _, ok := cfg.BetterLeaksConfidence["too_high"]; ok {
		t.Fatal("BetterLeaksConfidence contains invalid too_high confidence")
	}
}

func TestConfigFromEnvUsesRailwayVolumeForFileStore(t *testing.T) {
	t.Setenv("SECRET_DLP_ENABLED", "true")
	t.Setenv("SECRET_DLP_MASTER_KEY", "stable-master-key")
	t.Setenv("SECRET_DLP_STORE", "file")
	t.Setenv("SECRET_DLP_FILE_DIR", "")
	t.Setenv("RAILWAY_VOLUME_MOUNT_PATH", "/app/auths_railway")

	cfg := ConfigFromEnv()
	if cfg.Store != storeFile {
		t.Fatalf("ConfigFromEnv().Store = %q, want %q", cfg.Store, storeFile)
	}
	wantDir := filepath.Join("/app/auths_railway", "secret_dlp")
	if cfg.FileDir != wantDir {
		t.Fatalf("ConfigFromEnv().FileDir = %q, want %q", cfg.FileDir, wantDir)
	}
	if cfg.MasterKeyGenerated {
		t.Fatal("ConfigFromEnv().MasterKeyGenerated = true, want false")
	}
}
