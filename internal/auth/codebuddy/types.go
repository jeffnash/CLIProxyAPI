// Package codebuddy implements native CodeBuddy account authentication and
// Tencent HY4 model catalog handling for CLIProxyAPI.
//
// Realm is an endpoint/account profile, not just geography. The three profiles
// are "cn" (China), "global" (CodeBuddy international, www.codebuddy.ai) and
// "workbuddy-global" (WorkBuddy international, www.workbuddy.ai). Their tokens,
// catalog entitlements and hosts are separate and must never be collapsed.
package codebuddy

import (
	"fmt"
	"strings"
	"time"
)

// Provider is the native provider key for CodeBuddy.
const Provider = "codebuddy"

// Realm identifies a CodeBuddy endpoint/account profile.
type Realm string

// Supported CodeBuddy realms.
const (
	RealmCN              Realm = "cn"
	RealmGlobal          Realm = "global"
	RealmWorkBuddyGlobal Realm = "workbuddy-global"
)

// Canonical metadata keys for the persisted catalog snapshot.
const (
	CatalogMetadataKey          = "codebuddy_catalog"
	CatalogUpdatedAtMetadataKey = "codebuddy_catalog_updated_at"
)

// Credentials holds the normalized CodeBuddy account credential.
type Credentials struct {
	Realm        Realm
	AccessToken  string
	RefreshToken string
	UID          string
	EnterpriseID string
	Nickname     string
	Domain       string
	DeviceToken  string
	MachineID    string
	SessionID    string
	Expired      time.Time
}

// LoginSession captures a pending browser authorization.
type LoginSession struct {
	State     string
	AuthURL   string
	Realm     Realm
	ExpiresAt time.Time
}

// Model describes one HY4 catalog entry from the account's own snapshot.
type Model struct {
	ID                string   `json:"id"`
	Name              string   `json:"name,omitempty"`
	MaxInputTokens    int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	SupportsTools     *bool    `json:"supports_tools,omitempty"`
	SupportsImages    *bool    `json:"supports_images,omitempty"`
	SupportsReasoning *bool    `json:"supports_reasoning,omitempty"`
	OnlyReasoning     *bool    `json:"only_reasoning,omitempty"`
	Efforts           []string `json:"efforts,omitempty"`
	DefaultEffort     string   `json:"default_effort,omitempty"`
}

// Catalog is an account-scoped persisted model snapshot. It intentionally has
// no token, UID or realm field: the containing credential owns its identity.
type Catalog struct {
	Models   []Model  `json:"models"`
	Sources  []string `json:"sources"`
	Degraded bool     `json:"degraded"`
}

// ParseRealm resolves the account profile from an explicit flag value and an
// observed domain. The explicit value wins when it agrees with the domain; a
// recognized domain that disagrees with the explicit profile is an error.
func ParseRealm(explicit, domain string) (Realm, error) {
	trimmed := strings.ToLower(strings.TrimSpace(explicit))
	if trimmed != "" {
		var realm Realm
		switch trimmed {
		case string(RealmCN):
			realm = RealmCN
		case string(RealmGlobal):
			realm = RealmGlobal
		case string(RealmWorkBuddyGlobal):
			realm = RealmWorkBuddyGlobal
		default:
			return "", fmt.Errorf("codebuddy: invalid realm %q (want cn, global or workbuddy-global)", strings.TrimSpace(explicit))
		}
		if strings.TrimSpace(domain) != "" {
			inferred, recognized, err := recognizedRealm(domain)
			if err != nil {
				return "", err
			}
			if recognized && inferred != realm {
				return "", fmt.Errorf("codebuddy: realm %q disagrees with domain %q (domain implies %q)", realm, strings.TrimSpace(domain), inferred)
			}
		}
		return realm, nil
	}
	inferred, err := inferRealmFromDomain(domain)
	if err != nil {
		return "", err
	}
	if inferred != "" {
		return inferred, nil
	}
	if strings.TrimSpace(domain) == "" {
		return RealmCN, nil
	}
	return "", fmt.Errorf("codebuddy: unfamiliar domain %q, specify an explicit realm (cn, global or workbuddy-global)", strings.TrimSpace(domain))
}

// recognizedRealm maps a hostname to its realm without failing on unfamiliar
// hosts. URL-shaped values are always rejected.
func recognizedRealm(domain string) (Realm, bool, error) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return "", false, nil
	}
	lowered := strings.ToLower(trimmed)
	if strings.Contains(lowered, "://") || strings.ContainsAny(trimmed, "/@") || strings.Contains(trimmed, ":") {
		return "", false, fmt.Errorf("codebuddy: invalid domain %q (want a hostname, not a URL)", trimmed)
	}
	if lowered == "codebuddy.ai" || strings.HasSuffix(lowered, ".codebuddy.ai") {
		return RealmGlobal, true, nil
	}
	if lowered == "workbuddy.ai" || strings.HasSuffix(lowered, ".workbuddy.ai") {
		return RealmWorkBuddyGlobal, true, nil
	}
	if lowered == "codebuddy.cn" || strings.HasSuffix(lowered, ".codebuddy.cn") {
		return RealmCN, true, nil
	}
	if lowered == "copilot.tencent.com" || strings.HasSuffix(lowered, ".copilot.tencent.com") {
		return RealmCN, true, nil
	}
	return "", false, nil
}

// inferRealmFromDomain maps a recognized hostname to its realm. It returns an
// empty realm with nil error only when the domain itself is empty. Hostnames
// are matched exactly or by dot-delimited suffix; URLs and lookalike domains
// such as evilworkbuddy.ai are rejected.
func inferRealmFromDomain(domain string) (Realm, error) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return "", nil
	}
	realm, recognized, err := recognizedRealm(trimmed)
	if err != nil {
		return "", err
	}
	if recognized {
		return realm, nil
	}
	return "", fmt.Errorf("codebuddy: unfamiliar domain %q, specify an explicit realm (cn, global or workbuddy-global)", trimmed)
}

// PublicModelID builds the public catalog ID for a realm and upstream model.
func PublicModelID(realm Realm, upstreamID string) string {
	return "codebuddy-" + string(realm) + "-" + upstreamID
}

// IsHY4Model reports whether id is an in-scope HY4 model identifier.
func IsHY4Model(id string) bool {
	return id == "hy4" || strings.HasPrefix(id, "hy4-")
}

// Validate checks the credential fields required for stable account identity.
func (c Credentials) Validate() error {
	switch c.Realm {
	case RealmCN, RealmGlobal, RealmWorkBuddyGlobal:
	default:
		return fmt.Errorf("codebuddy: invalid realm %q", string(c.Realm))
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("codebuddy: missing access token")
	}
	if strings.TrimSpace(c.UID) == "" {
		return fmt.Errorf("codebuddy: missing account UID")
	}
	for name, value := range map[string]string{
		"access token":  c.AccessToken,
		"refresh token": c.RefreshToken,
		"uid":           c.UID,
		"domain":        c.Domain,
		"device token":  c.DeviceToken,
		"machine id":    c.MachineID,
		"session id":    c.SessionID,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("codebuddy: invalid %s (control characters are not allowed)", name)
		}
	}
	return nil
}
