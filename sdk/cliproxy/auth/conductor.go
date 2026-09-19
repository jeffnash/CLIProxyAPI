package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RequestAuthPreparer lets an executor update missing auth metadata immediately
// before a request. Manager serializes and persists returned updates.
type RequestAuthPreparer interface {
	ShouldPrepareRequestAuth(auth *Auth) bool
	PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// RouteModel is the requested logical route model before alias resolution.
	RouteModel string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// CredentialScope indicates that the failure affects the whole credential across models (e.g. Anthropic 5h/7d unified limits).
	CredentialScope bool
	// Error describes the failure when Success is false.
	Error *Error
	// Options carries execution request options (headers, metadata, etc.) for result tracking.
	Options cliproxyexecutor.Options
	// SkipQuotaObservation reports that this result must not replace the last
	// observed watermark. Count-tokens requests reuse the credential but are not
	// generation traffic; their response headers are not a generation snapshot.
	SkipQuotaObservation bool
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

type PluginScheduler interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

// PluginSchedulerAcrossPriorities is an optional interface implemented by schedulers
// that opt into receiving candidates across all priority tiers.
type PluginSchedulerAcrossPriorities interface {
	SchedulerWantsAcrossPriorities() bool
}

type pluginSchedulerState interface {
	HasScheduler() bool
}

// StoppableSelector is an optional interface for selectors that hold resources.
// Selectors that implement this interface will have Stop called during shutdown.
type StoppableSelector interface {
	Selector
	Stop()
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// ResultPolicy allows inspecting and mutating an execution result before in-memory quota mutations,
// cooldown persistence, and scheduler/registry publishing.
// Implementations of ResultPolicy must be safe for concurrent use by multiple goroutines.
type ResultPolicy interface {
	ApplyResultPolicy(ctx context.Context, result Result) Result
}

// ResultPolicyFunc enables using a plain function as a ResultPolicy.
type ResultPolicyFunc func(ctx context.Context, result Result) Result

// ApplyResultPolicy calls f(ctx, result).
func (f ResultPolicyFunc) ApplyResultPolicy(ctx context.Context, result Result) Result {
	return f(ctx, result)
}

type resultPolicyHolder struct {
	policy ResultPolicy
}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store                     Store
	cooldownStore             CooldownStateStore
	pendingCooldownStateStore CooldownStateStore
	executors                 map[string]ProviderExecutor
	selector                  Selector
	hook                      Hook
	resultPolicy              atomic.Pointer[resultPolicyHolder]
	mu                        sync.RWMutex
	selectorMu                sync.Mutex
	configCooldownMu          sync.Mutex
	auths                     map[string]*Auth
	authEpochs                map[string]uint64
	scheduler                 *authScheduler
	// pluginScheduler runs outside m.mu before falling back to native selection.
	pluginScheduler PluginScheduler
	// homeRuntimeAuths retains legacy session auth lookups for non-execution callers.
	homeRuntimeAuths map[string]map[string]*Auth
	// homeRuntimeAuthOwners prevents a stale selection from clearing a replacement auth.
	homeRuntimeAuthOwners map[string]map[string]*HomeDispatchSelection
	// homeSessionSelections owns retained Home selections for websocket sessions.
	homeSessionSelections map[string]map[homeSessionSelectionKey]*HomeDispatchSelection
	homeSessionLocks      sync.Map
	homeSessionAliases    homeSessionAliasCache
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets             map[string]int
	homeDispatchBundle          atomic.Pointer[HomeDispatchBundle]
	homeInFlightPublisherConfig atomic.Pointer[HomeInFlightPublisherConfig]

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelRouting atomically publishes per-auth aliases and configured capabilities.
	apiKeyModelRouting atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel context.CancelFunc
	refreshLoop   *authAutoRefreshLoop

	requestPrepareLocks sync.Map
	// refreshLocks serializes credential refresh per auth ID so concurrent
	// 401 recoveries and auto-refresh workers do not race the same refresh_token.
	refreshLocks sync.Map
	// persistLocks serializes disk persistence per auth ID and guards against out-of-order writes.
	persistLocks sync.Map
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                 store,
		executors:             make(map[string]ProviderExecutor),
		selector:              selector,
		hook:                  hook,
		auths:                 make(map[string]*Auth),
		authEpochs:            make(map[string]uint64),
		homeRuntimeAuths:      make(map[string]map[string]*Auth),
		homeRuntimeAuthOwners: make(map[string]map[string]*HomeDispatchSelection),
		homeSessionSelections: make(map[string]map[homeSessionSelectionKey]*HomeDispatchSelection),
		providerOffsets:       make(map[string]int),
		modelPoolOffsets:      make(map[string]int),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelRouting.Store(&apiKeyModelRoutingSnapshot{config: &internalconfig.Config{}})
	defaultInFlightConfig, errInFlightConfig := HomeInFlightPublisherConfigFromConfig(internalconfig.DefaultCredentialInFlightConfig())
	if errInFlightConfig == nil {
		manager.ApplyHomeInFlightPublisherConfig(defaultInFlightConfig)
	}
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

// Preserved ours-branch (HEAD c08af90c) helpers whose callers or contracts
// survive in the merged tree. Upstream split conductor into
// conductor_*.go files; these have no upstream counterpart.

func (m *Manager) ClearCooldown(ctx context.Context, provider, model string) ([]string, []string, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("auth manager is nil")
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return nil, nil, fmt.Errorf("provider is required")
	}
	now := time.Now()
	m.mu.Lock()
	// Collect matching auths (copy pointers while holding lock; mutation under same lock).
	matching := make([]*Auth, 0)
	for _, a := range m.auths {
		if a == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.Provider), provider) {
			matching = append(matching, a)
		}
	}
	if len(matching) == 0 {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("no auth found for provider %q", provider)
	}
	type pending struct {
		authID        string
		snapshot      *Auth
		persistSnap   *Auth
		before        []CooldownStateRecord
		after         []CooldownStateRecord
		clearedModels []string
	}
	pendings := make([]pending, 0, len(matching))
	allClearedSet := make(map[string]struct{})
	trackCooldown := m.cooldownStore != nil
	for _, auth := range matching {
		var before []CooldownStateRecord
		if trackCooldown {
			before = m.cooldownStateRecordsForAuthLocked(auth, now)
		}
		var cleared []string
		if model == "" {
			// provider-wide: collect all model keys before reset
			set := make(map[string]struct{})
			for k := range auth.ModelStates {
				k = strings.TrimSpace(k)
				if k != "" {
					set[k] = struct{}{}
				}
			}
			for _, rm := range modelsForRegisteredAuth(auth.ID) {
				rm = strings.TrimSpace(rm)
				if rm != "" {
					set[rm] = struct{}{}
				}
			}
			for _, st := range auth.ModelStates {
				if st != nil {
					resetModelState(st, now)
				}
			}
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
			auth.Quota = QuotaState{}
			auth.UpdatedAt = now
			updateAggregatedAvailability(auth, now)
			for k := range set {
				cleared = append(cleared, k)
			}
			// If no models known, still ensure at least provider-level cleared (leave cleared empty but auth status cleared)
		} else {
			// provider×model: case-insensitive lookup
			var matchedKey string
			for k := range auth.ModelStates {
				if strings.EqualFold(strings.TrimSpace(k), model) {
					matchedKey = k
					break
				}
			}
			target := model
			if matchedKey != "" {
				target = matchedKey
				if st := auth.ModelStates[matchedKey]; st != nil {
					resetModelState(st, now)
				}
			}
			updateAggregatedAvailability(auth, now)
			cleared = []string{target}
		}
		if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
		auth.UpdatedAt = now
		if len(cleared) == 0 {
			// Fallback: at least include model param or registered models for registry resume
			if model != "" {
				cleared = []string{model}
			} else {
				cleared = modelsForRegisteredAuth(auth.ID)
			}
		}
		cleared = dedupeStrings(cleared)
		snap := auth.Clone()
		psnap := snap.Clone()
		var after []CooldownStateRecord
		if trackCooldown {
			after = m.cooldownStateRecordsForAuthLocked(auth, now)
		}
		pendings = append(pendings, pending{authID: auth.ID, snapshot: snap, persistSnap: psnap, before: before, after: after, clearedModels: cleared})
		for _, cm := range cleared {
			allClearedSet[cm] = struct{}{}
		}
	}
	m.mu.Unlock()
	// Persist and resume outside lock
	cooldownChanged := false
	for _, p := range pendings {
		if errPersist := m.persist(ctx, p.persistSnap); errPersist != nil {
			return nil, nil, errPersist
		}
		for _, mk := range p.clearedModels {
			registry.GetGlobalRegistry().ClearModelQuotaExceeded(p.authID, mk)
			registry.GetGlobalRegistry().ResumeClientModel(p.authID, mk)
		}
		if m.scheduler != nil && p.snapshot != nil {
			m.scheduler.upsertAuth(p.snapshot)
		}
		if !cooldownChanged && trackCooldown && !cooldownStateRecordsEqual(p.before, p.after) {
			cooldownChanged = true
		}
	}
	if cooldownChanged {
		m.persistCooldownStates(ctx)
	}
	authIDs := make([]string, 0, len(pendings))
	for _, p := range pendings {
		authIDs = append(authIDs, p.authID)
	}
	models := make([]string, 0, len(allClearedSet))
	for k := range allClearedSet {
		models = append(models, k)
	}
	models = dedupeStrings(models)
	return authIDs, models, nil
}

func errorDisposition(err error) (cliproxyexecutor.ErrorDisposition, bool) {
	if err == nil {
		return nil, false
	}
	return errors.AsType[cliproxyexecutor.ErrorDisposition](err)
}

// errorAcceptancePhase returns a trustworthy acceptance phase when the error
// carries explicit phase evidence. Unclassified legacy errors return ok=false
// so credential failover keeps its existing provider behavior until executors
// emit accurate phases.
func errorAcceptancePhase(err error) (cliproxyexecutor.AcceptancePhase, bool) {
	if err == nil {
		return cliproxyexecutor.AcceptanceNotSent, true
	}
	if carrier, ok := errors.AsType[cliproxyexecutor.AcceptancePhaseCarrier](err); ok && carrier != nil {
		phase := carrier.AcceptancePhase()
		if cliproxyexecutor.IsValidAcceptancePhase(phase) {
			return phase, true
		}
	}
	return "", false
}

func errorRetryScope(err error) cliproxyexecutor.RetryScope {
	if disposition, ok := errorDisposition(err); ok && disposition != nil {
		if scope := disposition.RetryScope(); scope == cliproxyexecutor.RetryScopeSelectedExecution {
			return scope
		}
	}
	if phase, ok := errorAcceptancePhase(err); ok {
		if scope := cliproxyexecutor.RetryScopeFromAcceptancePhase(phase); scope == cliproxyexecutor.RetryScopeSelectedExecution {
			return scope
		}
	}
	if disposition, ok := errorDisposition(err); ok && disposition != nil {
		return disposition.RetryScope()
	}
	return cliproxyexecutor.RetryScopeDefault
}

func errorAuthAttributable(err error) bool {
	if disposition, ok := errorDisposition(err); ok && disposition != nil {
		return disposition.AuthAttributable()
	}
	return true
}

// allowsCredentialFailover consults acceptance phase only when the error
// carries trustworthy phase evidence. Unclassified errors preserve legacy
// auth-attribution failover behavior.
func allowsCredentialFailover(err error) bool {
	if phase, ok := errorAcceptancePhase(err); ok {
		return cliproxyexecutor.AllowsCredentialRotation(phase, errorAuthAttributable(err))
	}
	return errorAuthAttributable(err)
}

func hasTerminalRefreshFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.LastError.Code)) {
	case "refresh_rejected":
		return true
	default:
		return false
	}
}

func capQuotaCooldown(auth *Auth, d time.Duration) time.Duration {
	if auth == nil {
		return d
	}
	if secs, ok := auth.QuotaCooldownMaxOverride(); ok {
		maxDur := time.Duration(secs) * time.Second
		if d > maxDur {
			return maxDur
		}
	}
	return d
}

func modelSuspendDisabledForAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Metadata == nil {
		return false
	}
	if val, ok := auth.Metadata["disable_model_suspend"]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return false
}

// providerSuspendDisabledForAuth returns true if automatic provider suspension is disabled for this auth.
// This prevents cascade suspension when passthru routes encounter errors.
func providerSuspendDisabledForAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Metadata == nil {
		return false
	}
	if val, ok := auth.Metadata["disable_provider_suspend"]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return false
}

func isTransientStreamDisconnectResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusRequestTimeout {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(err.Message))
	return strings.Contains(lower, "stream disconnected before response.completed") ||
		strings.Contains(lower, "stream closed before response.completed")
}

// SetResultPolicy sets an execution result policy invoked before in-memory quota mutations and persistence.
func (m *Manager) SetResultPolicy(policy ResultPolicy) {
	if m == nil {
		return
	}
	if policy == nil {
		m.resultPolicy.Store(nil)
		return
	}
	m.resultPolicy.Store(&resultPolicyHolder{policy: policy})
}

// ResultPolicy returns the current execution result policy, or nil if none is configured.
func (m *Manager) ResultPolicy() ResultPolicy {
	if m == nil {
		return nil
	}
	holder := m.resultPolicy.Load()
	if holder == nil {
		return nil
	}
	return holder.policy
}
