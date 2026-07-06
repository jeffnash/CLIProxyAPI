package secretdlp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestServiceLogsSecretDLPTransformEventsWithoutLeakingSecret(t *testing.T) {
	hook := newSecretDLPLogHook(t)
	svc := newSecretDLPLoggingTestService(t, true)
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	const secret = "sk-testdlpfixture0000000000000000000000000000"
	ctx, response, safeLog := exerciseSecretDLPTransforms(t, svc, secret)
	if !strings.Contains(string(response), secret) {
		t.Fatalf("restored response = %q, want secret restored", response)
	}
	if strings.Contains(string(safeLog), secret) {
		t.Fatalf("safe log body still contains secret: %q", safeLog)
	}
	if SessionFromContext(ctx) == nil {
		t.Fatal("exerciseSecretDLPTransforms did not attach session")
	}

	wantActions := map[string]bool{
		"redact_request":      false,
		"restore_response":    false,
		"redact_response_log": false,
	}
	for _, entry := range hook.AllEntries() {
		if entry.Data["component"] != "secret_dlp" {
			continue
		}
		action, _ := entry.Data["action"].(string)
		if _, ok := wantActions[action]; ok {
			wantActions[action] = true
		}
		if strings.Contains(entry.Message, secret) {
			t.Fatalf("secret leaked in log message: %q", entry.Message)
		}
		for key, value := range entry.Data {
			if strings.Contains(fmt.Sprint(value), secret) {
				t.Fatalf("secret leaked in log field %q: %v", key, value)
			}
		}
	}
	for action, found := range wantActions {
		if !found {
			t.Fatalf("missing secret dlp log action %q; entries=%v", action, hook.AllEntries())
		}
	}
}

func TestServiceSuppressesSecretDLPTransformEventsWhenLogEventsDisabled(t *testing.T) {
	hook := newSecretDLPLogHook(t)
	svc := newSecretDLPLoggingTestService(t, false)
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	exerciseSecretDLPTransforms(t, svc, "sk-testdlpfixture0000000000000000000000000000")

	for _, entry := range hook.AllEntries() {
		if entry.Data["component"] == "secret_dlp" {
			t.Fatalf("unexpected secret dlp event log while disabled: %+v", entry)
		}
	}
}

func TestServiceLogsShadowFindingWithoutMutation(t *testing.T) {
	hook := newSecretDLPLogHook(t)
	svc := newSecretDLPLoggingTestService(t, true)
	svc.scanner = staticScanner{{Secret: "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6", RuleID: "high-entropy", Source: "test", Confidence: 0.70}}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"messages":[{"role":"user","content":"token A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6"}]}`)

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session != nil {
		t.Fatalf("session = %+v, want nil for shadow-only finding", session)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted body = %q, want unchanged %q", redacted, body)
	}

	for _, entry := range hook.AllEntries() {
		if entry.Data["component"] == "secret_dlp" && entry.Data["action"] == "shadow_finding" {
			return
		}
	}
	t.Fatalf("missing shadow_finding log; entries=%v", hook.AllEntries())
}

func newSecretDLPLogHook(t *testing.T) *test.Hook {
	t.Helper()
	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	return hook
}

func newSecretDLPLoggingTestService(t *testing.T, logEvents bool) *Service {
	t.Helper()
	svc, err := New(Config{
		Enabled:        true,
		Mode:           ModeRestore,
		MasterKey:      []byte("stable-master-key-for-logging"),
		TTL:            time.Hour,
		MaxFindings:    10,
		MinValueLength: 12,
		Scanner:        "builtin",
		LogEvents:      logEvents,
		Store:          storeMemory,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return svc
}

func exerciseSecretDLPTransforms(t *testing.T, svc *Service, secret string) (context.Context, []byte, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("Authorization", "Bearer cliproxy-client-key")

	redacted, session, err := svc.RedactGinPayload(c, []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, secret)))
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("RedactGinPayload() session = nil, want session")
	}
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redacted request still contains secret: %q", redacted)
	}
	placeholder := extractPlaceholderForTest(t, string(redacted))
	ctx := WithSession(context.Background(), session)
	response := svc.RestoreResponse(ctx, []byte(fmt.Sprintf(`{"echo":"%s"}`, placeholder)))
	safeLog := svc.RedactForLog(ctx, response)
	return ctx, response, safeLog
}
