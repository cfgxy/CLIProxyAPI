package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTLPExporterSendsCompleteProviderExchangeWithoutHeaders(t *testing.T) {
	resetProvider(t)
	received := make(chan []byte, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("test-public:test-secret"))
		if r.URL.Path != "/api/public/otel/v1/traces" || r.Header.Get("Authorization") != wantAuth {
			t.Error("OTLP request path or exporter authentication differs")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read OTLP export: %v", err)
		}
		received <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	t.Setenv("LANGFUSE_PUBLIC_KEY", "test-public")
	t.Setenv("LANGFUSE_SECRET_KEY", "test-secret")
	Init(context.Background(), appconfig.ObservabilityConfig{
		Enabled: true,
		Exporter: appconfig.ObservabilityExporterConfig{
			Endpoint: endpoint.URL + "/api/public/otel/v1/traces", Insecure: true,
		},
		Capture: appconfig.ObservabilityCaptureConfig{Upstream: true},
	})
	input := []byte(`{"messages":"` + strings.Repeat("original-context", 500) + `"}`)
	output := []byte(`{"reasoning":"` + strings.Repeat("original-thinking", 500) + `","tool_calls":[{"name":"search"}],"answer":"done"}`)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, input) {
			t.Errorf("actual provider input differs: err=%v", err)
		}
		_, _ = w.Write(output)
	}))
	defer provider.Close()
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	sdkusage.UpstreamCaptureFromContext(ctx).Arm()
	client := &http.Client{Transport: sdkusage.CaptureHTTPTransport(ctx, http.DefaultTransport)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer provider-header-only-secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	usagePlugin{}.HandleUsage(ctx, baseRecord())
	if err := Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wire []byte
	select {
	case wire = <-received:
	default:
		t.Fatal("OTLP exporter never delivered the generation")
	}
	if bytes.Contains(wire, []byte("provider-header-only-secret")) {
		t.Fatal("provider Authorization header leaked into OTLP export")
	}
	var exported collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(wire, &exported); err != nil {
		t.Fatal(err)
	}
	for _, resource := range exported.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				if span.Name != "generation openai" {
					continue
				}
				attrs := make(map[string]string)
				for _, attr := range span.Attributes {
					attrs[attr.Key] = attr.Value.GetStringValue()
				}
				if len(input) <= 4096 || len(output) <= 4096 ||
					attrs["langfuse.observation.input"] != string(input) ||
					attrs["langfuse.observation.output"] != string(output) ||
					attrs["langfuse.observation.model.name"] != "gpt-4o-2024-08-06" {
					t.Fatal("OTLP generation omitted or truncated the actual provider exchange")
				}
				return
			}
		}
	}
	t.Fatal("OTLP generation span is missing")
}
