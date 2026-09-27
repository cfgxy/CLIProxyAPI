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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testAPIKey = "test-caller-api-key"

// initIdentityProvider activates an in-memory provider with an explicit
// plaintext-user-id setting so the security switch can be exercised end to end.
func initIdentityProvider(t *testing.T, plaintextUserID bool) *tracetest.InMemoryExporter {
	t.Helper()
	resetProvider(t)
	exp := tracetest.NewInMemoryExporter()
	initProvider(context.Background(), settings{
		enabled:         true,
		serviceName:     "test",
		exportTimeout:   time.Second,
		maxQueueSize:    64,
		maxBatchSize:    64,
		batchTimeout:    time.Hour,
		shutdownTimeout: time.Second,
		plaintextUserID: plaintextUserID,
	}, exp)
	return exp
}

// newIdentityRouter mirrors the production middleware order and adds a stand-in
// for the auth middleware, which runs after observability.Middleware and is the
// only place the caller API key becomes available.
func newIdentityRouter(t *testing.T, apiKey string, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(internallogging.GinLogrusLogger())
	r.Use(internallogging.CPATraceIDMiddleware())
	r.Use(Middleware())
	r.Use(func(c *gin.Context) {
		if apiKey != "" {
			c.Set("userApiKey", apiKey)
		}
		c.Next()
	})
	r.POST("/v1/messages", handler)
	return r
}

func postJSON(r *gin.Engine, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func spanAttrs(t *testing.T, span tracetest.SpanStub) map[string]string {
	t.Helper()
	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	return attrs
}

func spanNamed(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no span named %q among %d spans", name, len(spans))
	return tracetest.SpanStub{}
}

// TestMiddleware_RootSpanCarriesSessionAndUser is deliverable P1 for the HTTP
// root span: the two attributes Langfuse reads must be present on it.
func TestMiddleware_RootSpanCarriesSessionAndUser(t *testing.T) {
	exp := initIdentityProvider(t, false)

	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	body := `{"model":"claude-sonnet-4","metadata":{"user_id":"{\"account_uuid\":\"` + testAccountUUID + `\",\"session_id\":\"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\"}"}}`
	if w := postJSON(router, body, nil); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	forceFlush(t)

	attrs := spanAttrs(t, spanNamed(t, exp.GetSpans(), "POST /v1/messages"))
	if attrs[attrSessionID] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("session.id = %q", attrs[attrSessionID])
	}
	if attrs[attrUserID] == "" {
		t.Fatal("user.id missing from the HTTP root span")
	}
	if attrs["cpa.user.id_source"] != userIDSourceAccount {
		t.Fatalf("cpa.user.id_source = %q, want %q", attrs["cpa.user.id_source"], userIDSourceAccount)
	}
	if attrs["cpa.client.type"] != "claude" {
		t.Fatalf("cpa.client.type = %q, want claude", attrs["cpa.client.type"])
	}
}

// TestMiddleware_RootSpanFallsBackToCallerScope covers the Deerflow / ZCode
// situation: no client identity in the request, so user.id must come from the
// caller credential rather than collapsing every caller into one user.
func TestMiddleware_RootSpanFallsBackToCallerScope(t *testing.T) {
	exp := initIdentityProvider(t, false)

	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	postJSON(router, `{"model":"gpt-4o","messages":[]}`, nil)
	forceFlush(t)

	attrs := spanAttrs(t, spanNamed(t, exp.GetSpans(), "POST /v1/messages"))
	if attrs["cpa.user.id_source"] != userIDSourceCallerScope {
		t.Fatalf("cpa.user.id_source = %q, want %q", attrs["cpa.user.id_source"], userIDSourceCallerScope)
	}
	want := callerScopeDigestPrefix + shortDigest(coresession.CallerScope(testAPIKey))
	if attrs[attrUserID] != want {
		t.Fatalf("user.id = %q, want %q", attrs[attrUserID], want)
	}
	if strings.Contains(attrs[attrUserID], testAPIKey) {
		t.Fatal("user.id leaked the caller API key")
	}
}

// TestMiddleware_RootSpanAnonymousWithoutAnyIdentity is the terminal level of
// the chain observed through the real middleware: no client identity and no
// caller credential still yields a non-empty user.id.
func TestMiddleware_RootSpanAnonymousWithoutAnyIdentity(t *testing.T) {
	exp := initIdentityProvider(t, false)

	router := newIdentityRouter(t, "", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	postJSON(router, `{"model":"gpt-4o","messages":[]}`, nil)
	forceFlush(t)

	attrs := spanAttrs(t, spanNamed(t, exp.GetSpans(), "POST /v1/messages"))
	if attrs[attrUserID] != anonymousUserID {
		t.Fatalf("user.id = %q, want %q", attrs[attrUserID], anonymousUserID)
	}
	if _, ok := attrs[attrSessionID]; ok {
		t.Fatalf("session.id should be omitted when unknown, got %q", attrs[attrSessionID])
	}
}

// TestMiddleware_PlaintextUserIDRequiresTheSwitch is the security assertion at
// span level: the same request emits a digest by default and the raw account
// identifier only once the switch is on.
func TestMiddleware_PlaintextUserIDRequiresTheSwitch(t *testing.T) {
	body := `{"model":"claude-sonnet-4","metadata":{"user_id":"{\"account_uuid\":\"` + testAccountUUID + `\"}"}}`

	hashedExp := initIdentityProvider(t, false)
	postJSON(newIdentityRouter(t, testAPIKey, func(c *gin.Context) { c.Status(http.StatusOK) }), body, nil)
	forceFlush(t)
	hashed := spanAttrs(t, spanNamed(t, hashedExp.GetSpans(), "POST /v1/messages"))[attrUserID]
	if strings.Contains(hashed, testAccountUUID) {
		t.Fatalf("default user.id %q leaked the raw account identifier", hashed)
	}

	plainExp := initIdentityProvider(t, true)
	postJSON(newIdentityRouter(t, testAPIKey, func(c *gin.Context) { c.Status(http.StatusOK) }), body, nil)
	forceFlush(t)
	plain := spanAttrs(t, spanNamed(t, plainExp.GetSpans(), "POST /v1/messages"))[attrUserID]
	if plain != testAccountUUID {
		t.Fatalf("plaintext user.id = %q, want %q", plain, testAccountUUID)
	}
}

// TestMiddleware_DisabledLeavesRequestUnchanged is the fail-open assertion for
// the mapping layer: with tracing off the handler still sees the full request
// body and no span is produced.
func TestMiddleware_DisabledLeavesRequestUnchanged(t *testing.T) {
	resetProvider(t)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	var seen string
	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("handler could not read the body: %v", err)
		}
		seen = string(raw)
		c.Status(http.StatusOK)
	})
	if w := postJSON(router, body, nil); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if seen != body {
		t.Fatalf("handler saw %q, want %q", seen, body)
	}
}

// TestMiddleware_EnabledRestoresRequestBody guards the extra body read the
// mapping layer introduces: buffering the payload for session recognition must
// not consume it before the handler runs.
func TestMiddleware_EnabledRestoresRequestBody(t *testing.T) {
	initIdentityProvider(t, false)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	var seen string
	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("handler could not read the body: %v", err)
		}
		seen = string(raw)
		c.Status(http.StatusOK)
	})
	postJSON(router, body, nil)
	if seen != body {
		t.Fatalf("handler saw %q, want %q", seen, body)
	}
}

// TestUsagePlugin_GenerationSpanCarriesSessionAndUser is deliverable P1 for the
// generation span: the session the usage record reports wins over the one
// recognized at ingress, because the record carries the identity the request
// was actually routed on.
func TestUsagePlugin_GenerationSpanCarriesSessionAndUser(t *testing.T) {
	exp := initIdentityProvider(t, false)

	routedSession := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		record := baseRecord()
		record.SessionID = routedSession
		record.ParentSessionID = "11111111-1111-1111-1111-111111111111"
		usagePlugin{}.HandleUsage(c.Request.Context(), record)
		c.Status(http.StatusOK)
	})
	postJSON(router, `{"model":"gpt-4o","messages":[]}`, nil)
	forceFlush(t)

	spans := exp.GetSpans()
	generation := spanAttrs(t, spanNamed(t, spans, "generation openai"))
	if generation[attrSessionID] != routedSession {
		t.Fatalf("generation session.id = %q, want %q", generation[attrSessionID], routedSession)
	}
	if generation["cpa.session.parent_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("generation cpa.session.parent_id = %q", generation["cpa.session.parent_id"])
	}
	if generation[attrUserID] == "" {
		t.Fatal("generation span is missing user.id")
	}
}

// TestMiddleware_RootSpanReportsRoutedSessionThroughHandlerCtxChain is the
// production form of the root-span session guarantee. The routed session is
// resolved inside the execution path, on a context that
// BaseAPIHandler.GetContextWithCancel deliberately derives from
// context.Background() rather than from the request context; the notification
// must still reach the span that ends with the request. A hand-built context
// cannot exercise that plumbing, and the write-back the usage plugin used to
// rely on cannot either, because the usage manager hands records to its worker
// goroutine only after the request has returned.
func TestMiddleware_RootSpanReportsRoutedSessionThroughHandlerCtxChain(t *testing.T) {
	exp := initIdentityProvider(t, false)

	ingressSession := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	routedSession := "ctx:v1:deerflow-thread-42"
	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		handler := &handlers.BaseAPIHandler{Cfg: &config.SDKConfig{}}
		cliCtx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
		defer cancel()

		// Stands in for sdk/cliproxy/auth.syncMetadataSessionToContext, which is
		// the single point where the canonical routed session becomes known.
		internallogging.ObserveSession(cliCtx, routedSession, "")
		c.Status(http.StatusOK)
	})
	body := `{"model":"claude-sonnet-4","metadata":{"user_id":"{\"account_uuid\":\"` + testAccountUUID + `\",\"session_id\":\"` + ingressSession + `\"}"}}`
	postJSON(router, body, nil)
	forceFlush(t)

	root := spanAttrs(t, spanNamed(t, exp.GetSpans(), "POST /v1/messages"))
	want := coresession.NormalizeToCanonicalUUID(routedSession)
	if root[attrSessionID] != want {
		t.Fatalf("root session.id = %q, want the routed session %q", root[attrSessionID], want)
	}
	if root[attrSessionID] == ingressSession {
		t.Fatal("root span reported the ingress session instead of the routed one")
	}
}

// awaitPlugin lets the test observe when the usage manager's worker goroutine
// has finished handing a record to the observability plugin.
type awaitPlugin struct {
	done chan struct{}
}

func (p awaitPlugin) HandleUsage(ctx context.Context, record sdkusage.Record) {
	usagePlugin{}.HandleUsage(ctx, record)
	close(p.done)
}

// gatePlugin blocks the manager's single worker goroutine so the test can prove
// the record is handled strictly after the HTTP request has ended.
type gatePlugin struct {
	release chan struct{}
}

func (p gatePlugin) HandleUsage(ctx context.Context, record sdkusage.Record) {
	<-p.release
}

// TestUsagePlugin_GenerationSpanHandledAfterRequestEndMatchesRootSpan drives the
// real dispatch form: the record goes through the usage manager queue and is
// handled by its worker goroutine only after the HTTP request has returned and
// the root span has ended. Both spans must still report the same session and
// the same non-anonymous user.
func TestUsagePlugin_GenerationSpanHandledAfterRequestEndMatchesRootSpan(t *testing.T) {
	exp := initIdentityProvider(t, false)

	routedSession := "ctx:v1:deerflow-thread-77"
	release := make(chan struct{})
	handled := make(chan struct{})
	manager := sdkusage.NewManager(8)
	manager.Register(gatePlugin{release: release})
	manager.Register(awaitPlugin{done: handled})
	t.Cleanup(manager.Stop)

	router := newIdentityRouter(t, testAPIKey, func(c *gin.Context) {
		handler := &handlers.BaseAPIHandler{Cfg: &config.SDKConfig{}}
		cliCtx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
		defer cancel()

		internallogging.ObserveSession(cliCtx, routedSession, "")
		record := baseRecord()
		record.SessionID = routedSession
		manager.Publish(cliCtx, record)
		c.Status(http.StatusOK)
	})
	postJSON(router, `{"model":"gpt-4o","messages":[]}`, nil)

	// The request has returned, so the root span is already ended and closed to
	// further attributes. Only now is the worker allowed to reach the plugin.
	close(release)
	<-handled
	forceFlush(t)

	spans := exp.GetSpans()
	root := spanAttrs(t, spanNamed(t, spans, "POST /v1/messages"))
	generation := spanAttrs(t, spanNamed(t, spans, "generation openai"))
	want := coresession.NormalizeToCanonicalUUID(routedSession)
	if generation[attrSessionID] != want {
		t.Fatalf("generation session.id = %q, want %q", generation[attrSessionID], want)
	}
	if root[attrSessionID] != want {
		t.Fatalf("root session.id = %q, want %q", root[attrSessionID], want)
	}
	if generation[attrUserID] == anonymousUserID {
		t.Fatalf("generation user.id fell back to %q although the caller credential was known", anonymousUserID)
	}
	if generation[attrUserID] != root[attrUserID] {
		t.Fatalf("root and generation spans disagree on user.id: %q vs %q", root[attrUserID], generation[attrUserID])
	}
}

// TestUsagePlugin_NormalizesOpaqueSessionID covers the Deerflow / ZCode
// fallback path: an opaque routed identifier is projected to a canonical UUID
// and different conversations stay distinct.
func TestUsagePlugin_NormalizesOpaqueSessionID(t *testing.T) {
	exp := initIdentityProvider(t, false)

	record := baseRecord()
	record.SessionID = "ctx:v1:deerflow-thread-42"
	usagePlugin{}.HandleUsage(context.Background(), record)
	forceFlush(t)

	attrs := spanAttrs(t, spanNamed(t, exp.GetSpans(), "generation openai"))
	want := coresession.NormalizeToCanonicalUUID("ctx:v1:deerflow-thread-42")
	if attrs[attrSessionID] != want {
		t.Fatalf("session.id = %q, want %q", attrs[attrSessionID], want)
	}
	if attrs[attrSessionID] == "ctx:v1:deerflow-thread-42" {
		t.Fatal("opaque session identifier reached the span unnormalized")
	}
	if attrs[attrUserID] != anonymousUserID {
		t.Fatalf("user.id = %q, want the anonymous fallback outside an HTTP request", attrs[attrUserID])
	}
}

// TestUsagePlugin_EmptyRecognitionIsFailOpen asserts that a record carrying no
// session identity at all still produces a complete, panic-free span.
func TestUsagePlugin_EmptyRecognitionIsFailOpen(t *testing.T) {
	exp := initIdentityProvider(t, false)

	usagePlugin{}.HandleUsage(context.Background(), sdkusage.Record{Provider: "openai"})
	forceFlush(t)

	attrs := spanAttrs(t, spanNamed(t, exp.GetSpans(), "generation openai"))
	if _, ok := attrs[attrSessionID]; ok {
		t.Fatalf("session.id should be omitted when unknown, got %q", attrs[attrSessionID])
	}
	if attrs[attrUserID] != anonymousUserID {
		t.Fatalf("user.id = %q, want %q", attrs[attrUserID], anonymousUserID)
	}
}
