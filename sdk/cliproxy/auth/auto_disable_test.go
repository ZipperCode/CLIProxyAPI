package auth

import (
	"testing"
	"time"
)

func TestQuotaAutoDisableMetadataRoundTrip(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{}}
	now := time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC)
	next := now.Add(6 * time.Hour)

	SetQuotaAutoDisableState(auth, QuotaAutoDisableState{
		Reason:        "quota_exhausted",
		DisabledAt:    now,
		NextCheckAt:   next,
		LastResult:    "quota_exhausted",
		ProbeProvider: "codex",
		SystemManaged: true,
	})

	state, ok := GetQuotaAutoDisableState(auth)
	if !ok {
		t.Fatal("expected state")
	}
	if state.Reason != "quota_exhausted" || !state.NextCheckAt.Equal(next) {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestClearQuotaAutoDisableStateRemovesMarkers(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		"auto_disabled_reason": "quota_exhausted",
	}}
	ClearQuotaAutoDisableState(auth)
	if _, ok := auth.Metadata["auto_disabled_reason"]; ok {
		t.Fatal("expected metadata key removed")
	}
}
