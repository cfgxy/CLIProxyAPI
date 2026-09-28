package observability

import (
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Middleware returns the Layer-1 HTTP root-span gin middleware described by
// ADR-001 chapter 8. When observability is disabled it returns a handler that
// only calls c.Next() and does not create any span, exporter interaction, or
// background work.
//
// The root span's SpanContext is propagated through the request context so
// that generation spans recorded later via RecordGeneration (Layers 2-4) are
// nested underneath it, bridging into the same OTEL trace as the HTTP
// request. Correlation with the existing internal/logging request-id/
// cpa_trace mechanism is made explicit by recording that request ID as a span
// attribute rather than minting a second, unrelated identity.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Enabled() {
			c.Next()
			return
		}
		tracer := activeTracer()

		ctx, span := tracer.Start(c.Request.Context(), spanNameForRequest(c),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		requestID := internallogging.GetGinRequestID(c)
		if requestID == "" {
			requestID = internallogging.GetRequestID(ctx)
		}
		if requestID != "" {
			span.SetAttributes(attribute.String("cpa.request_id", requestID))
			ctx = internallogging.WithRequestID(ctx, requestID)
		}

		// The identity holder is published on the request context before the
		// handler chain runs, so both the execution path (which reports the
		// session the request is actually routed on) and the usage plugin
		// (which records the generation span, possibly after this request has
		// returned) share the dimensions resolved for this request.
		holder := newIdentityHolder(c)
		ctx = withIdentityHolder(ctx, holder)
		if activeUpstreamCaptureEnabled() {
			ctx = sdkusage.WithUpstreamCaptureEnabled(ctx)
		}

		c.Request = c.Request.WithContext(ctx)

		captureInput, captureOutput, maxBytes := activeCaptureSettings()
		var rawBody []byte
		if captureInput || bodyCarriesIdentity(c.Request) {
			rawBody = readAndRestoreBody(c)
		}
		if captureInput {
			if body := captureRequestBody(rawBody, maxBytes); body != "" {
				span.SetAttributes(attribute.String("cpa.capture.input", body))
			}
		}
		holder.seed(extractIdentity(c.Request.Header, rawBody))
		var respWriter *captureResponseWriter
		if captureOutput {
			respWriter = newCaptureResponseWriter(c.Writer, maxBytes)
			c.Writer = respWriter
		}

		start := time.Now()
		c.Next()
		latency := time.Since(start)

		if respWriter != nil {
			if body := respWriter.captured(); body != "" {
				span.SetAttributes(attribute.String("cpa.capture.output", body))
			}
		}

		// Resolved after the handler chain: the caller credential is only set by
		// the auth middleware, and the routed session is observed from inside
		// the execution path. Both happen while the request is still open, which
		// is what lets this span report them before it ends.
		span.SetAttributes(identityAttributes(holder.snapshot(), activePlaintextUserID())...)

		status := c.Writer.Status()
		span.SetAttributes(
			attribute.String("http.method", c.Request.Method),
			attribute.String("http.route", c.FullPath()),
			attribute.Int("http.status_code", status),
			attribute.Int64("http.latency_ms", latency.Milliseconds()),
		)
		if status >= 500 {
			span.SetStatus(codes.Error, "http 5xx")
		} else if status >= 400 {
			span.SetStatus(codes.Error, "http 4xx")
		}
		for _, ginErr := range c.Errors {
			span.RecordError(ginErr.Err, trace.WithAttributes(
				attribute.String("error.message", redactText(ginErr.Error())),
			))
		}
	}
}

func spanNameForRequest(c *gin.Context) string {
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}
	return c.Request.Method + " " + path
}
