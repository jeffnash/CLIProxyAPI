package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateCodeBuddyFlags(t *testing.T) {
	cases := []struct {
		name       string
		login      bool
		importPath string
		realm      string
		exportMode bool
		otherLogin bool
		wantErr    bool
	}{
		{"login cn", true, "", "cn", false, false, false},
		{"login default realm", true, "", "", false, false, false},
		{"import preserves profile", false, "/tmp/x.json", "", false, false, false},
		{"import with realm", false, "/tmp/x.json", "workbuddy-global", false, false, false},
		{"export", false, "", "", true, false, false},
		{"export with realm", false, "", "global", true, false, false},
		{"no mode", false, "", "", false, false, false},
		{"login plus import", true, "/tmp/x.json", "", false, false, true},
		{"login plus export", true, "", "", true, false, true},
		{"realm without mode", false, "", "cn", false, false, true},
		{"bad realm", true, "", "eu", false, false, true},
		{"combined with kimi", true, "", "", false, true, true},
		{"import combined with other", false, "/tmp/x.json", "", false, true, true},
		{"export combined with other", false, "", "", true, true, true},
	}
	for _, tc := range cases {
		err := validateCodeBuddyFlags(tc.login, tc.importPath, tc.realm, tc.exportMode, tc.otherLogin)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

var codeBuddyServerBinaryOnce sync.Once
var codeBuddyServerBinaryPath string
var codeBuddyServerBinaryErr error

func codeBuddyServerBinary(t *testing.T) string {
	t.Helper()
	codeBuddyServerBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "codebuddy-flags")
		if err != nil {
			codeBuddyServerBinaryErr = err
			return
		}
		codeBuddyServerBinaryPath = filepath.Join(dir, "cli-proxy-api-flags-test")
		build := exec.Command("go", "build", "-o", codeBuddyServerBinaryPath, "./cmd/server")
		build.Dir = "../.."
		if out, err := build.CombinedOutput(); err != nil {
			codeBuddyServerBinaryErr = err
			codeBuddyServerBinaryPath = ""
			_, _ = os.Stderr.Write(out)
		}
	})
	if codeBuddyServerBinaryErr != nil {
		t.Fatalf("build server: %v", codeBuddyServerBinaryErr)
	}
	return codeBuddyServerBinaryPath
}

func TestCodeBuddyFlagsExitNonzero(t *testing.T) {
	binary := codeBuddyServerBinary(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 18081\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cases := [][]string{
		{"--config", configPath, "--codebuddy-login", "--codebuddy-import", "/tmp/x.json"},
		{"--config", configPath, "--codebuddy-login", "--codebuddy-export"},
		{"--config", configPath, "--codebuddy-realm", "cn"},
		{"--config", configPath, "--codebuddy-login", "--codebuddy-realm", "eu"},
		{"--config", configPath, "--codebuddy-login", "--kimi-login"},
	}
	for _, args := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, binary, args...)
		output, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			t.Fatalf("%v: exit 0, output: %s", args, output)
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("%v: err = %v", args, err)
		}
		if !strings.Contains(string(output), "codebuddy") {
			t.Fatalf("%v: output = %s", args, output)
		}
	}
}
