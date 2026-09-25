package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// initInMemoryProvider activates the package-wide provider around an
// in-memory exporter, forcing every span to flush synchronously via
// ForceFlush instead of waiting for the batch timeout, so tests can inspect
// exported spans deterministically.
func initInMemoryProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	resetProvider(t)
	exp := tracetest.NewInMemoryExporter()
	s := settings{
		enabled:         true,
		serviceName:     "test",
		exportTimeout:   time.Second,
		maxQueueSize:    64,
		maxBatchSize:    64,
		batchTimeout:    time.Hour,
		shutdownTimeout: time.Second,
	}
	initProvider(context.Background(), s, exp)
	return exp
}

func forceFlush(t *testing.T) {
	t.Helper()
	providerMu.RLock()
	p := current
	providerMu.RUnlock()
	if p == nil {
		return
	}
	if err := p.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush failed: %v", err)
	}
}

func attrString(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func TestMiddleware_Disabled_NoSpanNoOverhead(t *testing.T) {
	resetProvider(t)
	router := newGinTestRouter()
	w := performRequest(router, http.MethodGet, "/v1/models")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMiddleware_RecordsHTTPFields(t *testing.T) {
	exp := initInMemoryProvider(t)

	router := newGinTestRouter()
	performRequest(router, http.MethodGet, "/v1/models")
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	span := spans[0]

	if method, ok := attrString(span.Snapshot(), "http.method"); !ok || method != http.MethodGet {
		t.Fatalf("expected http.method=GET, got %q (present=%v)", method, ok)
	}
	if route, ok := attrString(span.Snapshot(), "http.route"); !ok || route != "/v1/models" {
		t.Fatalf("expected http.route=/v1/models, got %q (present=%v)", route, ok)
	}

	var statusFound, latencyFound bool
	for _, kv := range span.Snapshot().Attributes() {
		switch string(kv.Key) {
		case "http.status_code":
			statusFound = true
			if kv.Value.AsInt64() != http.StatusOK {
				t.Fatalf("expected http.status_code=200, got %d", kv.Value.AsInt64())
			}
		case "http.latency_ms":
			latencyFound = true
		}
	}
	if !statusFound {
		t.Fatalf("expected http.status_code attribute to be present")
	}
	if !latencyFound {
		t.Fatalf("expected http.latency_ms attribute to be present")
	}
}

func TestMiddleware_RecordsHTTPErrorStatus(t *testing.T) {
	exp := initInMemoryProvider(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(internallogging.GinLogrusLogger())
	r.Use(internallogging.CPATraceIDMiddleware())
	r.Use(Middleware())
	r.GET("/boom", func(c *gin.Context) {
		_ = c.Error(errBoom{})
		c.Status(http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	snap := spans[0].Snapshot()
	if snap.Status().Code != codes.Error {
		t.Fatalf("expected span status Error for a 5xx response, got %v", snap.Status().Code)
	}
	if len(snap.Events()) == 0 {
		t.Fatalf("expected the gin error to be recorded as a span event")
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func TestMiddleware_RequestIDBridgesToSpanAttribute(t *testing.T) {
	exp := initInMemoryProvider(t)

	router := newGinTestRouter()
	performRequest(router, http.MethodGet, "/v1/models")
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	requestID, ok := attrString(spans[0].Snapshot(), "cpa.request_id")
	if !ok || requestID == "" {
		t.Fatalf("expected cpa.request_id span attribute bridged from internal/logging, got %q (present=%v)", requestID, ok)
	}
}

func TestMiddleware_CaptureDisabledByDefault(t *testing.T) {
	exp := initInMemoryProvider(t)

	router := newGinTestRouter()
	performRequest(router, http.MethodGet, "/v1/models")
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if _, ok := attrString(spans[0].Snapshot(), "cpa.capture.input"); ok {
		t.Fatalf("expected no cpa.capture.input attribute when capture is disabled by default")
	}
	if _, ok := attrString(spans[0].Snapshot(), "cpa.capture.output"); ok {
		t.Fatalf("expected no cpa.capture.output attribute when capture is disabled by default")
	}
}

func TestMiddleware_CaptureEnabledRecordsRedactedPayloads(t *testing.T) {
	resetProvider(t)
	exp := tracetest.NewInMemoryExporter()
	s := settings{
		enabled:         true,
		serviceName:     "test",
		exportTimeout:   time.Second,
		maxQueueSize:    64,
		maxBatchSize:    64,
		batchTimeout:    time.Hour,
		shutdownTimeout: time.Second,
		captureInput:    true,
		captureOutput:   true,
		captureMaxBytes: 4096,
	}
	initProvider(context.Background(), s, exp)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(internallogging.GinLogrusLogger())
	r.Use(internallogging.CPATraceIDMiddleware())
	r.Use(Middleware())
	r.POST("/echo", func(c *gin.Context) {
		c.String(http.StatusOK, `{"reply":"ok Authorization: Bearer sk-live-abcdef1234567890"}`)
	})

	body := `{"Authorization":"Bearer sk-live-abcdef1234567890"}`
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	input, ok := attrString(spans[0].Snapshot(), "cpa.capture.input")
	if !ok {
		t.Fatalf("expected cpa.capture.input attribute when capture is enabled")
	}
	if containsCredential(input) {
		t.Fatalf("expected captured input to be redacted, got %q", input)
	}
	output, ok := attrString(spans[0].Snapshot(), "cpa.capture.output")
	if !ok {
		t.Fatalf("expected cpa.capture.output attribute when capture is enabled")
	}
	if containsCredential(output) {
		t.Fatalf("expected captured output to be redacted, got %q", output)
	}
}

func containsCredential(s string) bool {
	return len(s) > 0 && strings.Contains(s, "abcdef1234567890")
}
