package auth

import (
	"context"
	"net/http"
	"strings"
)

type QuotaProbeResult int

const (
	quotaProbeUnknown QuotaProbeResult = iota
	quotaProbeRecovered
	quotaProbeStillLimited
)

type QuotaProbeExecutor interface {
	ProbeAuth(ctx context.Context, auth *Auth) (statusCode int, body string, err error)
}

func classifyQuotaProbeResult(statusCode int, body string) QuotaProbeResult {
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		return quotaProbeRecovered
	}
	if statusCode == http.StatusTooManyRequests {
		normalized := strings.ToLower(body)
		if strings.Contains(normalized, "usage_limit_reached") {
			return quotaProbeStillLimited
		}
	}
	return quotaProbeUnknown
}
