package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	grokauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/grok"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// DoGrokLogin prompts for Grok SSO JWT and saves it as an auth file.
// This mirrors cookie-based login flows for other providers.
func DoGrokLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	fmt.Println("Grok login:")
	fmt.Println("1) Open https://grok.com in your browser and sign in.")
	fmt.Println("2) Open DevTools (F12), go to Application/Storage -> Cookies -> https://grok.com, find the 'sso' cookie.")
	fmt.Println("3) Copy the cookie VALUE (JWT) only — do not include 'sso=' or ';sso-rw='. Paste it when prompted.")
	fmt.Println("4) If a 'cf_clearance' cookie exists, copy its value (without the 'cf_clearance=' prefix) for the optional prompt.")

	promptFn := options.Prompt
	if promptFn == nil {
		reader := bufio.NewReader(os.Stdin)
		promptFn = func(prompt string) (string, error) {
			fmt.Print(prompt)
			text, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(text), nil
		}
	}

	label, _ := promptFn("Label for this Grok credential (default: grok): ")
	sso, err := promptFn("Enter Grok SSO JWT (bare value, not the full cookie): ")
	if err != nil {
		fmt.Printf("Failed to read SSO token: %v\n", err)
		return
	}
	sso = grokauth.NormalizeSSOToken(sso)
	if sso == "" {
		fmt.Println("SSO token cannot be empty")
		return
	}

	cf, _ := promptFn("Enter cf_clearance value (optional, just the value): ")
	cf = strings.TrimSpace(strings.TrimPrefix(cf, "cf_clearance="))
	tokenType, _ := promptFn("Token type (normal/super, default normal): ")
	tokenType = strings.ToLower(strings.TrimSpace(tokenType))
	if tokenType != "super" {
		tokenType = "normal"
	}

	storage := &grokauth.GrokTokenStorage{
		SSOToken:              sso,
		CFClearance:           cf,
		TokenType:             tokenType,
		Status:                "active",
		FailedCount:           0,
		RemainingQueries:      -1,
		HeavyRemainingQueries: -1,
		Note:                  label,
		Type:                  "grok",
	}

	if cfg == nil {
		cfg = &config.Config{}
	}
	authDir := cfg.AuthDir
	if authDir == "" {
		authDir = ".auth"
	}
	filename := fmt.Sprintf("grok-%s.json", sanitizeGrokLabel(label))
	path := filepath.Join(authDir, filename)

	if err := storage.SaveTokenToFile(path); err != nil {
		fmt.Printf("Failed to save Grok authentication: %v\n", err)
		return
	}

	fmt.Printf("Grok authentication saved to: %s\n", path)
}

func sanitizeGrokLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "default"
	}
	re := regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
	clean := re.ReplaceAllString(raw, "_")
	if clean == "" {
		return "default"
	}
	return clean
}
