package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestListCopilotAuthIDsFromDisk_FiltersAndSorts(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"copilot_individual_b.json": `{"type":"copilot","github_token":"ghu_b"}`,
		"copilot_individual_a.json": `{"type":"copilot","github_token":"ghu_a"}`,
		"kimi-1.json":                 `{"type":"kimi","access_token":"x"}`,
		"notes.txt":                  `not json`,
		"broken.json":                `{`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cfg := &config.Config{AuthDir: dir}
	got, err := listCopilotAuthIDsFromDisk(cfg)
	if err != nil {
		t.Fatalf("listCopilotAuthIDsFromDisk error: %v", err)
	}

	want := []string{"copilot_individual_a.json", "copilot_individual_b.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected ids\n got: %#v\nwant: %#v", got, want)
	}
}
