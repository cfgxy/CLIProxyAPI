// Package observability provides an abstract OpenTelemetry tracing facade for
// the proxy runtime. Callers outside this package only ever see abstract
// request/generation lifecycle hooks (Middleware, RecordGeneration); no
// Langfuse- or OTLP-specific concept leaks outside this package. When
// disabled, every entry point is a fully inert no-op: no spans, no exporter,
// no background goroutine is created.
package observability

import (
	"fmt"
	"os"
	"strings"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// settings is the fully resolved, runtime-ready observability configuration,
// including secrets resolved from environment variables. It is never derived
// from or written back into config.yaml.
type settings struct {
	enabled     bool
	serviceName string

	endpoint  string
	publicKey string
	secretKey string
	insecure  bool

	exportTimeout   time.Duration
	maxQueueSize    int
	maxBatchSize    int
	batchTimeout    time.Duration
	shutdownTimeout time.Duration

	captureInput    bool
	captureOutput   bool
	captureMaxBytes int

	plaintextUserID bool
}

// resolveSettings validates and normalizes an ObservabilityConfig, resolving
// secrets from the environment. Any validation failure is treated as Fail-Open:
// the caller must fall back to a disabled state rather than aborting startup.
func resolveSettings(cfg appconfig.ObservabilityConfig) (settings, error) {
	s := settings{
		enabled:         cfg.Enabled,
		serviceName:     strings.TrimSpace(cfg.ServiceName),
		endpoint:        strings.TrimSpace(cfg.Exporter.Endpoint),
		insecure:        cfg.Exporter.Insecure,
		captureInput:    cfg.Capture.Input,
		captureOutput:   cfg.Capture.Output,
		captureMaxBytes: cfg.Capture.MaxBytes,
		plaintextUserID: cfg.Identity.PlaintextUserID,
	}
	if !s.enabled {
		return s, nil
	}
	if s.serviceName == "" {
		s.serviceName = appconfig.DefaultObservabilityServiceName
	}
	if s.endpoint == "" {
		return settings{}, fmt.Errorf("observability: exporter.endpoint is required when enabled")
	}

	publicKeyEnv := strings.TrimSpace(cfg.Exporter.PublicKeyEnv)
	if publicKeyEnv == "" {
		publicKeyEnv = appconfig.DefaultObservabilityPublicKeyEnv
	}
	secretKeyEnv := strings.TrimSpace(cfg.Exporter.SecretKeyEnv)
	if secretKeyEnv == "" {
		secretKeyEnv = appconfig.DefaultObservabilitySecretKeyEnv
	}
	s.publicKey = strings.TrimSpace(os.Getenv(publicKeyEnv))
	s.secretKey = strings.TrimSpace(os.Getenv(secretKeyEnv))
	if s.publicKey == "" || s.secretKey == "" {
		return settings{}, fmt.Errorf("observability: missing exporter credentials in environment variables %s/%s", publicKeyEnv, secretKeyEnv)
	}

	s.exportTimeout = durationOrDefault(cfg.Exporter.TimeoutSeconds, appconfig.DefaultObservabilityExportTimeoutSeconds)
	s.maxQueueSize = intOrDefault(cfg.Exporter.MaxQueueSize, appconfig.DefaultObservabilityMaxQueueSize)
	s.maxBatchSize = intOrDefault(cfg.Exporter.MaxExportBatchSize, appconfig.DefaultObservabilityMaxExportBatchSize)
	s.batchTimeout = durationOrDefault(cfg.Exporter.BatchTimeoutSeconds, appconfig.DefaultObservabilityBatchTimeoutSeconds)
	s.shutdownTimeout = durationOrDefault(cfg.Exporter.ShutdownTimeoutSeconds, appconfig.DefaultObservabilityShutdownTimeoutSeconds)
	if s.captureMaxBytes <= 0 {
		s.captureMaxBytes = appconfig.DefaultObservabilityCaptureMaxBytes
	}
	return s, nil
}

func durationOrDefault(seconds, def int) time.Duration {
	if seconds <= 0 {
		seconds = def
	}
	return time.Duration(seconds) * time.Second
}

func intOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
