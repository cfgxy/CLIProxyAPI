package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
			CacheReadTokens: 10,
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
		"langfuse.observation.type":          "generation",
		"langfuse.observation.model.name":    "gpt-4o-2024-08-06",
		"langfuse.observation.usage_details": `{"input":90,"input_cache_read":10,"output":50,"total":150}`,
		"cpa.provider":                       "openai",
		"cpa.model.requested":                "gpt-4o-requested",
		"cpa.model.resolved":                 "gpt-4o-2024-08-06",
		"cpa.model.response":                 "gpt-4o-2024-08-06",
		"cpa.stream":                         true,
		"cpa.usage.input_tokens":             int64(100),
		"cpa.usage.output_tokens":            int64(50),
		"cpa.usage.total_tokens":             int64(150),
		"cpa.usage.cached_tokens":            int64(10),
		"cpa.usage.reasoning_tokens":         int64(5),
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

func TestLangfuseUsageDetailsExclusiveBuckets(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		detail   sdkusage.Detail
		want     map[string]int64
	}{
		{
			name: "OpenAI inclusive cache and reasoning", provider: "openai",
			detail: sdkusage.Detail{InputTokens: 100, OutputTokens: 30, CacheReadTokens: 40, CacheCreationTokens: 10, ReasoningTokens: 12, TotalTokens: 130},
			want:   map[string]int64{"input": 50, "input_cache_read": 40, "input_cache_creation": 10, "output": 30, "total": 130},
		},
		{
			name: "Claude independent cache", provider: "anthropic",
			detail: sdkusage.Detail{InputTokens: 30, OutputTokens: 5, CacheReadTokens: 7, CacheCreationTokens: 13, TotalTokens: 55},
			want:   map[string]int64{"input": 30, "input_cache_read": 7, "input_cache_creation": 13, "output": 5, "total": 55},
		},
		{
			name: "Gemini separate reasoning", provider: "gemini",
			detail: sdkusage.Detail{InputTokens: 20, OutputTokens: 7, CacheReadTokens: 5, ReasoningTokens: 3, TotalTokens: 30},
			want:   map[string]int64{"input": 15, "input_cache_read": 5, "output": 10, "total": 30},
		},
		{
			name: "partly known", provider: "openai",
			detail: sdkusage.Detail{TokenBreakdown: sdkusage.NewPartialSubsetTokenBreakdown(10, 4, 0, 0, 0, 15)},
			want:   map[string]int64{"input": 6, "input_cache_read": 4, "unclassified": 5, "total": 15},
		},
		{
			name: "contradictory provider counts", provider: "openai",
			detail: sdkusage.Detail{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4, ReasoningTokens: 1, TotalTokens: 20},
			want:   map[string]int64{"unclassified": 20, "total": 20},
		},
		{
			name: "unknown provider", provider: "custom-provider",
			detail: sdkusage.Detail{InputTokens: 10, OutputTokens: 3, TotalTokens: 13},
			want:   map[string]int64{"unclassified": 13, "total": 13},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := langfuseUsageDetails(sdkusage.Record{Provider: tc.provider, Detail: tc.detail})
			got := make(map[string]int64)
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("unexpected usage keys: got=%v want=%v", got, tc.want)
			}
			var sum int64
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s: got %d want %d", key, got[key], want)
				}
				if key != "total" {
					sum += got[key]
				}
			}
			if sum != got["total"] {
				t.Fatalf("buckets overlap or omit tokens: sum=%d total=%d", sum, got["total"])
			}
		})
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

func TestUsagePlugin_ExportsUnmodifiedProviderBodiesAfterResponseCompletes(t *testing.T) {
	exp := initInMemoryProvider(t)
	requestBody := []byte(`{"messages":"` + strings.Repeat("dialogue", 700) + `","tools":[{"name":"search"}]}`)
	providerBody := []byte("event: reasoning\r\ndata: {\"thinking\":\"step 1\"}\r\n\r\nevent: tool_call\ndata: {\"name\":\"search\"}\n\nevent: final\ndata: result\n\n")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(actual, requestBody) {
			t.Errorf("provider request differs from fixture: err=%v", err)
		}
		_, _ = w.Write(providerBody)
	}))
	defer provider.Close()

	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	sdkusage.UpstreamCaptureFromContext(ctx).Arm()
	client := &http.Client{Transport: sdkusage.CaptureHTTPTransport(ctx, http.DefaultTransport)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer header-only-secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	usagePlugin{}.HandleUsage(ctx, baseRecord())
	forceFlush(t)
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("generation span ended before response consumption: %d", got)
	}
	readBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	forceFlush(t)
	spans := exp.GetSpans()
	if len(spans) != 1 || !bytes.Equal(readBody, providerBody) {
		t.Fatalf("provider response or exported generation count differs: response=%d spans=%d", len(readBody), len(spans))
	}
	snap := spans[0].Snapshot()
	input, _ := attrString(snap, "langfuse.observation.input")
	output, _ := attrString(snap, "langfuse.observation.output")
	var complete bool
	for _, kv := range snap.Attributes() {
		if string(kv.Key) == "cpa.upstream.capture_complete" {
			complete = kv.Value.AsBool()
		}
	}
	if input != string(requestBody) || output != string(providerBody) {
		t.Fatal("generation attributes differ from the actual provider request or response")
	}
	if !complete {
		t.Fatal("provider response reached EOF but the capture was marked incomplete")
	}
	if strings.Contains(serializeSpanForAssertion(snap), "header-only-secret") {
		t.Fatal("authorization header leaked into exported span")
	}
}

func TestMiddleware_ProviderCaptureSurvivesRealHandlerContext(t *testing.T) {
	resetProvider(t)
	exp := tracetest.NewInMemoryExporter()
	initProvider(context.Background(), settings{
		enabled: true, serviceName: "test", captureUpstream: true,
		exportTimeout: time.Second, maxQueueSize: 64, maxBatchSize: 64,
		batchTimeout: time.Hour, shutdownTimeout: time.Second,
	}, exp)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, err := io.ReadAll(r.Body); err != nil || string(got) != "actual provider request" {
			t.Errorf("provider request differs: %q (%v)", got, err)
		}
		_, _ = w.Write([]byte("actual provider response"))
	}))
	defer provider.Close()
	r := newGinTestRouter()
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		handler := &handlers.BaseAPIHandler{Cfg: &config.SDKConfig{}}
		cliCtx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
		defer cancel()
		attempt := sdkusage.WithUpstreamCaptureAttempt(cliCtx)
		capture := sdkusage.UpstreamCaptureFromContext(attempt)
		if capture == nil {
			t.Error("handler context lost middleware's upstream capture setting")
			return
		}
		capture.Arm()
		client := &http.Client{Transport: sdkusage.CaptureHTTPTransport(attempt, http.DefaultTransport)}
		upstreamReq, err := http.NewRequestWithContext(attempt, http.MethodPost, provider.URL, strings.NewReader("actual provider request"))
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(upstreamReq)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		usagePlugin{}.HandleUsage(attempt, baseRecord())
		c.Status(http.StatusOK)
	})
	performRequest(r, http.MethodPost, "/v1/chat/completions")
	forceFlush(t)
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected root and generation spans, got %d", len(spans))
	}
	for _, span := range spans {
		if span.Name != "generation openai" {
			continue
		}
		gotInput, _ := attrString(span.Snapshot(), "langfuse.observation.input")
		gotOutput, _ := attrString(span.Snapshot(), "langfuse.observation.output")
		if gotInput == "actual provider request" && gotOutput == "actual provider response" {
			return
		}
		t.Errorf("generation did not record provider input/output: %q/%q", gotInput, gotOutput)
		return
	}
	t.Fatal("generation span is missing")
}

func TestUsagePlugin_ExportsEachDuplexTurnWhileConnectionStaysOpen(t *testing.T) {
	exp := initInMemoryProvider(t)
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	capture := sdkusage.UpstreamCaptureFromContext(ctx)
	capture.Arm()
	for turn := 1; turn <= 2; turn++ {
		input := fmt.Sprintf(`{"type":"response.create","turn":%d}`, turn)
		output := fmt.Sprintf(`{"type":"response.completed","turn":%d}`, turn)
		capture.RecordWebSocket("sent", 1, []byte(input))
		capture.RecordWebSocket("received", 1, []byte(output))
		usagePlugin{}.HandleUsage(sdkusage.CaptureWebSocketTurn(ctx, true), baseRecord())
		forceFlush(t)
		spans := exp.GetSpans()
		if len(spans) != turn {
			t.Fatalf("turn %d did not export while socket remained open: spans=%d", turn, len(spans))
		}
		inputAttr, _ := attrString(spans[turn-1].Snapshot(), "langfuse.observation.input")
		outputAttr, _ := attrString(spans[turn-1].Snapshot(), "langfuse.observation.output")
		var sent, received []struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal([]byte(inputAttr), &sent); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(outputAttr), &received); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 || len(received) != 1 || sent[0].Payload != input || received[0].Payload != output {
			t.Errorf("turn %d exported a different provider input/output", turn)
		}
	}
	capture.FinishWebSocket(false)
}

// TestUsagePlugin_GenerationSpanNestsUnderHTTPRootSpan_RealHandlerCtxChain is
// the disprovable nesting assertion: it drives a real HTTP request through
// the same gin middleware chain and context plumbing production handlers use
// (observability.Middleware -> BaseAPIHandler.GetContextWithCancel, exactly
// as every provider handler calls it before invoking an executor), instead
// of a hand-built context.Context that skips GetContextWithCancel entirely.
// GetContextWithCancel intentionally derives its returned context from
// context.Background() (so request cancellation does not tear down
// in-flight work), which is exactly the mechanism that used to drop the
// OTEL SpanContext started by Middleware; a hand-built ctx cannot exercise
// that code path. This test would fail if GetContextWithCancel stopped
// propagating the SpanContext.
func TestUsagePlugin_GenerationSpanNestsUnderHTTPRootSpan_RealHandlerCtxChain(t *testing.T) {
	exp := initInMemoryProvider(t)

	r := newGinTestRouter()
	r.GET("/v1/chat/completions", func(c *gin.Context) {
		handler := &handlers.BaseAPIHandler{Cfg: &config.SDKConfig{}}
		cliCtx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
		defer cancel()

		usagePlugin{}.HandleUsage(cliCtx, baseRecord())
		c.Status(http.StatusOK)
	})
	performRequest(r, http.MethodGet, "/v1/chat/completions")
	forceFlush(t)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected root span + generation span, got %d", len(spans))
	}

	var rootSpan, genSpan *tracetest.SpanStub
	for i := range spans {
		snap := &spans[i]
		switch snap.Name {
		case "GET /v1/chat/completions":
			rootSpan = snap
		case "generation openai":
			genSpan = snap
		}
	}
	if rootSpan == nil {
		t.Fatalf("expected to find the HTTP root span, spans=%+v", spans)
	}
	if genSpan == nil {
		t.Fatalf("expected to find the generation span, spans=%+v", spans)
	}
	if genSpan.SpanContext.TraceID() != rootSpan.SpanContext.TraceID() {
		t.Fatalf("expected generation span to share the HTTP root span's trace id, got generation=%s root=%s",
			genSpan.SpanContext.TraceID(), rootSpan.SpanContext.TraceID())
	}
	if genSpan.Parent.SpanID() != rootSpan.SpanContext.SpanID() {
		t.Fatalf("expected generation span's parent span id to equal the HTTP root span id, got parent=%s root=%s",
			genSpan.Parent.SpanID(), rootSpan.SpanContext.SpanID())
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
