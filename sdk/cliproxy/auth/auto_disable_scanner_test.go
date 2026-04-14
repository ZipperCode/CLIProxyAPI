package auth

import "testing"

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
