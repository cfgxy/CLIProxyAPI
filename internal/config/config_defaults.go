package config

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultDiscoveryServiceType  = "_ai-gateway._tcp"

	// DefaultObservabilityServiceName is the OTEL resource service.name reported when
	// observability.service-name is left empty.
	DefaultObservabilityServiceName = "cli-proxy-api"

	// DefaultObservabilityPublicKeyEnv/DefaultObservabilitySecretKeyEnv name the environment
	// variables read for exporter Basic Auth credentials when the config does not override them.
	// Secrets are never read from config.yaml itself, only from the named environment variable.
	DefaultObservabilityPublicKeyEnv = "LANGFUSE_PUBLIC_KEY"
	DefaultObservabilitySecretKeyEnv = "LANGFUSE_SECRET_KEY"

	// DefaultObservabilityExportTimeoutSeconds bounds a single OTLP export HTTP call.
	DefaultObservabilityExportTimeoutSeconds = 5
	// DefaultObservabilityMaxQueueSize bounds the in-memory span queue; once full, new spans are dropped.
	DefaultObservabilityMaxQueueSize = 2048
	// DefaultObservabilityMaxExportBatchSize bounds how many spans are sent per export call.
	DefaultObservabilityMaxExportBatchSize = 512
	// DefaultObservabilityBatchTimeoutSeconds bounds how long the batch processor waits before exporting.
	DefaultObservabilityBatchTimeoutSeconds = 5
	// DefaultObservabilityShutdownTimeoutSeconds bounds the graceful-shutdown flush.
	DefaultObservabilityShutdownTimeoutSeconds = 5
	// DefaultObservabilityCaptureMaxBytes truncates captured input/output payloads.
	DefaultObservabilityCaptureMaxBytes = 4096
)
