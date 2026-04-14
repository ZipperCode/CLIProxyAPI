package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestClassifyQuotaProbeResult_SuccessMeansRecoverable(t *testing.T) {
	result := ClassifyQuotaProbeResult(200, `{"ok":true}`)
	if result != QuotaProbeRecovered {
		t.Fatalf("got %v", result)
	}
}

func TestClassifyQuotaProbeResult_QuotaStillExceeded(t *testing.T) {
	result := ClassifyQuotaProbeResult(429, `{"error":{"message":"usage_limit_reached"}}`)
	if result != QuotaProbeStillLimited {
		t.Fatalf("got %v", result)
	}
}

func TestClassifyQuotaProbeResult_UnknownForServerError(t *testing.T) {
	result := ClassifyQuotaProbeResult(500, `{"error":"server_error"}`)
	if result != QuotaProbeUnknown {
		t.Fatalf("got %v", result)
	}
}

func TestClassifyQuotaProbeResult_UnknownForBadRequest(t *testing.T) {
	result := ClassifyQuotaProbeResult(400, `{"error":"bad_request"}`)
	if result != QuotaProbeUnknown {
		t.Fatalf("got %v", result)
	}
}

func TestClassifyQuotaProbeResult_UnknownWhenQuotaSignalMissing(t *testing.T) {
	result := ClassifyQuotaProbeResult(429, `{"error":{"message":"rate_limit"}}`)
	if result != QuotaProbeUnknown {
		t.Fatalf("got %v", result)
	}
}

func TestQuotaAutoDisableScanner_OnlyProcessesDueSystemDisabledAuths(t *testing.T) {
	manager := newTestManagerWithAuths(t,
		autoDisabledDueAuth(),
		autoDisabledNotDueAuth(),
		manuallyDisabledAuth(),
	)
	prober := &fakeQuotaProbeExecutor{}
	cfg := testQuotaAutoDisableConfig()

	scanner := NewQuotaAutoDisableScanner(manager, prober, cfg)
	scanner.runOnce(context.Background())

	if got := prober.Calls(); got != 1 {
		t.Fatalf("expected 1 probe, got %d", got)
	}
}

func TestQuotaAutoDisableScanner_RecoveredAuthEnabledImmediately(t *testing.T) {
	auth := autoDisabledDueAuth()
	manager := newTestManagerWithAuths(t, auth)
	prober := &fakeQuotaProbeExecutor{result: QuotaProbeRecovered}
	cfg := testQuotaAutoDisableConfig()

	scanner := NewQuotaAutoDisableScanner(manager, prober, cfg)
	scanner.runOnce(context.Background())

	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("expected auth enabled")
	}
	if IsQuotaAutoDisabled(updated) {
		t.Fatal("expected metadata cleared")
	}
}

func TestQuotaAutoDisableScanner_StillLimitedSchedulesRetry(t *testing.T) {
	auth := autoDisabledDueAuth()
	manager := newTestManagerWithAuths(t, auth)
	prober := &fakeQuotaProbeExecutor{result: QuotaProbeStillLimited}
	cfg := testQuotaAutoDisableConfig()

	start := time.Now().UTC()
	scanner := NewQuotaAutoDisableScanner(manager, prober, cfg)
	scanner.runOnce(context.Background())
	updated, _ := manager.GetByID(auth.ID)

	if !updated.Disabled {
		t.Fatal("expected auth still disabled")
	}
	if !IsQuotaAutoDisabled(updated) {
		t.Fatal("expected auto disable metadata")
	}
	state, ok := GetQuotaAutoDisableState(updated)
	if !ok {
		t.Fatal("expected auto disable state")
	}
	if state.LastCheckedAt.IsZero() {
		t.Fatal("expected last checked updated")
	}
	if state.LastResult != "quota_still_limited" {
		t.Fatalf("expected last result updated, got %q", state.LastResult)
	}
	retryInterval := time.Duration(cfg.RetryIntervalSeconds) * time.Second
	minNext := start.Add(retryInterval)
	maxNext := time.Now().UTC().Add(retryInterval + 2*time.Second)
	if state.NextCheckAt.Before(minNext) || state.NextCheckAt.After(maxNext) {
		t.Fatalf("expected next check within window, got %v", state.NextCheckAt)
	}
}

type fakeQuotaProbeExecutor struct {
	result QuotaProbeResult
	err    error
	calls  int
}

func (f *fakeQuotaProbeExecutor) ProbeAuth(context.Context, *Auth) (int, string, error) {
	f.calls++
	if f.err != nil {
		return 0, "", f.err
	}
	switch f.result {
	case QuotaProbeRecovered:
		return 200, `{"ok":true}`, nil
	case QuotaProbeStillLimited:
		return 429, `{"error":{"message":"usage_limit_reached"}}`, nil
	default:
		return 500, `{"error":"server_error"}`, nil
	}
}

func (f *fakeQuotaProbeExecutor) Calls() int {
	return f.calls
}

func newTestManagerWithAuths(t *testing.T, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	for i, auth := range auths {
		if auth == nil {
			continue
		}
		if auth.ID == "" {
			auth.ID = "auth-" + strconv.Itoa(i)
		}
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
	}
	return manager
}

func autoDisabledDueAuth() *Auth {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auth-due",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{},
	}
	SetQuotaAutoDisableState(auth, QuotaAutoDisableState{
		Reason:        "quota_exhausted",
		DisabledAt:    now.Add(-2 * time.Hour),
		NextCheckAt:   now.Add(-1 * time.Minute),
		LastResult:    "quota_exhausted",
		ProbeProvider: "codex",
		SystemManaged: true,
	})
	return auth
}

func autoDisabledNotDueAuth() *Auth {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auth-not-due",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{},
	}
	SetQuotaAutoDisableState(auth, QuotaAutoDisableState{
		Reason:        "quota_exhausted",
		DisabledAt:    now.Add(-2 * time.Hour),
		NextCheckAt:   now.Add(2 * time.Hour),
		LastResult:    "quota_exhausted",
		ProbeProvider: "codex",
		SystemManaged: true,
	})
	return auth
}

func manuallyDisabledAuth() *Auth {
	return &Auth{
		ID:       "auth-manual",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
	}
}

func testQuotaAutoDisableConfig() internalconfig.AuthQuotaAutoDisableConfig {
	return internalconfig.AuthQuotaAutoDisableConfig{
		Enabled:              true,
		ScanIntervalSeconds:  10,
		RetryIntervalSeconds: 120,
		MaxConcurrentProbes:  1,
		Providers:            []string{"codex"},
	}
}
