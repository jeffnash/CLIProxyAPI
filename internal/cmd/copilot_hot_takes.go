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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, localURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKeys[0])
	// Override initiator for this background job.
	req.Header.Set("force-copilot-initiator", "user")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("copilot hot takes: local call status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	out := extractAssistantText(body)
	log.Infof("[copilot hot takes] model=%s stories=%d\n%s", hotTakesModel(), len(titles), out)
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
