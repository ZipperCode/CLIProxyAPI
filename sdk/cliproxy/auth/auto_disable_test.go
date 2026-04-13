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
		metadataKeyAutoDisabledReason: "quota_exhausted",
	}}
	ClearQuotaAutoDisableState(auth)
	if _, ok := auth.Metadata["auto_disabled_reason"]; ok {
		t.Fatal("expected metadata key removed")
	}
}

func TestQuotaAutoDisableStateInvalidMetadata(t *testing.T) {
	cases := []map[string]any{
		{"auto_disabled_reason": ""},
		{"auto_disabled_reason": 42},
	}
	for idx, meta := range cases {
		auth := &Auth{Metadata: meta}
		if _, ok := GetQuotaAutoDisableState(auth); ok {
			t.Fatalf("case %d: expected invalid metadata to be rejected, got ok", idx)
		}
	}
}

func TestClearQuotaAutoDisableStatePreservesOtherMetadata(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		metadataKeyAutoDisabledReason:        "quota_exhausted",
		metadataKeyAutoDisabledAt:            "2026-01-01T00:00:00Z",
		metadataKeyAutoRecoveryLastCheckedAt: "2026-01-01T01:00:00Z",
		metadataKeyAutoRecoveryLastResult:    "quota_exhausted",
		"custom_key":                         "keep",
	}}
	ClearQuotaAutoDisableState(auth)
	for _, key := range []string{
		metadataKeyAutoDisabledReason,
		metadataKeyAutoDisabledAt,
		metadataKeyAutoRecoveryLastCheckedAt,
		metadataKeyAutoRecoveryLastResult,
		metadataKeyAutoRecoveryNextCheckAt,
		metadataKeyAutoRecoveryProbeProvider,
		metadataKeyAutoRecoverySystemManaged,
	} {
		if _, ok := auth.Metadata[key]; ok {
			t.Fatalf("expected %q key removed", key)
		}
	}
	if val, ok := auth.Metadata["custom_key"]; !ok || val != "keep" {
		t.Fatalf("expected custom key preserved, got %#+v", auth.Metadata["custom_key"])
	}
}

func TestIsQuotaAutoDisabledRequiresReasonAndDisabled(t *testing.T) {
	cases := []struct {
		auth     *Auth
		expected bool
	}{
		{auth: &Auth{Disabled: true, Metadata: map[string]any{}}, expected: false},
		{auth: &Auth{Disabled: true, Metadata: map[string]any{metadataKeyAutoDisabledReason: ""}}, expected: false},
		{auth: &Auth{Disabled: false, Metadata: map[string]any{metadataKeyAutoDisabledReason: "quota_exhausted"}}, expected: false},
		{auth: &Auth{Disabled: true, Metadata: map[string]any{metadataKeyAutoDisabledReason: "quota_exhausted"}}, expected: true},
	}
	for idx, test := range cases {
		if got := IsQuotaAutoDisabled(test.auth); got != test.expected {
			t.Fatalf("case %d: expected %v, got %v", idx, test.expected, got)
		}
	}
}

func TestClearQuotaAutoDisableStateResetsMetadata(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		metadataKeyAutoDisabledReason:        "quota_exhausted",
		metadataKeyAutoDisabledAt:            "2026-01-01T00:00:00Z",
		metadataKeyAutoRecoveryLastCheckedAt: "2026-01-01T01:00:00Z",
	}}
	ClearQuotaAutoDisableState(auth)
	if auth.Metadata != nil {
		t.Fatalf("expected metadata nil after clearing auto-disable keys, got %#v", auth.Metadata)
	}
}
