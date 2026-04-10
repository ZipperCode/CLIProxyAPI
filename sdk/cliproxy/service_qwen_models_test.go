package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestServiceRegisterModelsForAuth_PrefersDynamicQwenModels(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "qwen-auth-dynamic",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"qwen_models": []map[string]any{
				{"id": "qwen-dyn-only", "name": "Qwen Dyn Only"},
			},
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 2 {
		t.Fatalf("expected dynamic model plus static coder-model, got %d", len(models))
	}
	foundDynamic := false
	foundCoder := false
	for _, m := range models {
		if m == nil {
			continue
		}
		if m.ID == "qwen-dyn-only" && m.DisplayName == "Qwen Dyn Only" {
			foundDynamic = true
		}
		if m.ID == "coder-model" {
			foundCoder = true
		}
	}
	if !foundDynamic {
		t.Fatalf("expected dynamic qwen model to remain present, got %#v", models)
	}
	if !foundCoder {
		t.Fatalf("expected static coder-model to be preserved, got %#v", models)
	}
}

func TestServiceRegisterModelsForAuth_QwenFallsBackToStaticModelsWhenDynamicMissing(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "qwen-auth-static",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{},
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	static := registry.GetQwenModels()
	if len(static) == 0 {
		t.Fatal("expected static qwen models to be non-empty for fallback")
	}
	wantID := static[0].ID

	models := reg.GetModelsForClient(auth.ID)
	if len(models) == 0 {
		t.Fatal("expected fallback qwen models to be registered")
	}
	found := false
	for _, m := range models {
		if m != nil && m.ID == wantID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fallback to include static model %q, got %#v", wantID, models)
	}
}

func TestServiceRegisterModelsForAuth_QwenDoesNotPerformV2ModelSync(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "qwen-auth-no-v2-sync",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  srv.URL,
		},
		Metadata: map[string]any{
			"token_cookie": "legacy-cookie",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("expected no qwen v2 sync request, got %d", hits)
	}
}
