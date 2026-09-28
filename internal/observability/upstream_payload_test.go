package observability

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
)

func TestUpstreamPayloadAttributesPreserveCallPairsAndBinaryBytes(t *testing.T) {
	snapshot := sdkusage.UpstreamSnapshot{Complete: false, HTTP: []sdkusage.CapturedHTTP{
		{Method: "POST", Status: 503, Request: []byte(`{"input":"first"}`), Response: []byte{0, 255, 1}, RequestComplete: true, ResponseComplete: true},
		{Method: "POST", Status: 200, Request: []byte{255, 2}, Response: []byte(`{"answer":"second"}`), RequestComplete: true, ResponseComplete: false},
	}}
	attrs := upstreamPayloadAttributes(snapshot)
	values := make(map[string]attribute.Value)
	for _, attr := range attrs {
		values[string(attr.Key)] = attr.Value
	}
	if values["cpa.upstream.capture_complete"].AsBool() {
		t.Fatal("partial response was marked complete")
	}
	var requests, responses []struct {
		Body     json.RawMessage `json:"body"`
		Complete bool            `json:"complete"`
		Status   int             `json:"status"`
	}
	if err := json.Unmarshal([]byte(values["langfuse.observation.input"].AsString()), &requests); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(values["langfuse.observation.output"].AsString()), &responses); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(responses) != 2 || responses[0].Status != 503 || responses[1].Status != 200 ||
		!responses[0].Complete || responses[1].Complete {
		t.Fatalf("HTTP calls lost their paired order or completeness: requests=%d responses=%d", len(requests), len(responses))
	}
	var firstInput, secondOutput string
	if err := json.Unmarshal(requests[0].Body, &firstInput); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(responses[1].Body, &secondOutput); err != nil {
		t.Fatal(err)
	}
	var firstOutput, secondInput struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(responses[0].Body, &firstOutput); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(requests[1].Body, &secondInput); err != nil {
		t.Fatal(err)
	}
	if firstInput != `{"input":"first"}` || secondOutput != `{"answer":"second"}` ||
		firstOutput.Encoding != "base64" || firstOutput.Data != base64.StdEncoding.EncodeToString([]byte{0, 255, 1}) ||
		secondInput.Encoding != "base64" || secondInput.Data != base64.StdEncoding.EncodeToString([]byte{255, 2}) {
		t.Fatal("UTF-8 or binary provider payload changed in Langfuse attributes")
	}
}

func TestUpstreamPayloadAttributesExposePartialResponseWithoutChangingText(t *testing.T) {
	attrs := upstreamPayloadAttributes(sdkusage.UpstreamSnapshot{Complete: false, HTTP: []sdkusage.CapturedHTTP{{
		Method: "POST", Status: 200,
		Request: []byte(`{"messages":["full"]}`), Response: []byte("event: reasoning\r\ndata: partial"),
		RequestComplete: true, ResponseComplete: false,
	}}})
	values := make(map[string]attribute.Value)
	for _, attr := range attrs {
		values[string(attr.Key)] = attr.Value
	}
	if values["cpa.upstream.capture_complete"].AsBool() || !values["cpa.upstream.http.request_complete"].AsBool() ||
		values["cpa.upstream.http.response_complete"].AsBool() ||
		values["langfuse.observation.input"].AsString() != `{"messages":["full"]}` ||
		values["langfuse.observation.output"].AsString() != "event: reasoning\r\ndata: partial" {
		t.Fatal("single-call provider text or partial status changed")
	}
}
