package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ParsesAuthQuotaAutoDisable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
remote-management:
  secret-key: ""
auth-quota-auto-disable:
  enabled: true
  scan-interval: 90
  initial-recovery-wait: 1800
  retry-interval: 600
  max-concurrent-probes: 2
  providers:
    - codex
    - chatgpt
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil config")
	}

	got := cfg.AuthQuotaAutoDisable
	if !got.Enabled {
		t.Fatal("expected auth quota auto disable enabled")
	}
	if got.ScanIntervalSeconds != 90 {
		t.Fatalf("ScanIntervalSeconds = %d, want 90", got.ScanIntervalSeconds)
	}
	if got.InitialWaitSeconds != 1800 {
		t.Fatalf("InitialWaitSeconds = %d, want 1800", got.InitialWaitSeconds)
	}
	if got.RetryIntervalSeconds != 600 {
		t.Fatalf("RetryIntervalSeconds = %d, want 600", got.RetryIntervalSeconds)
	}
	if got.MaxConcurrentProbes != 2 {
		t.Fatalf("MaxConcurrentProbes = %d, want 2", got.MaxConcurrentProbes)
	}
	if len(got.Providers) != 2 || got.Providers[0] != "codex" || got.Providers[1] != "chatgpt" {
		t.Fatalf("Providers = %#v, want [codex chatgpt]", got.Providers)
	}
}
