package secretdlp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const ginSessionKey = "SECRET_DLP_SESSION"

type contextSessionKey struct{}

type Service struct {
	cfg     Config
	scanner Scanner
	store   mappingStore

	mu       sync.RWMutex
	sessions map[string]*Session
	stop     chan struct{}
	stopOnce sync.Once

	activeMu   sync.Mutex
	active     int
	draining   bool
	activeCond *sync.Cond
}

func NewFromEnv() (*Service, error) {
	cfg := ConfigFromEnv()
	if !cfg.Enabled {
		return nil, nil
	}
	return New(cfg)
}

func NewFromEnvWithProviderPolicy(defaultPolicy string, overrides map[string]string) (*Service, error) {
	cfg := WithProviderPolicy(ConfigFromEnv(), defaultPolicy, overrides)
	if !cfg.Enabled {
		return nil, nil
	}
	return New(cfg)
}

func New(cfg Config) (*Service, error) {
	cfg = normalizeConfig(cfg)
	if !cfg.Enabled {
		return nil, nil
	}

	scanner, err := NewScanner(cfg)
	if err != nil {
		return nil, err
	}
	store, err := newMappingStore(cfg)
	if err != nil {
		return nil, err
	}

	svc := &Service{
		cfg:      cfg,
		scanner:  scanner,
		store:    store,
		sessions: make(map[string]*Session),
		stop:     make(chan struct{}),
	}
	svc.activeCond = sync.NewCond(&svc.activeMu)
	go svc.reapExpired()
	return svc, nil
}

func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

func (s *Service) ProviderRedactionEnabled(provider, authPolicy string) bool {
	if s == nil || !s.cfg.Enabled {
		return false
	}
	if policy := normalizeProviderPolicy(authPolicy); policy != "" {
		return policy == "enabled"
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "" && len(s.cfg.ProviderOverrides) > 0 {
		if policy := normalizeProviderPolicy(s.cfg.ProviderOverrides[provider]); policy != "" {
			return policy == "enabled"
		}
	}
	policy := normalizeProviderPolicy(s.cfg.DefaultProviderPolicy)
	if policy == "" {
		policy = "enabled"
	}
	return policy == "enabled"
}

func (s *Service) RedactPayload(ctx context.Context, path string, body []byte) ([]byte, *Session, error) {
	var c *gin.Context
	if ctx != nil {
		c, _ = ctx.Value("gin").(*gin.Context)
	}
	return s.redactPayload(c, ctx, path, body)
}

func (s *Service) RedactGinPayload(c *gin.Context, body []byte) ([]byte, *Session, error) {
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	return s.redactPayload(c, requestContext(c), path, body)
}

func (s *Service) redactPayload(c *gin.Context, ctx context.Context, path string, body []byte) ([]byte, *Session, error) {
	if s == nil || !s.cfg.Enabled || len(bytes.TrimSpace(body)) == 0 {
		return body, nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pack := packForRoute(path)
	if pack.RawOnly {
		return s.redactRawFallback(ctx, c, body)
	}

	doc, err := tokenizeJSON(body)
	if err != nil {
		return s.redactRawFallback(ctx, c, body)
	}
	segments := extractSegments(doc, pack)
	if len(segments) == 0 {
		return s.passthroughBody(c, body)
	}
	identifiers := harvestIdentifiers(doc, pack)

	findings, err := s.scanner.Scan(ctx, segments)
	if err != nil {
		if s.cfg.FailClosed {
			return nil, nil, err
		}
		log.Warnf("secret dlp scan failed; passing request through because fail-closed is disabled: %v", err)
		return body, nil, nil
	}

	if len(findings) == 0 {
		return s.passthroughBody(c, body)
	}

	findings, shadow := filterFindings(findings, identifiers, s.cfg)
	s.logTransformEvent("shadow_finding", nil, len(shadow), log.Fields{
		"stage": "provider_egress",
	}, "secret dlp observed shadow candidate finding(s)")
	if len(findings) == 0 {
		return s.passthroughBody(c, body)
	}

	return s.redactWithFindings(ctx, c, body, findings, func(session *Session) ([]byte, []Mapping) {
		return session.RedactSegments(body, segments, findings)
	})
}

func (s *Service) redactRawFallback(ctx context.Context, c *gin.Context, body []byte) ([]byte, *Session, error) {
	findings := explicitRawFindings(body, s.cfg.MinValueLength, s.cfg.MaxFindings)
	if len(findings) == 0 {
		return s.passthroughBody(c, body)
	}
	findings, shadow := filterFindings(findings, nil, s.cfg)
	s.logTransformEvent("shadow_finding", nil, len(shadow), log.Fields{
		"stage": "provider_egress",
	}, "secret dlp observed shadow candidate finding(s)")
	if len(findings) == 0 {
		return s.passthroughBody(c, body)
	}
	return s.redactWithFindings(ctx, c, body, findings, func(session *Session) ([]byte, []Mapping) {
		return session.RedactRawWithMappings(body, findings)
	})
}

func (s *Service) redactWithFindings(ctx context.Context, c *gin.Context, body []byte, findings []Finding, redact func(*Session) ([]byte, []Mapping)) ([]byte, *Session, error) {
	var req *http.Request
	if c != nil {
		req = c.Request
	}
	session := sessionFromGin(c)
	if session == nil || session.Expired(time.Now()) {
		session = NewSession(s.cfg.MasterKey, clientCredential(req), s.cfg.TTL, s.cfg.Mode)
	}

	redacted, mappings := redact(session)
	if len(mappings) == 0 {
		return body, nil, nil
	}

	attachGinSession(c, session)

	if s.cfg.Mode == ModeBlock {
		return nil, session, fmt.Errorf("secret dlp blocked request with %d candidate secret(s)", len(mappings))
	}

	if s.cfg.Mode == ModeRestore {
		if ctx == nil {
			ctx = requestContext(c)
		}
		if err := s.persistMappings(ctx, session, mappings); err != nil {
			if s.cfg.StoreFailClosed {
				return nil, nil, err
			}
			log.Warnf("secret dlp mapping persistence failed; continuing with in-memory mappings because store fail-closed is disabled: %v", err)
		}
	}
	s.storeSession(session)

	s.logTransformEvent("redact_request", session, len(findings), log.Fields{
		"mappings": len(mappings),
		"stage":    "provider_egress",
	}, "secret dlp redacted candidate secret(s) before provider egress")

	return redacted, session, nil
}

func (s *Service) passthroughBody(c *gin.Context, body []byte) ([]byte, *Session, error) {
	return body, s.ensurePassthroughSession(c, body), nil
}

func (s *Service) ensurePassthroughSession(c *gin.Context, body []byte) *Session {
	if s == nil || len(body) == 0 || !placeholderPattern.Match(body) {
		return nil
	}

	var req *http.Request
	if c != nil {
		req = c.Request
	}
	session := sessionFromGin(c)
	if session == nil || session.Expired(time.Now()) {
		session = NewSession(s.cfg.MasterKey, clientCredential(req), s.cfg.TTL, s.cfg.Mode)
	}
	attachGinSession(c, session)
	s.storeSession(session)
	return session
}

func requestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func (s *Service) RestoreResponse(ctx context.Context, body []byte) []byte {
	if s == nil || !s.cfg.Enabled || s.cfg.Mode != ModeRestore {
		return bytes.Clone(body)
	}
	session := SessionFromContext(ctx)
	if session == nil {
		return bytes.Clone(body)
	}
	restored, count := session.RestoreJSONWithResolverStats(body, s.placeholderResolver(ctx))
	s.logTransformEvent("restore_response", session, count, log.Fields{
		"source": "session",
		"stage":  "client_response",
	}, "secret dlp rehydrated placeholder secret(s) before client response")
	return restored
}

func (s *Service) RestoreStreamChunk(ctx context.Context, body []byte) []byte {
	if s == nil || !s.cfg.Enabled || s.cfg.Mode != ModeRestore {
		return bytes.Clone(body)
	}
	session := SessionFromContext(ctx)
	if session == nil {
		return bytes.Clone(body)
	}
	restored, count := session.RestoreStreamJSONChunkWithResolverStats(body, s.placeholderResolver(ctx))
	s.logTransformEvent("restore_stream_response", session, count, log.Fields{
		"source": "session",
		"stage":  "client_response",
	}, "secret dlp rehydrated streamed placeholder secret(s) before client response")
	return restored
}

func (s *Service) FlushStream(ctx context.Context) []byte {
	if s == nil || !s.cfg.Enabled || s.cfg.Mode != ModeRestore {
		return nil
	}
	session := SessionFromContext(ctx)
	if session == nil {
		return nil
	}
	restored, count := session.FlushStreamJSONTailWithResolverStats(s.placeholderResolver(ctx))
	s.logTransformEvent("restore_stream_tail", session, count, log.Fields{
		"source": "session",
		"stage":  "client_response",
	}, "secret dlp rehydrated streamed placeholder tail before client response")
	return restored
}

func (s *Service) RedactForLog(ctx context.Context, body []byte) []byte {
	if s == nil || !s.cfg.Enabled {
		return bytes.Clone(body)
	}
	session := SessionFromContext(ctx)
	if session == nil {
		return bytes.Clone(body)
	}
	redacted, count := session.RedactForLogStats(body)
	s.logTransformEvent("redact_response_log", session, count, log.Fields{
		"stage": "response_log",
	}, "secret dlp redacted restored secret(s) before response logging")
	return redacted
}

func WithSession(ctx context.Context, session *Session) context.Context {
	if ctx == nil || session == nil {
		return ctx
	}
	return context.WithValue(ctx, contextSessionKey{}, session)
}

func SessionFromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}

	if session, ok := ctx.Value(contextSessionKey{}).(*Session); ok && session != nil {
		return session
	}

	if gc, ok := ctx.Value("gin").(*gin.Context); ok && gc != nil {
		return sessionFromGin(gc)
	}

	return nil
}

func attachGinSession(c *gin.Context, session *Session) {
	if c == nil || session == nil {
		return
	}
	c.Set(ginSessionKey, session)
	if c.Request != nil {
		c.Request = c.Request.WithContext(WithSession(c.Request.Context(), session))
	}
}

func sessionFromGin(c *gin.Context) *Session {
	if c == nil {
		return nil
	}
	value, exists := c.Get(ginSessionKey)
	if !exists {
		return nil
	}
	session, _ := value.(*Session)
	return session
}

func clientCredential(req *http.Request) string {
	if req == nil {
		return "unknown-client"
	}

	// Session.ClientID is an HMAC-short of this credential, never the raw value.
	// Store restore isolation depends on the authenticated client credential being
	// stable for the caller and unavailable to other callers.
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):])
	}
	if auth != "" {
		return auth
	}

	for _, key := range []string{"X-API-Key", "Api-Key", "x-api-key"} {
		if v := strings.TrimSpace(req.Header.Get(key)); v != "" {
			return v
		}
	}

	return "unknown-client"
}

func (s *Service) storeSession(session *Session) {
	if s == nil || session == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
}

func (s *Service) persistMappings(ctx context.Context, session *Session, mappings []Mapping) error {
	if s == nil || s.store == nil || session == nil || len(mappings) == 0 {
		return nil
	}
	for _, mapping := range mappings {
		if mapping.Placeholder == "" || len(mapping.Secret) == 0 {
			continue
		}
		if err := s.store.Put(ctx, storedMapping{
			Placeholder: mapping.Placeholder,
			Secret:      mapping.Secret,
			SessionID:   session.ID,
			ClientID:    session.ClientID,
			ExpiresAt:   session.ExpiresAt,
		}); err != nil {
			return fmt.Errorf("secret dlp store put placeholder_id=%s: %w", placeholderLogID(mapping.Placeholder), err)
		}
	}
	return nil
}

func (s *Service) placeholderResolver(ctx context.Context) PlaceholderResolver {
	clientID := ""
	if session := SessionFromContext(ctx); session != nil {
		clientID = session.ClientID
	}
	return func(placeholder string) ([]byte, bool) {
		return s.lookupPlaceholder(ctx, placeholder, clientID)
	}
}

func (s *Service) lookupPlaceholder(ctx context.Context, placeholder string, clientID string) ([]byte, bool) {
	if s == nil || s.store == nil || placeholder == "" {
		return nil, false
	}
	secret, ok, err := s.store.Get(ctx, placeholder, clientID, time.Now())
	if err != nil {
		log.Warnf("secret dlp store lookup failed for placeholder_id=%s: %v", placeholderLogID(placeholder), err)
		return nil, false
	}
	return secret, ok
}

func (s *Service) logTransformEvent(action string, session *Session, count int, fields log.Fields, message string) {
	if s == nil || !s.cfg.LogEvents || count <= 0 {
		return
	}
	if fields == nil {
		fields = log.Fields{}
	}
	fields["component"] = "secret_dlp"
	fields["action"] = action
	fields["occurrences"] = count
	if session != nil {
		fields["request_id"] = session.ID
		fields["client_id"] = session.ClientID
		fields["mode"] = session.Mode
	}
	log.WithFields(fields).Info(message)
}

func (s *Service) BeginRequest() func() {
	if s == nil {
		return func() {}
	}
	s.activeMu.Lock()
	s.active++
	s.activeMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeMu.Lock()
			if s.active > 0 {
				s.active--
			}
			if s.active == 0 {
				s.activeCond.Broadcast()
			}
			s.activeMu.Unlock()
		})
	}
}

func (s *Service) ActiveRequests() int {
	if s == nil {
		return 0
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.active
}

func (s *Service) StartDrain() {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	s.draining = true
	if s.active == 0 {
		s.activeCond.Broadcast()
	}
	s.activeMu.Unlock()
}

func (s *Service) DrainTimeout() time.Duration {
	if s == nil {
		return 0
	}
	return s.cfg.DrainTimeout
}

func (s *Service) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.StartDrain()

	s.activeMu.Lock()
	if s.active == 0 {
		s.activeMu.Unlock()
		return nil
	}
	active := s.active
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.activeMu.Lock()
		defer s.activeMu.Unlock()
		for s.active > 0 {
			s.activeCond.Wait()
		}
	}()
	s.activeMu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("secret dlp drain timed out with %d active request(s): %w", active, ctx.Err())
	}
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.store != nil {
			err = s.store.Close()
		}
	})
	return err
}

func (s *Service) reapExpired() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.mu.Lock()
			for id, session := range s.sessions {
				if session == nil || session.Expired(now) {
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
			if s.store != nil {
				if err := s.store.CleanupExpired(context.Background(), now); err != nil {
					log.Warnf("secret dlp store cleanup failed: %v", err)
				}
			}
		}
	}
}
