package auth

import "testing"

func TestClassifyQuotaProbeResult_SuccessMeansRecoverable(t *testing.T) {
	result := classifyQuotaProbeResult(200, `{"ok":true}`)
	if result != quotaProbeRecovered {
		t.Fatalf("got %v", result)
	}
}

func TestClassifyQuotaProbeResult_QuotaStillExceeded(t *testing.T) {
	result := classifyQuotaProbeResult(429, `{"error":{"message":"usage_limit_reached"}}`)
	if result != quotaProbeStillLimited {
		t.Fatalf("got %v", result)
	}
}
