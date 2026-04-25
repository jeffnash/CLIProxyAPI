package iflow

import "strings"

// ExtractBXAuth extracts the BXAuth value from a cookie string.
func ExtractBXAuth(cookie string) string {
	parts := strings.Split(cookie, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "BXAuth=") {
			return strings.TrimPrefix(part, "BXAuth=")
		}
	}
	return ""
}
