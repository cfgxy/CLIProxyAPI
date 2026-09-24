package observability

import (
	"context"
	"encoding/base64"
	"sync"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// provider is the process-wide observability runtime state. It is guarded by
// providerMu and replaced wholesale by Init/Reset; a nil tracerProvider means
// tracing is disabled and every exported entry point is a no-op.
type provider struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer

	shutdownTimeout time.Duration

	settings settings
}

var (
	providerMu             sync.RWMutex
	current                *provider
	disabledTracer         = nooptrace.NewTracerProvider().Tracer("cli-proxy-api/observability")
	pluginRegistrationName = "observability"
)

// Init (re)configures the package-wide observability runtime from cfg. It is
// safe to call multiple times, e.g. on config hot-reload; the previous
// provider (if any) is flushed and shut down first with a bounded timeout.
//
// When cfg.Enabled is false, or when configuration validation fails, Init
// leaves the package fully disabled (Fail-Open): zero spans, zero exporter,
// zero background goroutines are created, and the failure is only logged.
func Init(ctx context.Context, cfg appconfig.ObservabilityConfig) {
	resolved, err := resolveSettings(cfg)
	if err != nil {
		log.WithError(err).Warn("observability: disabled due to invalid configuration")
		swapProvider(ctx, nil)
		return
	}
	if !resolved.enabled {
		swapProvider(ctx, nil)
		return
	}

	exporter, err := newExporter(resolved)
	if err != nil {
		log.WithError(err).Warn("observability: disabled because the OTLP exporter could not be constructed")
		swapProvider(ctx, nil)
		return
	}
	initProvider(ctx, resolved, exporter)
}

// initProvider builds and activates a tracer provider around exporter. It is
// factored out of Init so tests can inject an in-memory sdktrace.SpanExporter
// and assert on captured spans directly, instead of decoding the OTLP wire
// format from a fake HTTP endpoint.
func initProvider(ctx context.Context, resolved settings, exporter sdktrace.SpanExporter) {
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(resolved.serviceName),
		),
	)
	if err != nil {
		// resource.New only fails on attribute schema conflicts; fall back to a
		// minimal resource instead of disabling tracing entirely.
		res = resource.NewSchemaless(semconv.ServiceName(resolved.serviceName))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(resolved.maxQueueSize),
			sdktrace.WithMaxExportBatchSize(resolved.maxBatchSize),
			sdktrace.WithBatchTimeout(resolved.batchTimeout),
			sdktrace.WithExportTimeout(resolved.exportTimeout),
		),
	)

	p := &provider{
		tp:              tp,
		tracer:          tp.Tracer("cli-proxy-api/observability"),
		shutdownTimeout: resolved.shutdownTimeout,
		settings:        resolved,
	}
	swapProvider(ctx, p)

	sdkusage.RegisterNamedPlugin(pluginRegistrationName, &usagePlugin{})
	log.WithField("endpoint", resolved.endpoint).Info("observability: OpenTelemetry tracing enabled")
}

// newExporter builds the OTLP/HTTP exporter with Basic Auth headers derived
// from the resolved (never-persisted) public/secret key pair.
func newExporter(s settings) (*otlptrace.Exporter, error) {
	creds := base64.StdEncoding.EncodeToString([]byte(s.publicKey + ":" + s.secretKey))
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(s.endpoint),
		otlptracehttp.WithTimeout(s.exportTimeout),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + creds,
		}),
	}
	if s.insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	client := otlptracehttp.NewClient(opts...)
	// Use context.Background(): construction only prepares the HTTP client and
	// never dials the network, so it must not inherit a caller-scoped deadline.
	return otlptrace.New(context.Background(), client)
}

// Shutdown flushes and stops the currently active tracer provider, if any,
// bounded by the configured (or default) shutdown timeout so a stuck or
// unreachable exporter can never hang process shutdown indefinitely.
func Shutdown(ctx context.Context) error {
	return swapProvider(ctx, nil)
}

func swapProvider(ctx context.Context, next *provider) error {
	providerMu.Lock()
	prev := current
	current = next
	providerMu.Unlock()

	if prev == nil || prev.tp == nil {
		return nil
	}
	timeout := prev.shutdownTimeout
	if timeout <= 0 {
		timeout = time.Duration(appconfig.DefaultObservabilityShutdownTimeoutSeconds) * time.Second
	}
	// Deliberately detached from ctx: a bounded shutdown flush must run to
	// completion (or its own timeout) even if the caller's shutdown context is
	// already close to its own deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := prev.tp.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Warn("observability: tracer provider shutdown/flush did not complete cleanly")
		return err
	}
	return nil
}

// Enabled reports whether tracing is currently active.
func Enabled() bool {
	providerMu.RLock()
	defer providerMu.RUnlock()
	return current != nil
}

func activeTracer() trace.Tracer {
	providerMu.RLock()
	defer providerMu.RUnlock()
	if current == nil {
		return disabledTracer
	}
	return current.tracer
}

func activeCaptureSettings() (input, output bool, maxBytes int) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	if current == nil {
		return false, false, 0
	}
	return current.settings.captureInput, current.settings.captureOutput, current.settings.captureMaxBytes
}
