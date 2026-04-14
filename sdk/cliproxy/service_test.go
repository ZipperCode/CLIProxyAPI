package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type fakeQuotaAutoDisableScanner struct {
	started chan struct{}
	stopped chan struct{}
	starts  int
	stops   int
}

func newFakeQuotaAutoDisableScanner() *fakeQuotaAutoDisableScanner {
	return &fakeQuotaAutoDisableScanner{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (f *fakeQuotaAutoDisableScanner) Start(context.Context) {
	f.starts++
	select {
	case <-f.started:
	default:
		close(f.started)
	}
}

func (f *fakeQuotaAutoDisableScanner) Stop() {
	f.stops++
	select {
	case <-f.stopped:
	default:
		close(f.stopped)
	}
}

type fakeTokenProvider struct{}

func (fakeTokenProvider) Load(context.Context, *config.Config) (*TokenClientResult, error) {
	return &TokenClientResult{}, nil
}

type fakeAPIKeyProvider struct{}

func (fakeAPIKeyProvider) Load(context.Context, *config.Config) (*APIKeyClientResult, error) {
	return &APIKeyClientResult{}, nil
}

func TestService_StartsQuotaAutoDisableScannerWhenEnabled(t *testing.T) {
	service, scanner, ctx, cancel, errs := startServiceWithScanner(t)
	defer cancel()

	select {
	case <-scanner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected scanner to start during service run")
	}

	cancel()
	<-errs

	if scanner.starts != 1 {
		t.Fatalf("expected scanner to start once, got %d", scanner.starts)
	}
	_ = service
	_ = ctx
}

func TestService_StopsQuotaAutoDisableScannerOnShutdown(t *testing.T) {
	_, scanner, ctx, cancel, errs := startServiceWithScanner(t)
	defer cancel()

	select {
	case <-scanner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected scanner to start during service run")
	}

	cancel()

	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("service Run did not return after context cancellation")
	}

	select {
	case <-scanner.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("expected scanner to stop during service shutdown")
	}
	if scanner.stops != 1 {
		t.Fatalf("expected scanner to stop once, got %d", scanner.stops)
	}
	_ = ctx
}

func startServiceWithScanner(t *testing.T) (*Service, *fakeQuotaAutoDisableScanner, context.Context, context.CancelFunc, chan error) {
	t.Helper()
	cfg := enabledQuotaAutoDisableConfig()
	service := newTestServiceWithConfig(cfg)

	scanner := newFakeQuotaAutoDisableScanner()
	service.quotaAutoDisableScannerFactory = func(*coreauth.Manager, coreauth.QuotaProbeExecutor, internalconfig.AuthQuotaAutoDisableConfig) quotaAutoDisableScanner {
		return scanner
	}
	service.tokenProvider = fakeTokenProvider{}
	service.apiKeyProvider = fakeAPIKeyProvider{}
	service.accessManager = sdkaccess.NewManager()
	service.watcherFactory = func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
		return &WatcherWrapper{
			start: func(context.Context) error { return nil },
			stop:  func() error { return nil },
			setConfig: func(*config.Config) {
			},
		}, nil
	}
	service.configPath = t.TempDir() + "/config.yaml"
	service.cfg.AuthDir = t.TempDir()
	service.cfg.Port = 0

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		errs <- service.Run(ctx)
	}()
	return service, scanner, ctx, cancel, errs
}
