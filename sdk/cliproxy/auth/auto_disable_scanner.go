package auth

import (
	"context"
	"net/http"
	"strings"
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
