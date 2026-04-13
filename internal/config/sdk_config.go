// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
	// AuthQuotaAutoDisable configures the automatic quota recovery scanner and probe behavior.
	AuthQuotaAutoDisable AuthQuotaAutoDisableConfig `yaml:"auth-quota-auto-disable" json:"auth-quota-auto-disable"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}

// AuthQuotaAutoDisableConfig configures the retry schedule and probe controls for quota auto-disable recovery.
type AuthQuotaAutoDisableConfig struct {
	Enabled              bool     `mapstructure:"enabled" yaml:"enabled" json:"enabled"`
	ScanIntervalSeconds  int      `mapstructure:"scan-interval" yaml:"scan-interval" json:"scan-interval"`
	InitialWaitSeconds   int      `mapstructure:"initial-recovery-wait" yaml:"initial-recovery-wait" json:"initial-recovery-wait"`
	RetryIntervalSeconds int      `mapstructure:"retry-interval" yaml:"retry-interval" json:"retry-interval"`
	MaxConcurrentProbes  int      `mapstructure:"max-concurrent-probes" yaml:"max-concurrent-probes" json:"max-concurrent-probes"`
	Providers            []string `mapstructure:"providers" yaml:"providers" json:"providers"`
}

const (
	// DefaultAuthQuotaAutoDisableScanIntervalSeconds is the default scan interval in seconds.
	DefaultAuthQuotaAutoDisableScanIntervalSeconds = 60
	// DefaultAuthQuotaAutoDisableInitialWaitSeconds sets the default cooldown before the first recovery probe.
	DefaultAuthQuotaAutoDisableInitialWaitSeconds = 6 * 60 * 60
	// DefaultAuthQuotaAutoDisableRetryIntervalSeconds defines the default delay between retry probes.
	DefaultAuthQuotaAutoDisableRetryIntervalSeconds = 60 * 60
	// DefaultAuthQuotaAutoDisableMaxConcurrentProbes caps concurrent recovery probes.
	DefaultAuthQuotaAutoDisableMaxConcurrentProbes = 1
)

var defaultAuthQuotaAutoDisableProviders = []string{"codex", "openai", "chatgpt"}
