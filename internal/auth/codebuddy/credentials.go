package codebuddy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Epoch magnitude boundaries for expiresAt/expires_at values. Values below
// 10^10 are seconds; values from 10^12 through 9999999999999 are milliseconds.
// The gap and anything larger are rejected, never guessed.
const (
	epochSecondsMax = int64(10000000000)
	epochMillisMin  = int64(1000000000000)
	epochMillisMax  = int64(9999999999999)
)

// ImportCredentials parses an explicitly supplied credential file. It accepts
// the official nested shape, flat camelCase, manager snake_case and canonical
// CLIProxyAPI CodeBuddy files. Exactly one coherent shape is accepted:
// conflicting camelCase/snake_case duplicates or conflicting nested/flat
// identity values are errors. Missing machine/session IDs are generated once
// here; metadata reads never generate them.
func ImportCredentials(raw []byte, realmOverride string, now time.Time) (Credentials, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Credentials{}, fmt.Errorf("codebuddy: empty credential file")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var top map[string]any
	if err := decoder.Decode(&top); err != nil {
		return Credentials{}, fmt.Errorf("codebuddy: invalid credential JSON: %w", err)
	}
	if err := ensureNoTrailingJSON(decoder); err != nil {
		return Credentials{}, err
	}
	if top == nil {
		return Credentials{}, fmt.Errorf("codebuddy: invalid credential JSON")
	}

	collected := map[string]string{}
	sources := map[string]string{}
	setField := func(name, value, source string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		if prev, ok := collected[name]; ok {
			if prev != value {
				return fmt.Errorf("codebuddy: conflicting %s in %s and %s", name, sources[name], source)
			}
			return nil
		}
		collected[name] = value
		sources[name] = source
		return nil
	}

	if rawType, ok := top["type"]; ok && rawType != nil {
		typeName, ok := rawType.(string)
		if !ok || (strings.TrimSpace(typeName) != "" && strings.TrimSpace(typeName) != Provider) {
			return Credentials{}, fmt.Errorf("codebuddy: credential file is not a CodeBuddy credential")
		}
	}

	nestedAuth, err := nestedObject(top, "auth")
	if err != nil {
		return Credentials{}, err
	}
	nestedAccount, err := nestedObject(top, "account")
	if err != nil {
		return Credentials{}, err
	}

	stringField := func(obj map[string]any, key string) (string, error) {
		rawValue, ok := obj[key]
		if !ok || rawValue == nil {
			return "", nil
		}
		value, ok := rawValue.(string)
		if !ok {
			return "", fmt.Errorf("codebuddy: field %q must be a string", key)
		}
		return value, nil
	}

	if nestedAuth != nil {
		for _, key := range []string{"accessToken", "refreshToken", "domain", "realm"} {
			value, err := stringField(nestedAuth, key)
			if err != nil {
				return Credentials{}, err
			}
			if err := setField(canonicalFieldName(key), value, "auth"); err != nil {
				return Credentials{}, err
			}
		}
	}
	if nestedAccount != nil {
		for _, key := range []string{"uid", "enterpriseId", "nickname"} {
			value, err := stringField(nestedAccount, key)
			if err != nil {
				return Credentials{}, err
			}
			if err := setField(canonicalFieldName(key), value, "account"); err != nil {
				return Credentials{}, err
			}
		}
	}
	for _, key := range []string{
		"accessToken", "access_token", "refreshToken", "refresh_token",
		"domain", "realm", "uid", "enterpriseId", "enterprise_id",
		"nickname", "device_token", "machine_id", "machineId", "codebuddy_session_id",
	} {
		rawValue, ok := top[key]
		if !ok || rawValue == nil {
			continue
		}
		value, ok := rawValue.(string)
		if !ok {
			return Credentials{}, fmt.Errorf("codebuddy: field %q must be a string", key)
		}
		if err := setField(canonicalFieldName(key), value, "credential"); err != nil {
			return Credentials{}, err
		}
	}

	expired, hasExpiry, err := importExpiry(top, nestedAuth)
	if err != nil {
		return Credentials{}, err
	}

	explicitRealm := strings.TrimSpace(realmOverride)
	if explicitRealm == "" {
		explicitRealm = collected["realm"]
	}
	realm, err := ParseRealm(explicitRealm, collected["domain"])
	if err != nil {
		return Credentials{}, err
	}

	refreshToken := collected["refresh_token"]
	if !hasExpiry && strings.TrimSpace(refreshToken) == "" {
		if _, hasExpiresIn := top["expires_in"]; hasExpiresIn {
			return Credentials{}, fmt.Errorf("codebuddy: credential has expires_in without an acquisition timestamp and no refresh token; re-login is required")
		}
		return Credentials{}, fmt.Errorf("codebuddy: no usable expiration and no refresh token; re-login is required")
	}
	if hasExpiry && !expired.IsZero() && !now.IsZero() && expired.Before(now) && strings.TrimSpace(refreshToken) == "" {
		return Credentials{}, fmt.Errorf("codebuddy: credential expired and no refresh token is available; re-login is required")
	}

	machineID := collected["machine_id"]
	if machineID == "" {
		machineID, err = randomHexID()
		if err != nil {
			return Credentials{}, fmt.Errorf("codebuddy: generate machine identity: %w", err)
		}
	}
	sessionID := collected["session_id"]
	if sessionID == "" {
		sessionID, err = randomHexID()
		if err != nil {
			return Credentials{}, fmt.Errorf("codebuddy: generate session identity: %w", err)
		}
	}

	credentials := Credentials{
		Realm:        realm,
		AccessToken:  collected["access_token"],
		RefreshToken: refreshToken,
		UID:          collected["uid"],
		EnterpriseID: collected["enterprise_id"],
		Nickname:     collected["nickname"],
		Domain:       collected["domain"],
		DeviceToken:  collected["device_token"],
		MachineID:    machineID,
		SessionID:    sessionID,
		Expired:      expired,
	}
	if err := credentials.Validate(); err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

// canonicalFieldName maps documented key spellings to one logical field.
func canonicalFieldName(key string) string {
	switch key {
	case "accessToken", "access_token":
		return "access_token"
	case "refreshToken", "refresh_token":
		return "refresh_token"
	case "enterpriseId", "enterprise_id":
		return "enterprise_id"
	case "machine_id", "machineId":
		return "machine_id"
	case "codebuddy_session_id":
		return "session_id"
	default:
		return key
	}
}

// nestedObject extracts an optional nested object, rejecting non-object shapes.
func nestedObject(top map[string]any, key string) (map[string]any, error) {
	raw, ok := top[key]
	if !ok || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("codebuddy: field %q must be an object", key)
	}
	return obj, nil
}

// ensureNoTrailingJSON rejects trailing content after the top-level value.
func ensureNoTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("codebuddy: invalid credential JSON: trailing content")
		}
		return fmt.Errorf("codebuddy: invalid credential JSON: %w", err)
	}
	return nil
}

// importExpiry resolves the credential expiry. Canonical RFC3339 wins, then
// epoch expiresAt/expires_at by magnitude. expires_in alone leaves expiry
// unknown: the caller must refresh with the supplied refresh token before
// saving instead of computing now + expires_in.
func importExpiry(top, nestedAuth map[string]any) (time.Time, bool, error) {
	if raw, ok := top["expired"]; ok && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return time.Time{}, false, fmt.Errorf("codebuddy: field \"expired\" must be an RFC3339 timestamp")
		}
		text = strings.TrimSpace(text)
		if text != "" {
			parsed, err := time.Parse(time.RFC3339, text)
			if err != nil {
				return time.Time{}, false, fmt.Errorf("codebuddy: invalid expired timestamp: %w", err)
			}
			return parsed.UTC(), true, nil
		}
	}
	atValues := []any{}
	if nestedAuth != nil {
		if raw, ok := nestedAuth["expiresAt"]; ok && raw != nil {
			atValues = append(atValues, raw)
		}
	}
	if raw, ok := top["expiresAt"]; ok && raw != nil {
		atValues = append(atValues, raw)
	}
	if raw, ok := top["expires_at"]; ok && raw != nil {
		atValues = append(atValues, raw)
	}
	if len(atValues) > 0 {
		first, err := epochToInt64(atValues[0])
		if err != nil {
			return time.Time{}, false, err
		}
		for _, raw := range atValues[1:] {
			other, err := epochToInt64(raw)
			if err != nil {
				return time.Time{}, false, err
			}
			if other != first {
				return time.Time{}, false, fmt.Errorf("codebuddy: conflicting expiresAt and expires_at values")
			}
		}
		if first == 0 {
			return time.Time{}, false, nil
		}
		expired, err := epochToTime(first)
		if err != nil {
			return time.Time{}, false, err
		}
		return expired, true, nil
	}
	return time.Time{}, false, nil
}

// epochToInt64 accepts an integer epoch as a JSON number or integer string.
func epochToInt64(raw any) (int64, error) {
	switch value := raw.(type) {
	case json.Number:
		return parseEpochInteger(value.String())
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0, fmt.Errorf("codebuddy: empty epoch value")
		}
		return parseEpochInteger(trimmed)
	default:
		return 0, fmt.Errorf("codebuddy: epoch value must be an integer")
	}
}

// parseEpochInteger rejects fractions, exponents and overflow.
func parseEpochInteger(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("codebuddy: empty epoch value")
	}
	negative := false
	digits := raw
	if digits[0] == '-' {
		negative = true
		digits = digits[1:]
	} else if digits[0] == '+' {
		digits = digits[1:]
	}
	if digits == "" {
		return 0, fmt.Errorf("codebuddy: invalid epoch value %q", raw)
	}
	var value int64
	for i := 0; i < len(digits); i++ {
		digit := digits[i]
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("codebuddy: invalid epoch value %q", raw)
		}
		value = value*10 + int64(digit-'0')
		if value < 0 {
			return 0, fmt.Errorf("codebuddy: epoch value %q overflows", raw)
		}
	}
	if negative {
		return 0, fmt.Errorf("codebuddy: invalid epoch value %q", raw)
	}
	return value, nil
}

// epochToTime converts an epoch to UTC time by magnitude.
func epochToTime(value int64) (time.Time, error) {
	switch {
	case value < 0:
		return time.Time{}, fmt.Errorf("codebuddy: invalid epoch value %d", value)
	case value < epochSecondsMax:
		return time.Unix(value, 0).UTC(), nil
	case value < epochMillisMin:
		return time.Time{}, fmt.Errorf("codebuddy: ambiguous epoch value %d (neither seconds nor milliseconds)", value)
	case value <= epochMillisMax:
		return time.UnixMilli(value).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("codebuddy: epoch value %d overflows", value)
	}
}

// CredentialsFromMetadata rebuilds credentials from a canonical auth record.
// It never generates device identity: missing machine/session IDs stay empty.
func CredentialsFromMetadata(metadata map[string]any) (Credentials, error) {
	if len(metadata) == 0 {
		return Credentials{}, fmt.Errorf("codebuddy: missing auth metadata")
	}
	stringsOf := func(key string) (string, error) {
		raw, ok := metadata[key]
		if !ok || raw == nil {
			return "", nil
		}
		value, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("codebuddy: metadata %q must be a string", key)
		}
		return strings.TrimSpace(value), nil
	}
	if typeName, err := stringsOf("type"); err != nil {
		return Credentials{}, err
	} else if typeName != "" && typeName != Provider {
		return Credentials{}, fmt.Errorf("codebuddy: metadata type %q is not a CodeBuddy credential", typeName)
	}
	realmRaw, err := stringsOf("realm")
	if err != nil {
		return Credentials{}, err
	}
	domain, err := stringsOf("domain")
	if err != nil {
		return Credentials{}, err
	}
	realm, err := ParseRealm(realmRaw, domain)
	if err != nil {
		return Credentials{}, err
	}
	accessToken, err := stringsOf("access_token")
	if err != nil {
		return Credentials{}, err
	}
	refreshToken, err := stringsOf("refresh_token")
	if err != nil {
		return Credentials{}, err
	}
	uid, err := stringsOf("uid")
	if err != nil {
		return Credentials{}, err
	}
	enterpriseID, err := stringsOf("enterprise_id")
	if err != nil {
		return Credentials{}, err
	}
	nickname, err := stringsOf("nickname")
	if err != nil {
		return Credentials{}, err
	}
	deviceToken, err := stringsOf("device_token")
	if err != nil {
		return Credentials{}, err
	}
	machineID, err := stringsOf("machine_id")
	if err != nil {
		return Credentials{}, err
	}
	sessionID, err := stringsOf("codebuddy_session_id")
	if err != nil {
		return Credentials{}, err
	}
	var expired time.Time
	if raw, ok := metadata["expired"]; ok && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return Credentials{}, fmt.Errorf("codebuddy: metadata \"expired\" must be an RFC3339 timestamp")
		}
		if strings.TrimSpace(text) != "" {
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
			if err != nil {
				return Credentials{}, fmt.Errorf("codebuddy: invalid expired timestamp: %w", err)
			}
			expired = parsed.UTC()
		}
	}
	if expired.IsZero() && strings.TrimSpace(refreshToken) == "" {
		return Credentials{}, fmt.Errorf("codebuddy: no usable expiration and no refresh token; re-login is required")
	}
	credentials := Credentials{
		Realm:        realm,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UID:          uid,
		EnterpriseID: enterpriseID,
		Nickname:     nickname,
		Domain:       domain,
		DeviceToken:  deviceToken,
		MachineID:    machineID,
		SessionID:    sessionID,
		Expired:      expired,
	}
	if err := credentials.Validate(); err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

// Metadata emits the canonical auth record for credentials, catalog snapshot
// and fetch time. The session key is codebuddy_session_id to avoid confusing
// a stable device/session header with token lifecycle metadata.
func Metadata(credentials Credentials, catalog Catalog, fetchedAt time.Time) map[string]any {
	metadata := map[string]any{
		"type":                      Provider,
		"realm":                     string(credentials.Realm),
		"uid":                       credentials.UID,
		"nickname":                  credentials.Nickname,
		"domain":                    credentials.Domain,
		"enterprise_id":             credentials.EnterpriseID,
		"access_token":              credentials.AccessToken,
		"refresh_token":             credentials.RefreshToken,
		"device_token":              credentials.DeviceToken,
		"machine_id":                credentials.MachineID,
		"codebuddy_session_id":      credentials.SessionID,
		"last_refresh":              fetchedAt.UTC().Format(time.RFC3339),
		CatalogMetadataKey:          catalog,
		CatalogUpdatedAtMetadataKey: fetchedAt.UTC().Format(time.RFC3339),
	}
	if !credentials.Expired.IsZero() {
		metadata["expired"] = credentials.Expired.UTC().Format(time.RFC3339)
	}
	return metadata
}

// UpdatedTokenMetadata returns a copy of the old record with only token
// lifecycle keys replaced. Catalog, realm, UID, machine/session IDs and all
// operator settings (disabled state, priority, weight, aliases, exclusions,
// proxy) are preserved untouched.
func UpdatedTokenMetadata(old map[string]any, credentials Credentials, refreshedAt time.Time) map[string]any {
	updated := make(map[string]any, len(old)+5)
	for key, value := range old {
		updated[key] = value
	}
	updated["access_token"] = credentials.AccessToken
	updated["refresh_token"] = credentials.RefreshToken
	updated["domain"] = credentials.Domain
	if !credentials.Expired.IsZero() {
		updated["expired"] = credentials.Expired.UTC().Format(time.RFC3339)
	}
	updated["last_refresh"] = refreshedAt.UTC().Format(time.RFC3339)
	return updated
}

// Filename derives the deterministic canonical filename from realm and UID.
// The same UID in different realms maps to different files, and nicknames or
// domains never become part of a path.
func Filename(credentials Credentials) string {
	sum := sha256.Sum256([]byte(string(credentials.Realm) + "\x00" + credentials.UID))
	return fmt.Sprintf("codebuddy-%s-%x.json", credentials.Realm, sum[:16])
}
