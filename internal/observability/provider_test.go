package observability

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// resetProvider ensures each test starts and ends with a clean package-wide
// singleton, regardless of what an earlier test left behind.
func resetProvider(t *testing.T) {
	t.Helper()
	_ = swapProvider(context.Background(), nil)
	t.Cleanup(func() {
		_ = swapProvider(context.Background(), nil)
	})
}

func TestInit_DisabledCreatesNoProviderOrExporter(t *testing.T) {
	resetProvider(t)
	Init(context.Background(), appconfig.ObservabilityConfig{Enabled: false})
	if Enabled() {
		t.Fatalf("expected Enabled() to be false when config.Enabled is false")
	}
	providerMu.RLock()
	got := current
	providerMu.RUnlock()
	if got != nil {
		t.Fatalf("expected no provider to be constructed when disabled")
	}
}

func TestInit_InvalidConfigFailsOpenToDisabled(t *testing.T) {
	resetProvider(t)
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	// Enabled with no endpoint and no credentials: must fail open, not panic
	// or abort startup.
	Init(context.Background(), appconfig.ObservabilityConfig{Enabled: true})
	if Enabled() {
		t.Fatalf("expected Enabled() to be false after invalid configuration")
	}
}

// slowExporter simulates a genuinely unreachable/slow OTLP endpoint: every
// export blocks until the caller's context is done, so it only returns once
// the SDK's own export timeout fires.
type slowExporter struct {
	exportCalls  atomic.Int64
	exportBlock  time.Duration
	shutdownCall atomic.Int64
	shutdownWait time.Duration
}

func (e *slowExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.exportCalls.Add(1)
	select {
	case <-time.After(e.exportBlock):
	case <-ctx.Done():
	}
	return ctx.Err()
}

func (e *slowExporter) Shutdown(ctx context.Context) error {
	e.shutdownCall.Add(1)
	select {
	case <-time.After(e.shutdownWait):
	case <-ctx.Done():
	}
	return nil
}

func TestFailOpen_UnreachableExporterDoesNotBlockRequests(t *testing.T) {
	resetProvider(t)

	// A real TCP listener that accepts connections but never responds, so
	// the OTLP HTTP client genuinely cannot complete a round trip.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open unreachable listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, errAccept := ln.Accept()
			if errAccept != nil {
				return
			}
			// Accept the connection but never write a response, and never close it.
			_ = conn
		}
	}()

	t.Setenv("LANGFUSE_PUBLIC_KEY", "pub")
	t.Setenv("LANGFUSE_SECRET_KEY", "sec")
	cfg := appconfig.ObservabilityConfig{
		Enabled: true,
		Exporter: appconfig.ObservabilityExporterConfig{
			Endpoint:            "http://" + ln.Addr().String() + "/v1/traces",
			Insecure:            true,
			TimeoutSeconds:      1,
			MaxQueueSize:        4,
			MaxExportBatchSize:  1,
			BatchTimeoutSeconds: 1,
		},
	}
	Init(context.Background(), cfg)
	if !Enabled() {
		t.Fatalf("expected tracing to be enabled with a valid (if unreachable) endpoint")
	}

	router := newGinTestRouter()
	start := time.Now()
	for i := 0; i < 10; i++ {
		performRequest(router, http.MethodGet, "/v1/models")
	}
	elapsed := time.Since(start)
	// The exporter is entirely unreachable; if export were on the request
	// path this would take at least MaxExportBatchSize * export-timeout(1s).
	// Fail-Open requires the request path to be unaffected.
	if elapsed > 2*time.Second {
		t.Fatalf("expected requests to complete quickly despite an unreachable exporter, took %s", elapsed)
	}
}

func TestFailOpen_QueueFullDropsInsteadOfBlocking(t *testing.T) {
	resetProvider(t)

	exp := &slowExporter{exportBlock: 10 * time.Second}
	s := settings{
		enabled:         true,
		serviceName:     "test",
		exportTimeout:   10 * time.Second,
		maxQueueSize:    1,
		maxBatchSize:    1,
		batchTimeout:    time.Hour,
		shutdownTimeout: 200 * time.Millisecond,
	}
	initProvider(context.Background(), s, exp)

	router := newGinTestRouter()
	start := time.Now()
	for i := 0; i < 50; i++ {
		performRequest(router, http.MethodGet, "/v1/models")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("expected a full export queue to drop spans rather than block requests, took %s", elapsed)
	}
}

func TestShutdown_BoundedFlushTimeoutNeverHangs(t *testing.T) {
	resetProvider(t)

	exp := &slowExporter{exportBlock: 30 * time.Second, shutdownWait: 30 * time.Second}
	s := settings{
		enabled:         true,
		serviceName:     "test",
		exportTimeout:   30 * time.Second,
		maxQueueSize:    10,
		maxBatchSize:    10,
		batchTimeout:    time.Hour,
		shutdownTimeout: 300 * time.Millisecond,
	}
	initProvider(context.Background(), s, exp)

	router := newGinTestRouter()
	performRequest(router, http.MethodGet, "/v1/models")

	start := time.Now()
	if err := Shutdown(context.Background()); err == nil {
		t.Fatalf("expected shutdown to report an error when the exporter never completes in time")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("expected the shutdown flush to be bounded by shutdownTimeout, took %s", elapsed)
	}
}
