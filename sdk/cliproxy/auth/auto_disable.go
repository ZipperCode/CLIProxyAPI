package auth

import "time"

type QuotaAutoDisableState struct {
	Reason        string
	DisabledAt    time.Time
	LastCheckedAt time.Time
	NextCheckAt   time.Time
	LastResult    string
	ProbeProvider string
	SystemManaged bool
}

// GetQuotaAutoDisableState reads stored auto-disable metadata markers.
// It only inspects stored metadata and does not imply the auth is currently disabled.
func GetQuotaAutoDisableState(auth *Auth) (QuotaAutoDisableState, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return QuotaAutoDisableState{}, false
	}
	meta := auth.Metadata
	reason, ok := meta[metadataKeyAutoDisabledReason].(string)
	if !ok || reason == "" {
		return QuotaAutoDisableState{}, false
	}
	state := QuotaAutoDisableState{
		Reason: reason,
	}
	if ts, ok := parseTimeValue(meta[metadataKeyAutoDisabledAt]); ok {
		state.DisabledAt = ts
	}
	if ts, ok := parseTimeValue(meta[metadataKeyAutoRecoveryLastCheckedAt]); ok {
		state.LastCheckedAt = ts
	}
	if ts, ok := parseTimeValue(meta[metadataKeyAutoRecoveryNextCheckAt]); ok {
		state.NextCheckAt = ts
	}
	if v, ok := meta[metadataKeyAutoRecoveryLastResult].(string); ok {
		state.LastResult = v
	}
	if v, ok := meta[metadataKeyAutoRecoveryProbeProvider].(string); ok {
		state.ProbeProvider = v
	}
	if v, ok := meta[metadataKeyAutoRecoverySystemManaged].(bool); ok {
		state.SystemManaged = v
	}
	return state, true
}

func SetQuotaAutoDisableState(auth *Auth, state QuotaAutoDisableState) {
	if auth == nil {
		return
	}
	if state.Reason == "" {
		ClearQuotaAutoDisableState(auth)
		return
	}
	meta := auth.Metadata
	if meta == nil {
		meta = make(map[string]any)
		auth.Metadata = meta
	}
	setStringMetadata(meta, metadataKeyAutoDisabledReason, state.Reason)
	setTimeMetadata(meta, metadataKeyAutoDisabledAt, state.DisabledAt)
	setTimeMetadata(meta, metadataKeyAutoRecoveryLastCheckedAt, state.LastCheckedAt)
	setTimeMetadata(meta, metadataKeyAutoRecoveryNextCheckAt, state.NextCheckAt)
	setStringMetadata(meta, metadataKeyAutoRecoveryLastResult, state.LastResult)
	setStringMetadata(meta, metadataKeyAutoRecoveryProbeProvider, state.ProbeProvider)
	if state.SystemManaged {
		meta[metadataKeyAutoRecoverySystemManaged] = state.SystemManaged
	} else {
		delete(meta, metadataKeyAutoRecoverySystemManaged)
	}
}

func ClearQuotaAutoDisableState(auth *Auth) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	keys := []string{
		metadataKeyAutoDisabledReason,
		metadataKeyAutoDisabledAt,
		metadataKeyAutoRecoveryLastCheckedAt,
		metadataKeyAutoRecoveryNextCheckAt,
		metadataKeyAutoRecoveryLastResult,
		metadataKeyAutoRecoveryProbeProvider,
		metadataKeyAutoRecoverySystemManaged,
	}
	for _, key := range keys {
		delete(auth.Metadata, key)
	}
	if len(auth.Metadata) == 0 {
		auth.Metadata = nil
	}
}

// IsQuotaAutoDisabled reports whether the auth is currently marked as auto-disabled.
// It requires the runtime Disabled flag plus the stored reason metadata, unlike GetQuotaAutoDisableState.
func IsQuotaAutoDisabled(auth *Auth) bool {
	if auth == nil || !auth.Disabled || len(auth.Metadata) == 0 {
		return false
	}
	if reason, ok := auth.Metadata[metadataKeyAutoDisabledReason].(string); !ok || reason == "" {
		return false
	}
	return true
}

func setTimeMetadata(meta map[string]any, key string, value time.Time) {
	if value.IsZero() {
		delete(meta, key)
		return
	}
	meta[key] = value.Format(time.RFC3339Nano)
}

func setStringMetadata(meta map[string]any, key, value string) {
	if value == "" {
		delete(meta, key)
		return
	}
	meta[key] = value
}
