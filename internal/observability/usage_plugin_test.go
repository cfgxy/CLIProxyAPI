package observability

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func baseRecord() sdkusage.Record {
	return sdkusage.Record{
		RequestID:     "req-1",
		TraceID:       "trace-1",
		Provider:      "openai",
		Alias:         "gpt-4o-requested",
		Model:         "gpt-4o-2024-08-06",
		ResponseModel: "gpt-4o-2024-08-06",
		Stream:        true,
		RequestedAt:   time.Now(),
		Latency:       250 * time.Millisecond,
		TTFT:          80 * time.Millisecond,
		Detail: sdkusage.Detail{
			InputTokens:     100,
			OutputTokens:    50,
			TotalTokens:     150,
			CachedTokens:    10,
			ReasoningTokens: 5,
		},
	}
}

func TestUsagePlugin_Disabled_NoOp(t *testing.T) {
	resetProvider(t)
	// No provider active: HandleUsage must not panic and must not create work.
	usagePlugin{}.HandleUsage(context.Background(), baseRecord())
}

func TestUsagePlugin_RecordsGenerationSpanFields(t *testing.T) {
	exp := initInMemoryProvider(t)

	usagePlugin{}.HandleUsage(context.Background(), baseRecord())
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 generation span, got %d", len(spans))
	}
	snap := spans[0].Snapshot()

	want := map[string]interface{}{
		"cpa.provider":               "openai",
		"cpa.model.requested":        "gpt-4o-requested",
		"cpa.model.resolved":         "gpt-4o-2024-08-06",
		"cpa.model.response":         "gpt-4o-2024-08-06",
		"cpa.stream":                 true,
		"cpa.usage.input_tokens":     int64(100),
		"cpa.usage.output_tokens":    int64(50),
		"cpa.usage.total_tokens":     int64(150),
		"cpa.usage.cached_tokens":    int64(10),
		"cpa.usage.reasoning_tokens": int64(5),
	}
	got := map[string]interface{}{}
	for _, kv := range snap.Attributes() {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("attribute %s: expected %v, got %v", k, v, got[k])
		}
	}
	if _, ok := got["cpa.ttft_ms"]; !ok {
		t.Fatalf("expected cpa.ttft_ms to be recorded when TTFT is obtainable")
	}
	if _, ok := got["cpa.latency_ms"]; !ok {
		t.Fatalf("expected cpa.latency_ms to be recorded")
	}
	if snap.Status().Code != codes.Ok {
		t.Fatalf("expected span status Ok for a successful record, got %v", snap.Status().Code)
	}
}

func TestUsagePlugin_RecordsProviderError(t *testing.T) {
	exp := initInMemoryProvider(t)

	rec := baseRecord()
	rec.Failed = true
	rec.Fail = sdkusage.Failure{StatusCode: http.StatusBadGateway, Body: "upstream exploded"}
	usagePlugin{}.HandleUsage(context.Background(), rec)
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	snap := spans[0].Snapshot()
	if snap.Status().Code != codes.Error {
		t.Fatalf("expected span status Error for a failed record, got %v", snap.Status().Code)
	}
	if len(snap.Events()) == 0 {
		t.Fatalf("expected the provider error to be recorded as a span event")
	}
}

func TestUsagePlugin_GenerationSpanNestsUnderHTTPRootSpan(t *testing.T) {
	exp := initInMemoryProvider(t)

	tracer := activeTracer()
	ctx, rootSpan := tracer.Start(context.Background(), "GET /v1/chat/completions", trace.WithSpanKind(trace.SpanKindServer))
	usagePlugin{}.HandleUsage(ctx, baseRecord())
	rootSpan.End()
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected root span + generation span, got %d", len(spans))
	}

	var rootSpanID, genParentID trace.SpanID
	for _, s := range spans {
		snap := s.Snapshot()
		switch snap.Name() {
		case "GET /v1/chat/completions":
			rootSpanID = snap.SpanContext().SpanID()
		case "generation openai":
			genParentID = snap.Parent().SpanID()
		}
	}
	if rootSpanID == (trace.SpanID{}) {
		t.Fatalf("expected to find the HTTP root span")
	}
	if genParentID != rootSpanID {
		t.Fatalf("expected generation span's parent span id to equal the HTTP root span id, got parent=%s root=%s", genParentID, rootSpanID)
	}
}

func TestUsagePlugin_StreamingTerminationPaths_EachProducesExactlyOneSpan(t *testing.T) {
	cases := []struct {
		name string
		mod  func(r *sdkusage.Record)
	}{
		{"normal completion", func(r *sdkusage.Record) {}},
		{"client disconnect", func(r *sdkusage.Record) {
			r.Failed = true
			r.Fail = sdkusage.Failure{StatusCode: 499, Body: "client closed request"}
		}},
		{"provider disconnect", func(r *sdkusage.Record) {
			r.Failed = true
			r.Fail = sdkusage.Failure{StatusCode: http.StatusBadGateway, Body: "provider connection reset"}
		}},
		{"context cancellation", func(r *sdkusage.Record) {
			r.Failed = true
			r.Fail = sdkusage.Failure{StatusCode: 499, Body: "context canceled"}
		}},
		{"timeout", func(r *sdkusage.Record) {
			r.Failed = true
			r.Fail = sdkusage.Failure{StatusCode: http.StatusGatewayTimeout, Body: "upstream timeout"}
		}},
		{"stream error", func(r *sdkusage.Record) {
			r.Failed = true
			r.Fail = sdkusage.Failure{StatusCode: http.StatusInternalServerError, Body: "stream decode error"}
		}},
		{"usage-only in last chunk", func(r *sdkusage.Record) {
			r.Detail = sdkusage.Detail{TotalTokens: 42}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := initInMemoryProvider(t)
			rec := baseRecord()
			tc.mod(&rec)
			usagePlugin{}.HandleUsage(context.Background(), rec)
			forceFlush(t)

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("expected exactly 1 ended span for termination path %q, got %d", tc.name, len(spans))
			}
			snap := spans[0].Snapshot()
			if snap.EndTime().Before(snap.StartTime()) {
				t.Fatalf("expected the span to have a valid end time for termination path %q", tc.name)
			}
		})
	}
}

// TestUsagePlugin_RedactionSurvivesFullSpanSerialization is the disprovable
// redaction assertion: it constructs a record whose failure body carries a
// real-shaped credential, exports the resulting span through an in-memory
// exporter, and asserts the credential does not appear anywhere in the
// span's fully serialized attributes/events/status. If redactText were
// replaced with an identity function, this assertion would fail.
func TestUsagePlugin_RedactionSurvivesFullSpanSerialization(t *testing.T) {
	exp := initInMemoryProvider(t)

	const secret = "sk-live-abcdef1234567890superscret"
	rec := baseRecord()
	rec.Failed = true
	rec.Fail = sdkusage.Failure{
		StatusCode: http.StatusUnauthorized,
		Body:       "upstream rejected request: Authorization: Bearer " + secret,
	}
	usagePlugin{}.HandleUsage(context.Background(), rec)
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}

	serialized := serializeSpanForAssertion(spans[0].Snapshot())
	if strings.Contains(serialized, secret) {
		t.Fatalf("credential leaked into exported span: %q", serialized)
	}
}

// serializeSpanForAssertion flattens every human-readable surface of a
// ReadOnlySpan (name, all attribute values, every event's name/attributes,
// and the status description) into a single string, so tests can assert a
// credential is absent from the span as a whole rather than from one
// attribute in isolation.
func serializeSpanForAssertion(snap sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(snap.Name())
	b.WriteString("|")
	b.WriteString(snap.Status().Description)
	for _, kv := range snap.Attributes() {
		fmt.Fprintf(&b, "|%s=%v", kv.Key, kv.Value.AsInterface())
	}
	for _, ev := range snap.Events() {
		b.WriteString("|event:")
		b.WriteString(ev.Name)
		for _, kv := range ev.Attributes {
			fmt.Fprintf(&b, "|%s=%v", kv.Key, kv.Value.AsInterface())
		}
	}
	return b.String()
}
