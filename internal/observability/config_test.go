package observability

import (
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveSettings_DisabledSkipsValidation(t *testing.T) {
	cfg := appconfig.ObservabilityConfig{Enabled: false}
	s, err := resolveSettings(cfg)
	if err != nil {
		t.Fatalf("expected no error for disabled config, got %v", err)
	}
	if s.enabled {
		t.Fatalf("expected settings.enabled to be false")
	}
}

func TestResolveSettings_MissingEndpointFails(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pub")
	t.Setenv("LANGFUSE_SECRET_KEY", "sec")
	cfg := appconfig.ObservabilityConfig{Enabled: true}
	if _, err := resolveSettings(cfg); err == nil {
		t.Fatalf("expected an error when exporter.endpoint is empty")
	}
}

func TestResolveSettings_MissingCredentialsFails(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	cfg := appconfig.ObservabilityConfig{
		Enabled: true,
		Exporter: appconfig.ObservabilityExporterConfig{
			Endpoint: "https://example.invalid/api/public/otel/v1/traces",
		},
	}
	if _, err := resolveSettings(cfg); err == nil {
		t.Fatalf("expected an error when exporter credentials are not set in the environment")
	}
}

func TestResolveSettings_NeverReadsSecretsFromConfig(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	// Simulate a misguided attempt to put the secret directly into config.yaml
	// fields that resolveSettings does not even define; this test documents
	// that ObservabilityExporterConfig has no such field.
	cfg := appconfig.ObservabilityConfig{
		Enabled: true,
		Exporter: appconfig.ObservabilityExporterConfig{
			Endpoint: "https://example.invalid/api/public/otel/v1/traces",
		},
	}
	if _, err := resolveSettings(cfg); err == nil {
		t.Fatalf("expected resolution to fail without env-provided credentials, proving config.yaml alone cannot supply secrets")
	}
}

func TestResolveSettings_AppliesDefaultsAndReadsEnvCredentials(t *testing.T) {
	t.Setenv("CUSTOM_PUBLIC_KEY", "pub-value")
	t.Setenv("CUSTOM_SECRET_KEY", "sec-value")
	cfg := appconfig.ObservabilityConfig{
		Enabled: true,
		Exporter: appconfig.ObservabilityExporterConfig{
			Endpoint:     "https://example.invalid/api/public/otel/v1/traces",
			PublicKeyEnv: "CUSTOM_PUBLIC_KEY",
			SecretKeyEnv: "CUSTOM_SECRET_KEY",
		},
	}
	s, err := resolveSettings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.publicKey != "pub-value" || s.secretKey != "sec-value" {
		t.Fatalf("expected credentials to be resolved from the named env vars, got %q/%q", s.publicKey, s.secretKey)
	}
	if s.serviceName != appconfig.DefaultObservabilityServiceName {
		t.Fatalf("expected default service name, got %q", s.serviceName)
	}
	if s.maxQueueSize != appconfig.DefaultObservabilityMaxQueueSize {
		t.Fatalf("expected default max queue size, got %d", s.maxQueueSize)
	}
	if s.captureInput || s.captureOutput {
		t.Fatalf("expected capture toggles to default to false")
	}
}
