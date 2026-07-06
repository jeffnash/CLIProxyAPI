package secretdlp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type Mode string

const (
	ModeRestore Mode = "restore"
	ModeRedact  Mode = "redact"
	ModeBlock   Mode = "block"
)

type Config struct {
	Enabled               bool
	Mode                  Mode
	MasterKey             []byte
	MasterKeyGenerated    bool
	TTL                   time.Duration
	MaxFindings           int
	MinValueLength        int
	Scanner               string
	HighEntropy           bool
	RedactThreshold       float64
	BetterLeaksConfidence map[string]float64
	FailClosed            bool
	LogEvents             bool
	Store                 string
	FileDir               string
	StoreFailClosed       bool
	DrainTimeout          time.Duration
	DefaultProviderPolicy string
	ProviderOverrides     map[string]string
}

func ConfigFromEnv() Config {
	enabled := envBool("SECRET_DLP_ENABLED", false)

	mode := Mode(strings.ToLower(strings.TrimSpace(os.Getenv("SECRET_DLP_MODE"))))
	if mode == "" {
		mode = ModeRestore
	}
	switch mode {
	case ModeRestore, ModeRedact, ModeBlock:
	default:
		log.Warnf("SECRET_DLP_MODE=%q is invalid; using restore", mode)
		mode = ModeRestore
	}

	masterKey := []byte(strings.TrimSpace(os.Getenv("SECRET_DLP_MASTER_KEY")))
	masterKeyGenerated := false
	if enabled && len(masterKey) == 0 {
		masterKey = randomBytes(32)
		masterKeyGenerated = true
		log.Warn("SECRET_DLP_ENABLED=true but SECRET_DLP_MASTER_KEY is unset; generated boot-scoped key. Set SECRET_DLP_MASTER_KEY on Railway for stable behavior across restarts.")
	}

	store := strings.ToLower(strings.TrimSpace(os.Getenv("SECRET_DLP_STORE")))
	if store == "" {
		store = "memory"
	}
	switch store {
	case "memory", "file":
	default:
		log.Warnf("SECRET_DLP_STORE=%q is invalid; using memory", store)
		store = "memory"
	}

	fileDir := strings.TrimSpace(os.Getenv("SECRET_DLP_FILE_DIR"))
	if fileDir == "" && store == "file" {
		if mount := strings.TrimSpace(os.Getenv("RAILWAY_VOLUME_MOUNT_PATH")); mount != "" {
			fileDir = filepath.Join(mount, "secret_dlp")
		}
	}

	return Config{
		Enabled:               enabled,
		Mode:                  mode,
		MasterKey:             masterKey,
		MasterKeyGenerated:    masterKeyGenerated,
		TTL:                   time.Duration(envInt("SECRET_DLP_TTL_SECONDS", 3600)) * time.Second,
		MaxFindings:           envInt("SECRET_DLP_MAX_FINDINGS", 256),
		MinValueLength:        envInt("SECRET_DLP_MIN_VALUE_LENGTH", 12),
		Scanner:               envString("SECRET_DLP_SCANNER", "betterleaks"),
		HighEntropy:           envBool("SECRET_DLP_HIGH_ENTROPY", false),
		RedactThreshold:       envFloat("SECRET_DLP_REDACT_THRESHOLD", 0.80),
		BetterLeaksConfidence: envFloatMap("SECRET_DLP_BETTERLEAKS_CONFIDENCE"),
		FailClosed:            envBool("SECRET_DLP_FAIL_CLOSED", false),
		LogEvents:             envBool("SECRET_DLP_LOG_EVENTS", true),
		Store:                 store,
		FileDir:               fileDir,
		StoreFailClosed:       envBool("SECRET_DLP_STORE_FAIL_CLOSED", false),
		DrainTimeout:          time.Duration(envInt("SECRET_DLP_DRAIN_SECONDS", 25)) * time.Second,
		DefaultProviderPolicy: normalizeProviderPolicy(envString("SECRET_DLP_DEFAULT_PROVIDER_POLICY", "enabled")),
		ProviderOverrides:     normalizeProviderPolicyMap(envStringMap("SECRET_DLP_PROVIDER_OVERRIDES")),
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.RedactThreshold <= 0 || cfg.RedactThreshold > 1 {
		cfg.RedactThreshold = 0.80
	}
	cfg.DefaultProviderPolicy = normalizeProviderPolicy(cfg.DefaultProviderPolicy)
	if cfg.DefaultProviderPolicy == "" {
		cfg.DefaultProviderPolicy = "enabled"
	}
	cfg.ProviderOverrides = normalizeProviderPolicyMap(cfg.ProviderOverrides)
	return cfg
}

func WithProviderPolicy(cfg Config, defaultPolicy string, overrides map[string]string) Config {
	defaultPolicy = normalizeProviderPolicy(defaultPolicy)
	if defaultPolicy != "" {
		cfg.DefaultProviderPolicy = defaultPolicy
	}
	cfg.ProviderOverrides = mergeProviderPolicyMap(cfg.ProviderOverrides, overrides)
	return cfg
}

func envString(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		log.Warnf("%s=%q is not a boolean; using %v", key, v, fallback)
		return fallback
	}
}

func envFloat(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || n > 1 {
		log.Warnf("%s=%q is not a confidence value in (0, 1]; using %.2f", key, v, fallback)
		return fallback
	}
	return n
}

func envFloatMap(key string) map[string]float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	out := make(map[string]float64)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			log.Warnf("%s entry %q must be rule_id=confidence; skipping", key, entry)
			continue
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || n > 1 {
			log.Warnf("%s entry %q has invalid confidence; expected 0 < confidence <= 1", key, entry)
			continue
		}
		out[name] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envStringMap(key string) map[string]string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			log.Warnf("%s entry %q must be provider=policy; skipping", key, entry)
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeProviderPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "enabled", "enable", "on", "true", "1", "yes":
		return "enabled"
	case "disabled", "disable", "off", "false", "0", "no":
		return "disabled"
	case "inherit", "":
		return ""
	default:
		log.Warnf("secret dlp provider policy %q is invalid; using inherit", policy)
		return ""
	}
}

func normalizeProviderPolicyMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for name, policy := range values {
		name = strings.ToLower(strings.TrimSpace(name))
		policy = normalizeProviderPolicy(policy)
		if name != "" && policy != "" {
			out[name] = policy
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeProviderPolicyMap(base, overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return normalizeProviderPolicyMap(base)
	}
	out := normalizeProviderPolicyMap(base)
	if out == nil {
		out = make(map[string]string, len(overrides))
	}
	for name, policy := range normalizeProviderPolicyMap(overrides) {
		out[name] = policy
	}
	return out
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Warnf("%s=%q is not a positive integer; using %d", key, v, fallback)
		return fallback
	}
	return n
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Warnf("secret dlp random source failed; falling back to boot-scoped digest: %v", err)
		sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		return sum[:]
	}
	return b
}

func randomToken(n int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(n))
}
