package observability

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
)

func sdkusageSnapshotSingle(response, request []byte) sdkusage.UpstreamSnapshot {
	return sdkusage.UpstreamSnapshot{Complete: true, HTTP: []sdkusage.CapturedHTTP{{
		Method: "POST", Status: 200, Request: request, Response: response,
		RequestComplete: true, ResponseComplete: true,
	}}}
}

func attributeValues(attrs []attribute.KeyValue) map[string]attribute.Value {
	values := make(map[string]attribute.Value, len(attrs))
	for _, attr := range attrs {
		values[string(attr.Key)] = attr.Value
	}
	return values
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func assertTextOutput(t *testing.T, value any, ok bool, want string) {
	t.Helper()
	if !ok {
		t.Fatal("semantic extraction failed")
	}
	text, isText := value.(string)
	if !isText || text != want {
		t.Fatalf("expected plain text %q, got %#v", want, value)
	}
}

func assertBlocksOutput(t *testing.T, value any, ok bool) assistantOutput {
	t.Helper()
	if !ok {
		t.Fatal("semantic extraction failed")
	}
	structured, isStructured := value.(assistantOutput)
	if !isStructured || structured.Role != "assistant" || len(structured.Content) == 0 {
		t.Fatalf("expected structured assistant output, got %#v", value)
	}
	return structured
}

func TestSemanticLLMOutputGzippedOpenAIChatText(t *testing.T) {
	body := []byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"你好，世界"},"finish_reason":"stop"}]}`)
	value, ok := semanticLLMOutput(gzipBytes(t, body))
	assertTextOutput(t, value, ok, "你好，世界")
}

func TestSemanticLLMOutputZstdOpenAIChatText(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"zstd text"}}]}`)
	value, ok := semanticLLMOutput(zstdBytes(t, body))
	assertTextOutput(t, value, ok, "zstd text")
}

func TestSemanticLLMOutputOpenAIChatToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`)
	value, ok := semanticLLMOutput(body)
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 1 {
		t.Fatalf("expected one tool call block, got %#v", structured.Content)
	}
	call := structured.Content[0]
	if call.Type != "tool_call" || call.ID != "c1" || call.Name != "get_weather" {
		t.Fatalf("tool call block lost its identity: %#v", call)
	}
	args, isMap := call.Arguments.(map[string]any)
	if !isMap || args["city"] != "Paris" {
		t.Fatalf("tool call arguments were not preserved as structure: %#v", call.Arguments)
	}
}

func TestSemanticLLMOutputAnthropicContentBlocks(t *testing.T) {
	body := []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"tool_use","id":"t1","name":"get_weather","input":{"q":1}}],"stop_reason":"tool_use"}`)
	value, ok := semanticLLMOutput(body)
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 2 {
		t.Fatalf("expected text plus tool_use blocks, got %#v", structured.Content)
	}
	if structured.Content[0].Type != "text" || structured.Content[0].Text != "Hi" {
		t.Fatalf("text block changed: %#v", structured.Content[0])
	}
	if structured.Content[1].Type != "tool_call" || structured.Content[1].Name != "get_weather" {
		t.Fatalf("tool_use block changed: %#v", structured.Content[1])
	}
}

func TestSemanticLLMOutputGeminiParts(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"Hi"}],"role":"model"},"finishReason":"STOP"}]}`)
	value, ok := semanticLLMOutput(body)
	assertTextOutput(t, value, ok, "Hi")
}

func TestSemanticLLMOutputSSEOpenAIChatAggregation(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	value, ok := semanticLLMOutput([]byte(stream))
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 2 {
		t.Fatalf("expected aggregated text plus tool call, got %#v", structured.Content)
	}
	if structured.Content[0].Text != "Hello" {
		t.Fatalf("streamed text deltas were not reassembled: %#v", structured.Content[0])
	}
	if structured.Content[1].Name != "get_weather" {
		t.Fatalf("tool call name lost: %#v", structured.Content[1])
	}
	if args, isMap := structured.Content[1].Arguments.(map[string]any); !isMap || args["city"] != "Paris" {
		t.Fatalf("streamed tool call arguments were not merged: %#v", structured.Content[1].Arguments)
	}
}

func TestSemanticLLMOutputSSEAnthropicAggregation(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"role":"assistant"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"get_weather"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	value, ok := semanticLLMOutput([]byte(stream))
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 2 || structured.Content[0].Text != "Hello" {
		t.Fatalf("anthropic stream blocks were not reassembled: %#v", structured.Content)
	}
	if args, isMap := structured.Content[1].Arguments.(map[string]any); !isMap || args["city"] != "Paris" {
		t.Fatalf("anthropic tool input was not merged: %#v", structured.Content[1].Arguments)
	}
}

func TestSemanticLLMOutputSSEResponsesPrefersCompletedOutput(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``,
		`data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final text"}]},{"type":"function_call","call_id":"c9","name":"lookup","arguments":"{}"}]}}`,
		``,
	}, "\n")
	value, ok := semanticLLMOutput([]byte(stream))
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 2 || structured.Content[0].Text != "final text" {
		t.Fatalf("response.completed output was not preferred: %#v", structured.Content)
	}
	if structured.Content[1].Name != "lookup" || structured.Content[1].ID != "c9" {
		t.Fatalf("response function_call lost identity: %#v", structured.Content[1])
	}
}

func TestSemanticLLMOutputSSEGeminiAggregation(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Sa"}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"yo"}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"a":1}}}],"role":"model"}}]}`,
		``,
	}, "\n")
	value, ok := semanticLLMOutput([]byte(stream))
	structured := assertBlocksOutput(t, value, ok)
	if len(structured.Content) != 2 || structured.Content[0].Text != "Sayo" {
		t.Fatalf("gemini stream text was not reassembled: %#v", structured.Content)
	}
	if structured.Content[1].Name != "lookup" {
		t.Fatalf("gemini functionCall lost: %#v", structured.Content[1])
	}
}

func TestSemanticLLMOutputFailsOpenOnUnrecognizedPayloads(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00, 0xff, 0x01},
		gzipBytes(t, []byte("plain text body")),
		[]byte(`{"error":{"message":"boom"}}`),
		[]byte(`{"unrelated":{"shape":true}}`),
		[]byte("event: reasoning\r\ndata: partial"),
		[]byte("event: message_start\ndata: {broken json"),
	}
	for i, raw := range cases {
		if value, ok := semanticLLMOutput(raw); ok {
			t.Fatalf("case %d unexpectedly extracted %#v", i, value)
		}
	}
}

func TestUpstreamPayloadAttributesSemanticSingleCall(t *testing.T) {
	response := gzipBytes(t, []byte(`{"choices":[{"message":{"role":"assistant","content":"semantic answer"}}]}`))
	snapshot := sdkusageSnapshotSingle(response, []byte(`{"model":"gpt"}`))
	attrs := upstreamPayloadAttributes(snapshot, 0)
	values := attributeValues(attrs)
	if values["langfuse.observation.output"].AsString() != "semantic answer" {
		t.Fatalf("single-call output was not semanticized: %q", values["langfuse.observation.output"].AsString())
	}
	if values["langfuse.observation.input"].AsString() != `{"model":"gpt"}` {
		t.Fatalf("input attribute regressed: %q", values["langfuse.observation.input"].AsString())
	}
}

func TestUpstreamPayloadAttributesSemanticOutputTruncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	response := gzipBytes(t, []byte(`{"choices":[{"message":{"role":"assistant","content":"`+long+`"}}]}`))
	snapshot := sdkusageSnapshotSingle(response, nil)
	attrs := upstreamPayloadAttributes(snapshot, 32)
	values := attributeValues(attrs)
	got := values["langfuse.observation.output"].AsString()
	if len(got) != 32+len("...[truncated]") || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("semantic output ignored the capture byte budget: %q", got)
	}
}

func TestUpstreamPayloadAttributesSemanticMultiCallBodies(t *testing.T) {
	first := gzipBytes(t, []byte(`{"choices":[{"message":{"role":"assistant","content":"first"}}]}`))
	snapshot := sdkusage.UpstreamSnapshot{Complete: true, HTTP: []sdkusage.CapturedHTTP{
		{Method: "POST", Status: 500, Request: []byte(`{}`), Response: first, RequestComplete: true, ResponseComplete: true},
		{Method: "POST", Status: 200, Request: []byte(`{}`), Response: []byte(`{"choices":[{"message":{"role":"assistant","content":"second"}}]}`), RequestComplete: true, ResponseComplete: true},
	}}
	attrs := upstreamPayloadAttributes(snapshot, 0)
	values := attributeValues(attrs)
	var responses []struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal([]byte(values["langfuse.observation.output"].AsString()), &responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[0].Status != 500 || responses[1].Status != 200 {
		t.Fatalf("multi-call pairing regressed: %#v", responses)
	}
	if string(responses[0].Body) != `"first"` || string(responses[1].Body) != `"second"` {
		t.Fatalf("multi-call bodies were not semanticized: %s %s", responses[0].Body, responses[1].Body)
	}
}

func TestUpstreamPayloadAttributesSemanticMultiCallTruncationDegradesToText(t *testing.T) {
	// An over-budget structured assistant output used to be byte-truncated as
	// raw JSON, poisoning the outer Marshal and emptying the whole output
	// attribute; it must degrade to bounded readable text instead.
	big := strings.Repeat("a", 200)
	structured := gzipBytes(t, []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"`+big+`\"}"}}]}}]}`))
	snapshot := sdkusage.UpstreamSnapshot{Complete: true, HTTP: []sdkusage.CapturedHTTP{
		{Method: "POST", Status: 200, Request: []byte(`{}`), Response: structured, RequestComplete: true, ResponseComplete: true},
		{Method: "POST", Status: 200, Request: []byte(`{}`), Response: []byte(`{"choices":[{"message":{"role":"assistant","content":"second"}}]}`), RequestComplete: true, ResponseComplete: true},
	}}
	attrs := upstreamPayloadAttributes(snapshot, 96)
	values := attributeValues(attrs)
	output := values["langfuse.observation.output"].AsString()
	if output == "" {
		t.Fatal("truncated multi-call structured output emptied the whole output attribute")
	}
	var responses []struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal([]byte(output), &responses); err != nil {
		t.Fatalf("output attribute is not valid JSON: %v", err)
	}
	if len(responses) != 2 || responses[0].Status != 200 || responses[1].Status != 200 {
		t.Fatalf("multi-call pairing regressed: %#v", responses)
	}
	degraded := string(responses[0].Body)
	if !strings.Contains(degraded, "lookup") || !strings.Contains(degraded, "...[truncated]") {
		t.Fatalf("over-budget body lost semantic content: %s", degraded)
	}
	if string(responses[1].Body) != `"second"` {
		t.Fatalf("within-budget call regressed: %s", responses[1].Body)
	}
}
