package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	hnTopStoriesURL = "https://hacker-news.firebaseio.com/v0/topstories.json"
	hnItemURLFmt    = "https://hacker-news.firebaseio.com/v0/item/%d.json"
)

func hotTakesInterval() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv("COPILOT_HOT_TAKES_INTERVAL_MINS"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Warnf("copilot hot takes: invalid COPILOT_HOT_TAKES_INTERVAL_MINS=%q; feature disabled", raw)
		return 0, false
	}
	return time.Duration(n) * time.Minute, true
}

func hotTakesModel() string {
	raw := strings.TrimSpace(os.Getenv("COPILOT_HOT_TAKES_MODEL"))
	if raw == "" {
		// User typo support
		raw = strings.TrimSpace(os.Getenv("COPILOT_HOT_TAKES_MOEL"))
	}
	if raw == "" {
		raw = "claude-haiku-4.5"
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "copilot-") {
		raw = "copilot-" + raw
	}
	return raw
}

func pickRandomUnique(ids []int64, n int) []int64 {
	if n <= 0 || len(ids) == 0 {
		return nil
	}
	if n > len(ids) {
		n = len(ids)
	}
	// Fisher-Yates partial shuffle.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	out := append([]int64(nil), ids...)
	for i := 0; i < n; i++ {
		j := i + r.Intn(len(out)-i)
		out[i], out[j] = out[j], out[i]
	}
	return out[:n]
}

func fetchTopStoryIDs(ctx context.Context, client *http.Client) ([]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hnTopStoriesURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("hn topstories: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ids []int64
	if err := json.NewDecoder(resp.Body).Decode(&ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func fetchHNTitle(ctx context.Context, client *http.Client, id int64) (string, error) {
	u := fmt.Sprintf(hnItemURLFmt, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("hn item %d: status %d: %s", id, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(gjson.GetBytes(body, "title").String())
	if title == "" {
		return "", fmt.Errorf("hn item %d: missing title", id)
	}
	return title, nil
}

func extractAssistantText(respBytes []byte) string {
	// Chat Completions (string)
	if v := gjson.GetBytes(respBytes, "choices.0.message.content"); v.Exists() && v.Type == gjson.String {
		return v.String()
	}
	// Chat Completions (array parts)
	if v := gjson.GetBytes(respBytes, "choices.0.message.content.0.text"); v.Exists() && v.Type == gjson.String {
		return v.String()
	}
	// Responses API-ish
	if v := gjson.GetBytes(respBytes, "output.0.content.0.text"); v.Exists() && v.Type == gjson.String {
		return v.String()
	}
	// Fallback
	return strings.TrimSpace(string(respBytes))
}

func listCopilotAuthIDs(ctx context.Context, cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("no api-keys configured")
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/v0/management/auth-files", cfg.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKeys[0])
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth-files status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Files []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, f := range parsed.Files {
		provider := strings.ToLower(strings.TrimSpace(f.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(f.Type))
		}
		if provider != "copilot" {
			continue
		}
		id := strings.TrimSpace(f.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func doCopilotHotTakesOnce(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	if len(cfg.APIKeys) == 0 {
		return fmt.Errorf("no api-keys configured; cannot call local server")
	}

	hnClient := &http.Client{Timeout: 15 * time.Second}
	ids, err := fetchTopStoryIDs(ctx, hnClient)
	if err != nil {
		return err
	}
	// Shuffle the full list and take the first 7 titles we can fetch.
	// This preserves the "random 7 IDs from topstories" intent while avoiding the
	// "sometimes fewer than 7 titles" outcome when an item fetch fails.
	shuffled := pickRandomUnique(ids, len(ids))
	titles := make([]string, 0, 7)
	for _, id := range shuffled {
		title, err := fetchHNTitle(ctx, hnClient, id)
		if err != nil {
			log.Debugf("copilot hot takes: skip HN item %d: %v", id, err)
			continue
		}
		titles = append(titles, title)
		if len(titles) >= 7 {
			break
		}
	}
	if len(titles) == 0 {
		return fmt.Errorf("no HN titles fetched")
	}
	if len(titles) < 7 {
		log.Warnf("copilot hot takes: only fetched %d/7 titles; continuing anyway", len(titles))
	}

	var b strings.Builder
	b.WriteString("What do you think about these headliens?\n")
	for _, t := range titles {
		b.WriteString("- ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	prompt := b.String()

	payload := map[string]any{
		"model": hotTakesModel(),
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
		"stream": false,
	}
	raw, _ := json.Marshal(payload)
	localURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", cfg.Port)
	client := &http.Client{Timeout: 120 * time.Second}

	authIDs, err := listCopilotAuthIDs(ctx, cfg)
	if err != nil {
		log.Warnf("copilot hot takes: list copilot auths failed, falling back to unpinned call: %v", err)
		authIDs = []string{""}
	}
	if len(authIDs) == 0 {
		log.Warn("copilot hot takes: no copilot auths found; running single unpinned call")
		authIDs = []string{""}
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i, authID := range authIDs {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, localURL, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKeys[0])
		// Override initiator for this background job.
		req.Header.Set("force-copilot-initiator", "user")
		if authID != "" {
			req.Header.Set("X-Pinned-Auth-Id", authID)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Warnf("copilot hot takes: call %d/%d auth=%q failed: %v", i+1, len(authIDs), authID, err)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			_ = resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				log.Warnf("copilot hot takes: call %d/%d auth=%q status %d: %s", i+1, len(authIDs), authID, resp.StatusCode, strings.TrimSpace(string(body)))
			} else {
				out := extractAssistantText(body)
				log.Infof("[copilot hot takes] call=%d/%d auth=%q model=%s stories=%d\n%s", i+1, len(authIDs), authID, hotTakesModel(), len(titles), out)
			}
		}

		if i < len(authIDs)-1 {
			// Space per-account calls: 30s ± 3s.
			j := time.Duration(r.Int63n(int64(6*time.Second)+1)) - 3*time.Second
			sleep := 30*time.Second + j
			if sleep < 1*time.Second {
				sleep = 1 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleep):
			}
		}
	}
	return nil
}

func waitForLocalServer(ctx context.Context, port int) error {
	// Poll /v1/models until it responds, or ctx cancels.
	u := fmt.Sprintf("http://127.0.0.1:%d/v1/models", port)
	client := &http.Client{Timeout: 2 * time.Second}
	backoff := 250 * time.Millisecond
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("local server not ready after 2m")
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func waitForCopilotAuthReady(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	if cfg.Port == 0 {
		return fmt.Errorf("missing port")
	}
	if len(cfg.APIKeys) == 0 || strings.TrimSpace(cfg.APIKeys[0]) == "" {
		return fmt.Errorf("missing api key")
	}

	targetModel := hotTakesModel()
	u := fmt.Sprintf("http://127.0.0.1:%d/v1/models", cfg.Port)
	client := &http.Client{Timeout: 5 * time.Second}
	backoff := 500 * time.Millisecond
	deadline := time.Now().Add(5 * time.Minute)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("copilot not ready after 5m (waiting for model %q)", targetModel)
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKeys[0])
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				// Consider Copilot ready if we see at least one Copilot model, ideally the target.
				if gjson.GetBytes(body, "data").IsArray() {
					foundAnyCopilot := false
					foundTarget := false
					for _, it := range gjson.GetBytes(body, "data").Array() {
						id := strings.ToLower(strings.TrimSpace(it.Get("id").String()))
						if strings.HasPrefix(id, "copilot-") {
							foundAnyCopilot = true
							if id == strings.ToLower(targetModel) {
								foundTarget = true
								break
							}
						}
					}
					if foundTarget || foundAnyCopilot {
						return nil
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func StartCopilotHotTakesLoop(ctx context.Context, cfg *config.Config) {
	interval, ok := hotTakesInterval()
	if !ok {
		return
	}
	if cfg == nil {
		log.Warn("copilot hot takes: nil config; disabled")
		return
	}

	go func() {
		if err := waitForLocalServer(ctx, cfg.Port); err != nil {
			log.Warnf("copilot hot takes: server readiness failed: %v", err)
			return
		}

		// Don't attempt the Copilot call until Copilot auth/models have loaded.
		if err := waitForCopilotAuthReady(ctx, cfg); err != nil {
			log.Warnf("copilot hot takes: copilot readiness failed: %v", err)
			return
		}

		// Run once immediately, then on the interval.
		if err := doCopilotHotTakesOnce(ctx, cfg); err != nil {
			log.Warnf("copilot hot takes: run failed: %v", err)
		}

		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			// Add jitter to avoid hammering at a fixed schedule:
			// sleep = interval +/- random(0..3 minutes)
			j := time.Duration(r.Int63n(int64(3*time.Minute) + 1))
			if r.Intn(2) == 0 {
				j = -j
			}
			sleep := interval + j
			// Clamp to a sane minimum so negative jitter can't collapse the loop.
			if sleep < 10*time.Second {
				sleep = 10 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
			runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			err := doCopilotHotTakesOnce(runCtx, cfg)
			cancel()
			if err != nil {
				log.Warnf("copilot hot takes: run failed: %v", err)
			}
		}
	}()
}
