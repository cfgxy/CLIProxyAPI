package observability

import (
	"context"
	"encoding/json"

	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// usagePlugin bridges the existing sdk/cliproxy/usage pub/sub pipeline
// (Layers 2-4: model routing, executor/provider generation, and streaming
// lifecycle) into an OTEL "generation" span, without requiring executor or
// provider code to depend on this package or on any Langfuse/OTLP concept.
//
// usage.Manager already guarantees exactly-once publication per attempt
// (see internal/runtime/executor/helps/usage_helpers.go), covering every
// streaming termination path (normal completion, client/provider disconnect,
// context cancellation, timeout, stream error, and usage-only-in-last-chunk).
// This plugin only needs to translate the resulting Record into a span.
type usagePlugin struct{}

// HandleUsage implements sdkusage.Plugin. It is invoked once per completed
// request attempt with the same ctx that was passed to usage.Publish, which
// carries the OTEL SpanContext started by Middleware for the inbound HTTP
// request, so the generation span it creates here is nested underneath the
// Layer-1 HTTP root span.
func (usagePlugin) HandleUsage(ctx context.Context, record sdkusage.Record) {
	if !Enabled() {
		return
	}
	tracer := activeTracer()

	start := record.RequestedAt
	var startOpt trace.SpanStartOption
	if !start.IsZero() {
		startOpt = trace.WithTimestamp(start)
	}

	spanName := "generation " + record.Provider
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient)}
	if startOpt != nil {
		opts = append(opts, startOpt)
	}
	_, span := tracer.Start(ctx, spanName, opts...)

	attrs := []attribute.KeyValue{
		attribute.String("langfuse.observation.type", "generation"),
		attribute.String("cpa.provider", record.Provider),
		attribute.String("cpa.model.requested", record.Alias),
		attribute.String("cpa.model.resolved", record.Model),
		attribute.Bool("cpa.stream", record.Stream),
	}
	if record.Model != "" {
		attrs = append(attrs, attribute.String("langfuse.observation.model.name", record.Model))
	}
	if record.ResponseModel != "" {
		attrs = append(attrs, attribute.String("cpa.model.response", record.ResponseModel))
	}
	if record.TraceID != "" {
		attrs = append(attrs, attribute.String("cpa.request_id", record.TraceID))
	}
	if record.RequestID != "" {
		attrs = append(attrs, attribute.String("cpa.execution_id", record.RequestID))
	}
	if record.Latency > 0 {
		attrs = append(attrs, attribute.Int64("cpa.latency_ms", record.Latency.Milliseconds()))
	}
	if record.TTFT > 0 {
		attrs = append(attrs, attribute.Int64("cpa.ttft_ms", record.TTFT.Milliseconds()))
	}
	if record.Detail.InputTokens > 0 {
		attrs = append(attrs, attribute.Int64("cpa.usage.input_tokens", record.Detail.InputTokens))
	}
	if record.Detail.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int64("cpa.usage.output_tokens", record.Detail.OutputTokens))
	}
	if record.Detail.TotalTokens > 0 {
		attrs = append(attrs, attribute.Int64("cpa.usage.total_tokens", record.Detail.TotalTokens))
	}
	if record.Detail.CachedTokens > 0 {
		attrs = append(attrs, attribute.Int64("cpa.usage.cached_tokens", record.Detail.CachedTokens))
	}
	if record.Detail.ReasoningTokens > 0 {
		attrs = append(attrs, attribute.Int64("cpa.usage.reasoning_tokens", record.Detail.ReasoningTokens))
	}
	if usageDetails := langfuseUsageDetails(record); usageDetails != "" {
		attrs = append(attrs, attribute.String("langfuse.observation.usage_details", usageDetails))
	}
	attrs = append(attrs, resolveIdentityAttributes(ctx, record)...)
	span.SetAttributes(attrs...)

	if record.Failed {
		span.SetStatus(codes.Error, redactText(record.Fail.Body))
		span.RecordError(providerError{record: record}, trace.WithAttributes(
			attribute.Int("cpa.error.status_code", record.Fail.StatusCode),
			attribute.String("cpa.error.body", redactText(truncate(record.Fail.Body, 2048))),
		))
	} else {
		span.SetStatus(codes.Ok, "")
	}

	endOpts := []trace.SpanEndOption(nil)
	if !start.IsZero() && record.Latency > 0 {
		endOpts = append(endOpts, trace.WithTimestamp(start.Add(record.Latency)))
	}
	if capture := sdkusage.UpstreamCaptureFromContext(ctx); capture != nil {
		if snapshot, ok := sdkusage.WebSocketTurnSnapshotFromContext(ctx); ok {
			span.SetAttributes(upstreamPayloadAttributes(snapshot)...)
			span.End(endOpts...)
			return
		}
		capture.OnComplete(func(snapshot sdkusage.UpstreamSnapshot) {
			span.SetAttributes(upstreamPayloadAttributes(snapshot)...)
			span.End(endOpts...)
		})
		return
	}
	span.End(endOpts...)
}

// langfuseUsageDetails uses exclusive billing buckets. Reasoning remains part
// of output because some model definitions price only output; its diagnostic
// count is separately available in cpa.usage.reasoning_tokens.
func langfuseUsageDetails(record sdkusage.Record) string {
	breakdown := sdkusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown
	if !breakdown.Valid() || breakdown.TotalTokens == 0 {
		return ""
	}
	counts := map[string]int64{"total": breakdown.TotalTokens}
	if breakdown.Quality != sdkusage.TokenAccountingQualityInconsistent {
		for key, value := range map[string]int64{
			"input":                breakdown.Input.UncachedTokens,
			"input_cache_read":     breakdown.Input.CacheReadTokens,
			"input_cache_creation": breakdown.Input.CacheWriteTokens,
			"output":               breakdown.Output.TotalTokens,
		} {
			if value > 0 {
				counts[key] = value
			}
		}
	}
	if breakdown.UnclassifiedTokens > 0 {
		counts["unclassified"] = breakdown.UnclassifiedTokens
	}
	encoded, err := json.Marshal(counts)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// resolveIdentityAttributes merges the caller dimensions resolved for the
// inbound request with the session the usage record reports, which is the one
// the request was actually routed on and therefore wins over the one recognized
// at ingress.
//
// Nothing is written back to the holder: this function runs on the usage
// manager's worker goroutine, typically after the request has returned and its
// root span has been ended, so a write-back could not reach that span. The root
// span gets the routed session synchronously instead, from the execution path
// via logging.ObserveSession.
//
// A record published outside an HTTP request (no holder on the context) still
// produces a complete attribute set: user.id falls back to the anonymous
// literal rather than being omitted.
func resolveIdentityAttributes(ctx context.Context, record sdkusage.Record) []attribute.KeyValue {
	id := identityHolderFrom(ctx).snapshot()
	if sessionID := coresession.NormalizeToCanonicalUUID(record.SessionID); sessionID != "" {
		parentID := coresession.NormalizeToCanonicalUUID(record.ParentSessionID)
		if parentID == sessionID {
			parentID = ""
		}
		id.sessionID = sessionID
		id.parentSessionID = parentID
	}
	return identityAttributes(id, activePlaintextUserID())
}

// providerError adapts a failed usage.Record into an error value so it can be
// passed to span.RecordError without leaking any credential the upstream
// response body might otherwise have carried (the message is redacted).
type providerError struct {
	record sdkusage.Record
}

func (e providerError) Error() string {
	return redactText(e.record.Fail.Body)
}
