package cliproxy

import (
	"context"
	"testing"
)

func TestService_StartsQuotaAutoDisableScannerWhenEnabled(t *testing.T) {
	service := newTestServiceWithConfig(enabledQuotaAutoDisableConfig())
	service.StartBackgroundLoops(context.Background())
	if service.quotaAutoDisableScanner == nil {
		t.Fatal("expected scanner")
	}
}

func TestService_StopsQuotaAutoDisableScannerOnShutdown(t *testing.T) {
	service := newTestServiceWithConfig(enabledQuotaAutoDisableConfig())
	service.StartBackgroundLoops(context.Background())
	if service.quotaAutoDisableScanner == nil {
		t.Fatal("expected scanner")
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}
	if service.quotaAutoDisableScanner != nil {
		t.Fatal("expected scanner to stop")
	}
}
