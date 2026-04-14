package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type QuotaProbeResult int

const (
	QuotaProbeUnknown QuotaProbeResult = iota
	QuotaProbeRecovered
	QuotaProbeStillLimited
)

type QuotaProbeExecutor interface {
	ProbeAuth(ctx context.Context, auth *Auth) (statusCode int, body string, err error)
}

type QuotaAutoDisableScanner struct {
	manager *Manager
	prober  QuotaProbeExecutor
	cfg     internalconfig.AuthQuotaAutoDisableConfig
	cancel  context.CancelFunc
}

func NewQuotaAutoDisableScanner(manager *Manager, prober QuotaProbeExecutor, cfg internalconfig.AuthQuotaAutoDisableConfig) *QuotaAutoDisableScanner {
	return &QuotaAutoDisableScanner{
		manager: manager,
		prober:  prober,
		cfg:     cfg,
	}
}

func (s *QuotaAutoDisableScanner) Start(parent context.Context) {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	interval := time.Duration(s.cfg.ScanIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Duration(internalconfig.DefaultAuthQuotaAutoDisableScanIntervalSeconds) * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.runOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

func (s *QuotaAutoDisableScanner) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.cancel = nil
}

func (s *QuotaAutoDisableScanner) runOnce(ctx context.Context) {
	if s == nil || s.manager == nil || s.prober == nil {
		return
	}
	if !s.cfg.Enabled {
		return
	}
	now := time.Now().UTC()
	auths := s.manager.List()
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !IsQuotaAutoDisabled(auth) {
			continue
		}
		state, ok := GetQuotaAutoDisableState(auth)
		if !ok || !state.SystemManaged {
			continue
		}
		if !isQuotaProbeDue(state.NextCheckAt, now) {
			continue
		}
		statusCode, body, err := s.prober.ProbeAuth(ctx, auth)
		result := QuotaProbeUnknown
		if err == nil {
			result = ClassifyQuotaProbeResult(statusCode, body)
		}
		switch result {
		case QuotaProbeRecovered:
			auth.Disabled = false
			auth.Status = StatusActive
			auth.StatusMessage = ""
			ClearQuotaAutoDisableState(auth)
		case QuotaProbeStillLimited:
			updateQuotaAutoDisableState(auth, now, now.Add(retryInterval(s.cfg)), "quota_still_limited")
		default:
			updateQuotaAutoDisableState(auth, now, now.Add(retryInterval(s.cfg)), "probe_failed")
		}
		_, _ = s.manager.Update(ctx, auth)
	}
}

func ClassifyQuotaProbeResult(statusCode int, body string) QuotaProbeResult {
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		return QuotaProbeRecovered
	}
	if statusCode == http.StatusTooManyRequests {
		normalized := strings.ToLower(body)
		if strings.Contains(normalized, "usage_limit_reached") {
			return QuotaProbeStillLimited
		}
	}
	return QuotaProbeUnknown
}

func isQuotaProbeDue(nextCheckAt time.Time, now time.Time) bool {
	if nextCheckAt.IsZero() {
		return true
	}
	return !nextCheckAt.After(now)
}

func retryInterval(cfg internalconfig.AuthQuotaAutoDisableConfig) time.Duration {
	retrySeconds := cfg.RetryIntervalSeconds
	if retrySeconds <= 0 {
		retrySeconds = internalconfig.DefaultAuthQuotaAutoDisableRetryIntervalSeconds
	}
	return time.Duration(retrySeconds) * time.Second
}

func updateQuotaAutoDisableState(auth *Auth, now time.Time, nextCheck time.Time, lastResult string) {
	if auth == nil {
		return
	}
	state, ok := GetQuotaAutoDisableState(auth)
	if !ok {
		return
	}
	state.LastCheckedAt = now
	state.NextCheckAt = nextCheck
	state.LastResult = lastResult
	SetQuotaAutoDisableState(auth, state)
}
