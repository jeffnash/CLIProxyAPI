package cliproxy

import (
	"context"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func codeBuddyServiceAuth(id string, realm codebuddy.Realm, uid string, models ...codebuddy.Model) *coreauth.Auth {
	tools := true
	reasoning := true
	for i := range models {
		if models[i].SupportsTools == nil {
			models[i].SupportsTools = &tools
		}
		if models[i].SupportsReasoning == nil {
			models[i].SupportsReasoning = &reasoning
			models[i].Efforts = []string{"high"}
		}
	}
	creds := codebuddy.Credentials{
		Realm: realm, AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		UID: uid, Expired: time.Now().Add(time.Hour),
	}
	catalog := codebuddy.Catalog{Models: models, Sources: []string{"/v3/config"}}
	return &coreauth.Auth{ID: id, Provider: "codebuddy", Metadata: codebuddy.Metadata(creds, catalog, time.Now())}
}

func codeBuddyService(t *testing.T, cfg *config.Config) (*Service, *coreauth.Manager) {
	t.Helper()
	mgr := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: mgr, cfg: cfg}
	t.Cleanup(func() {
		for _, id := range []string{"codebuddy-svc-a", "codebuddy-svc-b", "codebuddy-svc-alias", "codebuddy-svc-disable"} {
			GlobalModelRegistry().UnregisterClient(id)
		}
	})
	return service, mgr
}

// codeBuddyRegister mirrors production flow: the auth must be known to the
// conductor manager before model registration (the manager-membership gate in
// registerModelsForAuthWithCache skips unknown IDs).
func codeBuddyRegister(t *testing.T, service *Service, mgr *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	service.registerModelsForAuth(context.Background(), auth)
}

func TestServiceCodeBuddyExecutorBinding(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	foundBaseline := false
	for _, auth := range baselineExecutorAuths() {
		if auth.Provider == "codebuddy" {
			foundBaseline = true
		}
	}
	if !foundBaseline {
		t.Fatal("codebuddy missing from baseline executors")
	}
	service.registerExecutorForAuth(codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmGlobal, "u", codebuddy.Model{ID: "hy4-preview"}), false)
	got, ok := mgr.Executor("codebuddy")
	if !ok {
		t.Fatal("codebuddy executor not bound")
	}
	if _, ok := got.(*executor.CodeBuddyExecutor); !ok {
		t.Fatalf("executor = %T", got)
	}
}

func TestServiceCodeBuddyRegistration(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	auth := codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmGlobal, "user-1", codebuddy.Model{ID: "hy4-preview", Name: "HY4"})
	codeBuddyRegister(t, service, mgr, auth)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, "codebuddy-global-hy4-preview") {
		t.Fatal("model not registered for client")
	}
	infos := reg.GetModelsForClient(auth.ID)
	if len(infos) != 1 || infos[0].UpstreamID != "hy4-preview" || infos[0].Type != "codebuddy" {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestServiceCodeBuddyRealmIsolation(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	cn := codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmCN, "same-user", codebuddy.Model{ID: "hy4-preview"})
	global := codeBuddyServiceAuth("codebuddy-svc-b", codebuddy.RealmGlobal, "same-user", codebuddy.Model{ID: "hy4-preview"})
	codeBuddyRegister(t, service, mgr, cn)
	codeBuddyRegister(t, service, mgr, global)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(cn.ID, "codebuddy-cn-hy4-preview") || reg.ClientSupportsModel(cn.ID, "codebuddy-global-hy4-preview") {
		t.Fatal("CN client model set wrong")
	}
	if !reg.ClientSupportsModel(global.ID, "codebuddy-global-hy4-preview") || reg.ClientSupportsModel(global.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("global client model set wrong")
	}
}

func TestServiceCodeBuddyDisjointCatalogs(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	a := codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmCN, "user-a", codebuddy.Model{ID: "hy4-preview"})
	b := codeBuddyServiceAuth("codebuddy-svc-b", codebuddy.RealmCN, "user-b", codebuddy.Model{ID: "hy4-preview-f"})
	codeBuddyRegister(t, service, mgr, a)
	codeBuddyRegister(t, service, mgr, b)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(a.ID, "codebuddy-cn-hy4-preview") || reg.ClientSupportsModel(a.ID, "codebuddy-cn-hy4-preview-f") {
		t.Fatal("client A model set wrong")
	}
	if !reg.ClientSupportsModel(b.ID, "codebuddy-cn-hy4-preview-f") || reg.ClientSupportsModel(b.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("client B model set wrong")
	}
}

func TestServiceCodeBuddyAlias(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"codebuddy": {{Name: "codebuddy-global-hy4-preview", Alias: "hy4", Fork: true}},
	}}
	service, mgr := codeBuddyService(t, cfg)
	auth := codeBuddyServiceAuth("codebuddy-svc-alias", codebuddy.RealmGlobal, "user-alias", codebuddy.Model{ID: "hy4-preview"})
	codeBuddyRegister(t, service, mgr, auth)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, "hy4") || !reg.ClientSupportsModel(auth.ID, "codebuddy-global-hy4-preview") {
		t.Fatalf("alias not registered: %+v", reg.GetModelsForClient(auth.ID))
	}
	for _, info := range reg.GetModelsForClient(auth.ID) {
		if info.UpstreamID != "hy4-preview" {
			t.Fatalf("alias lost upstream id: %+v", info)
		}
	}
}

func TestServiceCodeBuddyDisableAndEmptySnapshot(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	auth := codeBuddyServiceAuth("codebuddy-svc-disable", codebuddy.RealmCN, "user-d", codebuddy.Model{ID: "hy4-preview"})
	codeBuddyRegister(t, service, mgr, auth)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("model not registered")
	}
	// A successful empty snapshot removes old models.
	empty := codeBuddyServiceAuth("codebuddy-svc-disable", codebuddy.RealmCN, "user-d")
	codeBuddyRegister(t, service, mgr, empty)
	if reg.ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("stale model survived empty snapshot")
	}
	// Disabling removes the registration as well.
	codeBuddyRegister(t, service, mgr, auth)
	disabled := codeBuddyServiceAuth("codebuddy-svc-disable", codebuddy.RealmCN, "user-d", codebuddy.Model{ID: "hy4-preview"})
	disabled.Disabled = true
	codeBuddyRegister(t, service, mgr, disabled)
	if reg.ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("disabled auth still registered")
	}
}

func TestServiceCodeBuddyInvalidSnapshot(t *testing.T) {
	service, mgr := codeBuddyService(t, &config.Config{})
	auth := codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmCN, "user-x", codebuddy.Model{ID: "hy4-preview"})
	codeBuddyRegister(t, service, mgr, auth)
	broken := &coreauth.Auth{ID: "codebuddy-svc-a", Provider: "codebuddy", Metadata: map[string]any{"type": "codebuddy"}}
	codeBuddyRegister(t, service, mgr, broken)
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview") {
		t.Fatal("stale model survived invalid snapshot")
	}
}

func TestServiceCodeBuddyExclusions(t *testing.T) {
	cfg := &config.Config{OAuthExcludedModels: map[string][]string{
		"codebuddy": {"codebuddy-cn-hy4-preview-x"},
	}}
	service, mgr := codeBuddyService(t, cfg)
	auth := codeBuddyServiceAuth("codebuddy-svc-a", codebuddy.RealmCN, "user-e",
		codebuddy.Model{ID: "hy4-preview"}, codebuddy.Model{ID: "hy4-preview-x"})
	codeBuddyRegister(t, service, mgr, auth)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview") || reg.ClientSupportsModel(auth.ID, "codebuddy-cn-hy4-preview-x") {
		t.Fatal("exclusion not applied")
	}
}
