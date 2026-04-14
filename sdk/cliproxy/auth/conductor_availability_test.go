package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestUpdateAggregatedAvailability_UnavailableWithoutNextRetryDoesNotBlockAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if auth.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = %v, want zero", auth.NextRetryAfter)
	}
}

func TestUpdateAggregatedAvailability_FutureNextRetryBlocksAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	next := now.Add(5 * time.Minute)
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = zero, want %v", next)
	}
	if auth.NextRetryAfter.Sub(next) > time.Second || next.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, next)
	}
}

func TestManager_MarkResult_UnsupportedProviderSkipsQuotaAutoDisable(t *testing.T) {
	store := &captureStore{}
	m := NewManager(store, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.AuthQuotaAutoDisable = internalconfig.AuthQuotaAutoDisableConfig{
		Enabled:            true,
		InitialWaitSeconds: 600,
		Providers:          []string{"codex"},
	}
	m.SetConfig(cfg)

	if _, errRegister := m.Register(context.Background(), &Auth{
		ID:       "codex-b",
		Provider: "unsupported",
		Metadata: map[string]any{},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:   "codex-b",
		Provider: "unsupported",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota exhausted"},
	})

	updated, ok := m.GetByID("codex-b")
	if !ok || updated == nil {
		t.Fatal("expected auth updated")
	}
	if !updated.Quota.Exceeded {
		t.Fatal("expected quota exceeded")
	}
	if updated.Disabled {
		t.Fatal("expected auth not disabled")
	}
	if IsQuotaAutoDisabled(updated) {
		t.Fatal("expected no auto disable metadata")
	}
}
