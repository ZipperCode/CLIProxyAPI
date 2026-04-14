package auth

import (
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

func TestApplyAuthFailureState_UnsupportedProviderSkipsQuotaAutoDisable(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "codex-b",
		Provider: "unsupported",
		Metadata: map[string]any{},
	}
	errInfo := &Error{HTTPStatus: 429, Message: "quota exhausted"}
	cfg := &internalconfig.Config{}
	cfg.AuthQuotaAutoDisable = internalconfig.AuthQuotaAutoDisableConfig{
		Enabled:            true,
		InitialWaitSeconds: 600,
		Providers:          []string{"codex"},
	}

	applyAuthFailureState(auth, errInfo, nil, now, cfg)

	if !auth.Quota.Exceeded {
		t.Fatal("expected quota exceeded")
	}
	if auth.Disabled {
		t.Fatal("expected auth not disabled")
	}
	if IsQuotaAutoDisabled(auth) {
		t.Fatal("expected no auto disable metadata")
	}
}
