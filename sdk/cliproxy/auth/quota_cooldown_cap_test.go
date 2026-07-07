package auth

import (
	"testing"
	"time"
)

func TestQuotaCooldownMaxOverride(t *testing.T) {
	cases := []struct {
		name     string
		auth     *Auth
		wantSecs int
		wantOK   bool
	}{
		{"nil auth", nil, 0, false},
		{"absent", &Auth{Attributes: map[string]string{"x": "1"}}, 0, false},
		{"attribute string", &Auth{Attributes: map[string]string{"quota_cooldown_max_seconds": "60"}}, 60, true},
		{"attribute zero", &Auth{Attributes: map[string]string{"quota_cooldown_max_seconds": "0"}}, 0, true},
		{"attribute negative ignored", &Auth{Attributes: map[string]string{"quota_cooldown_max_seconds": "-5"}}, 0, false},
		{"metadata int", &Auth{Metadata: map[string]any{"quota_cooldown_max_seconds": 90}}, 90, true},
		{"metadata float", &Auth{Metadata: map[string]any{"quota_cooldown_max_seconds": float64(45)}}, 45, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secs, ok := c.auth.QuotaCooldownMaxOverride()
			if secs != c.wantSecs || ok != c.wantOK {
				t.Fatalf("got (%d,%v), want (%d,%v)", secs, ok, c.wantSecs, c.wantOK)
			}
		})
	}
}

func TestCapQuotaCooldown(t *testing.T) {
	capped := &Auth{Attributes: map[string]string{"quota_cooldown_max_seconds": "60"}}
	uncapped := &Auth{}

	if got := capQuotaCooldown(capped, 30*time.Minute); got != 60*time.Second {
		t.Fatalf("30m should cap to 60s, got %v", got)
	}
	if got := capQuotaCooldown(capped, 10*time.Second); got != 10*time.Second {
		t.Fatalf("below cap should pass through, got %v", got)
	}
	if got := capQuotaCooldown(uncapped, 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("no override should pass through, got %v", got)
	}
	if got := capQuotaCooldown(nil, 5*time.Second); got != 5*time.Second {
		t.Fatalf("nil auth should pass through, got %v", got)
	}
}
