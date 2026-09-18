package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
)

// RetryPolicy is opt-in. The first matching rule owns additional retry rounds.
// Empty selectors match any value; specified selectors are combined with AND.
type RetryPolicy struct {
	Providers        []string           `yaml:"providers,omitempty" json:"providers,omitempty"`
	Models           []string           `yaml:"models,omitempty" json:"models,omitempty"`
	BaseURL          string             `yaml:"base-url,omitempty" json:"base-url,omitempty"`
	MaxRetries       int                `yaml:"max-retries" json:"max-retries"`
	WindowSeconds    float64            `yaml:"window-seconds,omitempty" json:"window-seconds,omitempty"`
	DelaysSeconds    []float64          `yaml:"delays-seconds" json:"delays-seconds"`
	Errors           []RetryPolicyError `yaml:"errors,omitempty" json:"errors,omitempty"`
	RetryProxyErrors bool               `yaml:"retry-proxy-errors,omitempty" json:"retry-proxy-errors,omitempty"`
	ProxyURLs        []string           `yaml:"proxy-urls,omitempty" json:"proxy-urls,omitempty"`
}

type RetryPolicyError struct {
	Status   int    `yaml:"status" json:"status"`
	Contains string `yaml:"contains,omitempty" json:"contains,omitempty"`
}

func (cfg *Config) loadRetryPolicies() error {
	if raw := strings.TrimSpace(os.Getenv("RETRY_POLICIES_JSON")); raw != "" {
		var policies []RetryPolicy
		if err := json.Unmarshal([]byte(raw), &policies); err != nil {
			return fmt.Errorf("invalid RETRY_POLICIES_JSON")
		}
		cfg.RetryPolicies = policies
	}
	for i, p := range cfg.RetryPolicies {
		bad := func(field string) error { return fmt.Errorf("retry-policies[%d]: invalid %s", i, field) }
		validSeconds := func(v float64) bool {
			return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64)/1e9
		}
		if p.MaxRetries < 0 || !validSeconds(p.WindowSeconds) {
			return bad("retry limit/window")
		}
		if len(p.DelaysSeconds) == 0 {
			return bad("delays-seconds (must not be empty)")
		}
		for _, d := range p.DelaysSeconds {
			if !validSeconds(d) {
				return bad("delays-seconds")
			}
		}
		for _, e := range p.Errors {
			if e.Status < 400 || e.Status > 599 {
				return bad("error status")
			}
		}
		for _, raw := range p.ProxyURLs {
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				return bad("proxy-urls")
			}
		}
	}
	return nil
}
