package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
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

type probeCheckingScanner struct {
	prober  coreauth.QuotaProbeExecutor
	auth    *coreauth.Auth
	started chan struct{}
	done    chan struct{}
	err     error
	status  int
}

func newProbeCheckingScanner(prober coreauth.QuotaProbeExecutor, auth *coreauth.Auth) *probeCheckingScanner {
	return &probeCheckingScanner{
		prober:  prober,
		auth:    auth,
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (p *probeCheckingScanner) Start(ctx context.Context) {
	close(p.started)
	status, _, err := p.prober.ProbeAuth(ctx, p.auth)
	p.status = status
	p.err = err
	close(p.done)
}

func (p *probeCheckingScanner) Stop() {}

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

func TestService_ProbeExecutorFallbackDuringColdStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	cfg := enabledQuotaAutoDisableConfig()
	service := newTestServiceWithConfig(cfg)

	auth := &coreauth.Auth{
		ID:       "disabled-openai-auth",
		Provider: "openai",
		Disabled: true,
		Status:   coreauth.StatusDisabled,
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}
	if _, err := service.coreManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	scanner := newProbeCheckingScanner(&serviceQuotaProbeExecutor{service: service}, auth)
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

	select {
	case <-scanner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected probe to run during service start")
	}

	select {
	case <-scanner.done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not finish in time")
	}

	if scanner.err != nil {
		t.Fatalf("expected probe to succeed, got error: %v", scanner.err)
	}
	if scanner.status != http.StatusOK {
		t.Fatalf("expected probe status 200, got %d", scanner.status)
	}

	cancel()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("service Run did not return after context cancellation")
	}
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
