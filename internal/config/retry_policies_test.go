package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetryPolicyEnvironmentValidation(t *testing.T) {
	for _, raw := range []string{`broken`, `[{"max-retries":-1,"delays-seconds":[1]}]`, `[{"delays-seconds":[]}]`, `[{"delays-seconds":[-1]}]`, `[{"delays-seconds":[1],"proxy-urls":["garbage"]}]`, `[{"delays-seconds":[1],"errors":[{"status":200}]}]`} {
		t.Setenv("RETRY_POLICIES_JSON", raw)
		cfg := &Config{}
		if err := cfg.loadRetryPolicies(); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	t.Setenv("RETRY_POLICIES_JSON", `[{"providers":["claude"],"models":["muse-spark-1.3-contributor"],"max-retries":12,"window-seconds":180,"delays-seconds":[1,2,4,8,15],"errors":[{"status":404,"contains":"Model not found"}]}]`)
	cfg := &Config{}
	if err := cfg.loadRetryPolicies(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.RetryPolicies) != 1 || cfg.RetryPolicies[0].MaxRetries != 12 {
		t.Fatal("environment config lost")
	}
}

func TestRetryPolicyLoadedFromYAMLAndEnvironment(t *testing.T) {
	t.Setenv("RETRY_POLICIES_JSON", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("retry-policies:\n  - providers: [claude]\n    models: [test]\n    max-retries: 7\n    delays-seconds: [1, 2, 4]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RetryPolicies) != 1 || cfg.RetryPolicies[0].MaxRetries != 7 {
		t.Fatal("YAML policy lost")
	}
	t.Setenv("RETRY_POLICIES_JSON", `[{"max-retries":0,"delays-seconds":[0]}]`)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RetryPolicies) != 1 || cfg.RetryPolicies[0].MaxRetries != 0 || len(cfg.RetryPolicies[0].Providers) != 0 {
		t.Fatal("environment did not replace YAML")
	}
}
