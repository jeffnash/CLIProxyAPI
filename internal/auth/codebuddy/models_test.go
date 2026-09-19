package codebuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func readCatalogFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func catalogTestCreds(realm Realm) Credentials {
	return Credentials{
		Realm: realm, AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		UID: "fixture-user", MachineID: "m", SessionID: "s",
		Expired: testFixedTime.AddDate(1, 0, 0),
	}
}

func catalogRouter(t *testing.T, routes map[string]roundTripFunc, seen *[]*http.Request) roundTripFunc {
	t.Helper()
	var mu sync.Mutex
	return func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		*seen = append(*seen, req)
		mu.Unlock()
		if auth := req.Header.Get("Authorization"); auth != "Bearer SYNTHETIC_ACCESS" {
			t.Fatalf("%s: Authorization = %q", req.URL.Path, auth)
		}
		if req.Header.Get("X-Refresh-Token") != "" {
			t.Fatalf("%s: catalog carries refresh token", req.URL.Path)
		}
		handler, ok := routes[req.URL.Host+req.URL.Path]
		if !ok {
			t.Fatalf("unexpected catalog request %s", req.URL.String())
			return nil, nil
		}
		return handler(req)
	}
}

func modelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestFetchCatalogCN(t *testing.T) {
	v3 := readCatalogFixture(t, "catalog_v3.json")
	enterprise := readCatalogFixture(t, "catalog_cn_enterprise.json")
	var seen []*http.Request
	client := testClient(catalogRouter(t, map[string]roundTripFunc{
		"copilot.tencent.com/v3/config": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, v3), nil
		},
		"copilot.tencent.com/console/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, enterprise), nil
		},
	}, &seen))
	catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmCN))
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if catalog.Degraded {
		t.Fatal("full success marked degraded")
	}
	got := modelIDs(catalog.Models)
	want := []string{"hy4-preview-f", "hy4-preview"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v want %v", got, want)
	}
	// v3 fields win over the enterprise supplement for shared IDs.
	if catalog.Models[1].Name != "V3 authoritative name" || catalog.Models[1].MaxInputTokens != 200000 {
		t.Fatalf("hy4-preview = %+v", catalog.Models[1])
	}
	// hy4-not-entitled is absent from the cli agent list; other-model is not HY4.
	for _, id := range got {
		if id == "hy4-not-entitled" || id == "other-model" || id == "hy4-preview-x" || id == "hy4-disabled" {
			t.Fatalf("unexpected model %q in %v", id, got)
		}
	}
	if strings.Join(catalog.Sources, ",") != "/v3/config,/console/enterprises/personal/models" {
		t.Fatalf("sources = %v", catalog.Sources)
	}
}

func TestFetchCatalogGlobal(t *testing.T) {
	v3 := readCatalogFixture(t, "catalog_v3.json")
	enterprise := readCatalogFixture(t, "catalog_global_enterprise.json")
	var seen []*http.Request
	client := testClient(catalogRouter(t, map[string]roundTripFunc{
		"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, v3), nil
		},
		"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, enterprise), nil
		},
	}, &seen))
	catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	got := modelIDs(catalog.Models)
	if strings.Join(got, ",") != "hy4-preview-f,hy4-preview" {
		t.Fatalf("models = %v", got)
	}
	for _, req := range seen {
		if req.URL.Host != "www.codebuddy.ai" {
			t.Fatalf("host = %s", req.URL.Host)
		}
	}
}

func TestCatalogMerge(t *testing.T) {
	t.Run("enterprise adds absent ids only", func(t *testing.T) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-preview","name":"V3"}]}}`), nil
			},
			"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-preview","name":"Enterprise"},{"id":"hy4-extra"}]}}`), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
		if err != nil {
			t.Fatalf("FetchCatalog: %v", err)
		}
		if got := modelIDs(catalog.Models); strings.Join(got, ",") != "hy4-preview,hy4-extra" {
			t.Fatalf("models = %v", got)
		}
		if catalog.Models[0].Name != "V3" {
			t.Fatalf("v3 not authoritative: %+v", catalog.Models[0])
		}
	})
	t.Run("v3 tombstone blocks resurrection", func(t *testing.T) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-dead","disabled":true}]}}`), nil
			},
			"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-dead","name":"Resurrected"}]}}`), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
		if err != nil {
			t.Fatalf("FetchCatalog: %v", err)
		}
		if len(catalog.Models) != 0 {
			t.Fatalf("resurrected: %+v", catalog.Models)
		}
	})
	t.Run("degraded on single failure", func(t *testing.T) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(500, `{"code":500,"msg":"boom","data":null}`), nil
			},
			"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
		if err != nil {
			t.Fatalf("FetchCatalog: %v", err)
		}
		if !catalog.Degraded || len(catalog.Sources) != 1 || catalog.Sources[0] != "/v2/enterprises/personal/models" {
			t.Fatalf("catalog = %+v", catalog)
		}
	})
	t.Run("double failure errors", func(t *testing.T) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(500, `x`), nil
			},
			"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(500, `x`), nil
			},
		}, &seen))
		if _, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal)); err == nil {
			t.Fatal("double failure accepted")
		}
	})
	t.Run("global 404 falls back to console", func(t *testing.T) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[]}}`), nil
			},
			"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(404, `not found`), nil
			},
			"www.codebuddy.ai/console/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
		if err != nil {
			t.Fatalf("FetchCatalog: %v", err)
		}
		if got := modelIDs(catalog.Models); len(got) != 1 {
			t.Fatalf("models = %v", got)
		}
		if strings.Join(catalog.Sources, ",") != "/v3/config,/console/enterprises/personal/models" {
			t.Fatalf("sources = %v", catalog.Sources)
		}
	})
	t.Run("cn has no enterprise fallback", func(t *testing.T) {
		var enterpriseCalls atomic.Int32
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"copilot.tencent.com/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[]}}`), nil
			},
			"copilot.tencent.com/console/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				enterpriseCalls.Add(1)
				return fakeJSONResponse(404, `not found`), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmCN))
		if err != nil {
			t.Fatalf("FetchCatalog: %v", err)
		}
		if !catalog.Degraded || enterpriseCalls.Load() != 1 {
			t.Fatalf("catalog = %+v calls = %d", catalog, enterpriseCalls.Load())
		}
	})
}

func TestCatalogAccountIsolation(t *testing.T) {
	newClient := func(payload string) (*Client, *[]*http.Request) {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"copilot.tencent.com/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, payload), nil
			},
			"copilot.tencent.com/console/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
			},
		}, &seen))
		return client, &seen
	}
	credsA := catalogTestCreds(RealmCN)
	credsA.UID = "user-a"
	credsB := catalogTestCreds(RealmCN)
	credsB.UID = "user-b"
	clientA, _ := newClient(`{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`)
	clientB, _ := newClient(`{"code":0,"data":{"models":[{"id":"hy4-preview-f"}]}}`)
	catalogA, err := clientA.FetchCatalog(context.Background(), credsA)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	catalogB, err := clientB.FetchCatalog(context.Background(), credsB)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if modelIDs(catalogA.Models)[0] != "hy4-preview" || modelIDs(catalogB.Models)[0] != "hy4-preview-f" {
		t.Fatalf("A=%v B=%v", modelIDs(catalogA.Models), modelIDs(catalogB.Models))
	}
	modelsA := RegistryModels(credsA, catalogA)
	modelsB := RegistryModels(credsB, catalogB)
	if modelsA[0].ID != "codebuddy-cn-hy4-preview" || modelsB[0].ID != "codebuddy-cn-hy4-preview-f" {
		t.Fatalf("A=%s B=%s", modelsA[0].ID, modelsB[0].ID)
	}
}

func TestCatalogMalformed(t *testing.T) {
	cases := map[string]struct {
		v3         string
		enterprise string
	}{
		"negative limit":    {`{"code":0,"data":{"models":[{"id":"hy4-preview","maxInputTokens":-1}]}}`, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`},
		"nonintegral limit": {`{"code":0,"data":{"models":[{"id":"hy4-preview","maxInputTokens":1.5}]}}`, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`},
		"conflicting dup":   {`{"code":0,"data":{"models":[{"id":"hy4-preview","name":"A"},{"id":"hy4-preview","name":"B"}]}}`, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`},
		"missing cli":       {`{"code":0,"data":{"models":[]}}`, `{"code":0,"data":{"models":[{"id":"hy4-preview"}],"agents":[{"name":"web","models":["hy4-preview"]}]}}`},
		"business error":    {`{"code":11140,"msg":"request illegal","data":null}`, `{"code":11140,"msg":"request illegal","data":null}`},
		"missing code":      {`{"msg":"ok","data":{}}`, `{"msg":"ok","data":{}}`},
		"empty id":          {`{"code":0,"data":{"models":[{"id":""}]}}`, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`},
	}
	for name, tc := range cases {
		var seen []*http.Request
		client := testClient(catalogRouter(t, map[string]roundTripFunc{
			"copilot.tencent.com/v3/config": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, tc.v3), nil
			},
			"copilot.tencent.com/console/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
				return fakeJSONResponse(200, tc.enterprise), nil
			},
		}, &seen))
		catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmCN))
		if name == "business error" || name == "missing code" {
			if err == nil {
				t.Fatalf("%s: double failure accepted", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !catalog.Degraded {
			t.Fatalf("%s: malformed source not degraded", name)
		}
		if len(catalog.Models) != 0 {
			t.Fatalf("%s: models = %+v", name, catalog.Models)
		}
	}
}

func TestCatalogNarrowAndIdenticalDup(t *testing.T) {
	var seen []*http.Request
	client := testClient(catalogRouter(t, map[string]roundTripFunc{
		"www.codebuddy.ai/v3/config": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"data":["hy4-preview","hy4-preview","other-model"]}`), nil
		},
		"www.codebuddy.ai/v2/enterprises/personal/models": func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"data":{"models":[{"id":"hy4-preview","name":"E","maxInputTokens":5},{"id":"hy4-preview","name":"E","maxInputTokens":5}]}}`), nil
		},
	}, &seen))
	catalog, err := client.FetchCatalog(context.Background(), catalogTestCreds(RealmGlobal))
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "hy4-preview" {
		t.Fatalf("catalog = %+v", catalog.Models)
	}
	if catalog.Models[0].Name != "" || catalog.Models[0].MaxInputTokens != 0 {
		t.Fatalf("narrow v3 must stay capability-free: %+v", catalog.Models[0])
	}
}

func TestRegistryModels(t *testing.T) {
	tools := true
	noTools := false
	reasoning := true
	creds := catalogTestCreds(RealmWorkBuddyGlobal)
	catalog := Catalog{Models: []Model{
		{ID: "hy4-preview", Name: "HY4", MaxInputTokens: 100, MaxOutputTokens: 10, SupportsTools: &tools, SupportsReasoning: &reasoning, Efforts: []string{"high"}, DefaultEffort: "high"},
		{ID: "hy4-preview-f", SupportsTools: &noTools},
		{ID: "other-model"},
	}}
	models := RegistryModels(creds, catalog)
	if len(models) != 2 {
		t.Fatalf("models = %d", len(models))
	}
	first := models[0]
	if first.ID != "codebuddy-workbuddy-global-hy4-preview" || first.UpstreamID != "hy4-preview" {
		t.Fatalf("identity = %s / %s", first.ID, first.UpstreamID)
	}
	if first.Object != "model" || first.OwnedBy != "tencent" || first.Type != "codebuddy" {
		t.Fatalf("model = %+v", first)
	}
	if first.ContextLength != 100 || first.MaxCompletionTokens != 10 || first.DisplayName != "HY4" {
		t.Fatalf("limits/display = %+v", first)
	}
	if first.Thinking == nil || len(first.Thinking.Levels) != 1 || first.Thinking.Levels[0] != "high" {
		t.Fatalf("thinking = %+v", first.Thinking)
	}
	if first.UserDefined {
		t.Fatal("UserDefined must be false")
	}
	hasTools := false
	for _, param := range first.SupportedParameters {
		if param == "tools" {
			hasTools = true
		}
	}
	if !hasTools {
		t.Fatalf("tools not advertised: %v", first.SupportedParameters)
	}
	second := models[1]
	for _, param := range second.SupportedParameters {
		if param == "tools" {
			t.Fatal("tools advertised without capability")
		}
	}
	if second.Thinking != nil {
		t.Fatalf("unknown reasoning must stay nil: %+v", second.Thinking)
	}
	for _, model := range models {
		if strings.Join(model.SupportedInputModalities, ",") != "TEXT" || strings.Join(model.SupportedOutputModalities, ",") != "TEXT" {
			t.Fatalf("modalities = %+v", model)
		}
	}
}

func TestCatalogFromMetadataValidation(t *testing.T) {
	tools := true
	valid := Catalog{Models: []Model{{ID: "hy4-preview", SupportsTools: &tools}}, Sources: []string{"s"}}
	metadata := Metadata(catalogTestCreds(RealmCN), valid, testFixedTime)
	// Simulate a JSON round trip through the file store.
	var decoded map[string]any
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := CatalogFromMetadata(decoded); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	bad := []map[string]any{
		{},
		{CatalogMetadataKey: map[string]any{"models": []any{map[string]any{"id": "other"}}}},
		{CatalogMetadataKey: map[string]any{"models": []any{map[string]any{"id": "hy4-preview"}, map[string]any{"id": "hy4-preview"}}}},
		{CatalogMetadataKey: map[string]any{"models": []any{map[string]any{"id": "hy4-preview", "max_input_tokens": -1}}}},
		{CatalogMetadataKey: map[string]any{"models": []any{map[string]any{"id": "hy4-preview", "supports_tools": "yes"}}}},
		{CatalogMetadataKey: "nope"},
	}
	for i, metadata := range bad {
		if _, err := CatalogFromMetadata(metadata); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
