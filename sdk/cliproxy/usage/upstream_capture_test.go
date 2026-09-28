package usage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamCaptureHTTPPreservesLargeRequestAndRawSSE(t *testing.T) {
	input := []byte(`{"model":"test","messages":"` + strings.Repeat("context", 1000) + `"}`)
	parts := [][]byte{
		[]byte("event: thinking\r\ndata: {\"text\":\"" + strings.Repeat("considering", 500) + "\"}\r\n\r\n"),
		[]byte("event: tool_call\ndata: {\"name\":\"search\"}\n\n"),
		[]byte("event: answer\ndata: final\n\n"),
	}
	output := bytes.Join(parts, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(actual, input) {
			t.Errorf("provider received a different request: err=%v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range parts {
			_, _ = w.Write(part)
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()

	ctx := WithUpstreamCaptureAttempt(WithUpstreamCaptureEnabled(context.Background()))
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	client := &http.Client{Transport: CaptureHTTPTransport(ctx, http.DefaultTransport)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer never-export-this")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []UpstreamSnapshot
	capture.OnComplete(func(snapshot UpstreamSnapshot) { snapshots = append(snapshots, snapshot) })
	if len(snapshots) != 0 {
		t.Fatal("capture ended before the provider response was read")
	}
	actual, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !bytes.Equal(actual, output) || len(snapshots) != 1 || len(snapshots[0].HTTP) != 1 {
		t.Fatalf("response or attempt count mismatch: response=%d snapshots=%d", len(actual), len(snapshots))
	}
	got := snapshots[0].HTTP[0]
	if !snapshots[0].Complete || !got.RequestComplete || !got.ResponseComplete ||
		!bytes.Equal(got.Request, input) || !bytes.Equal(got.Response, output) || len(got.Request) <= 4096 || len(got.Response) <= 4096 {
		t.Fatal("captured request/response differ from the actual provider exchange")
	}
	if bytes.Contains(got.Request, []byte("never-export-this")) || bytes.Contains(got.Response, []byte("never-export-this")) {
		t.Fatal("authorization header entered the captured payload")
	}
}

func TestUpstreamCaptureReportsEarlyCloseInsteadOfClaimingCompleteness(t *testing.T) {
	ctx := WithUpstreamCaptureAttempt(WithUpstreamCaptureEnabled(context.Background()))
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	client := &http.Client{Transport: CaptureHTTPTransport(ctx, stubCaptureTransport{})}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://provider.invalid/model", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 3)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	var got UpstreamSnapshot
	called := false
	capture.OnComplete(func(snapshot UpstreamSnapshot) { got, called = snapshot, true })
	if called {
		t.Fatal("response was prematurely marked complete")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !called || got.Complete || got.HTTP[0].ResponseComplete || string(got.HTTP[0].Response) != "res" {
		t.Fatalf("expected a visible partial provider response, got %+v", got)
	}
}

func TestUpstreamCaptureCancellationKeepsPartialProviderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	base, cancel := context.WithCancel(WithUpstreamCaptureEnabled(context.Background()))
	defer cancel()
	ctx := WithUpstreamCaptureAttempt(base)
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	client := &http.Client{Transport: CaptureHTTPTransport(ctx, http.DefaultTransport)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("sent request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("partial"))
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	var snapshot UpstreamSnapshot
	capture.OnComplete(func(got UpstreamSnapshot) { snapshot = got })
	if snapshot.Complete || len(snapshot.HTTP) != 1 || !snapshot.HTTP[0].RequestComplete ||
		snapshot.HTTP[0].ResponseComplete || string(snapshot.HTTP[0].Request) != "sent request" ||
		string(snapshot.HTTP[0].Response) != "partial" {
		t.Fatal("canceled provider response was lost or marked complete")
	}
}

func TestUpstreamCapturePreservesBinaryBodiesAcrossSequentialHTTPCalls(t *testing.T) {
	inputs := [][]byte{{0, 255, 1}, {255, 1, 0}}
	outputs := [][]byte{{2, 255, 0}, {255, 2, 1}}
	callIndex := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := callIndex
		callIndex++
		body, err := io.ReadAll(r.Body)
		if err != nil || index >= len(inputs) || !bytes.Equal(body, inputs[index]) {
			t.Errorf("provider request %d differs: err=%v", index, err)
			return
		}
		_, _ = w.Write(outputs[index])
	}))
	defer provider.Close()
	ctx := WithUpstreamCaptureAttempt(WithUpstreamCaptureEnabled(context.Background()))
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	client := &http.Client{Transport: CaptureHTTPTransport(ctx, http.DefaultTransport)}
	for i, input := range inputs {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || !bytes.Equal(body, outputs[i]) {
			t.Fatalf("provider response %d differs: err=%v", i, err)
		}
	}
	var got UpstreamSnapshot
	capture.OnComplete(func(snapshot UpstreamSnapshot) { got = snapshot })
	if !got.Complete || len(got.HTTP) != 2 {
		t.Fatalf("sequential calls were not preserved: complete=%t calls=%d", got.Complete, len(got.HTTP))
	}
	for i, call := range got.HTTP {
		if !call.RequestComplete || !call.ResponseComplete || !bytes.Equal(call.Request, inputs[i]) || !bytes.Equal(call.Response, outputs[i]) {
			t.Errorf("provider exchange %d lost binary bytes", i)
		}
	}
}

type stubCaptureTransport struct{}

func (stubCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("response")), Header: make(http.Header)}, nil
}

func TestUpstreamCaptureWebSocketKeepsOrderedMessagesAndRetryIsolation(t *testing.T) {
	ctx := WithUpstreamCaptureEnabled(context.Background())
	first := UpstreamCaptureFromContext(WithUpstreamCaptureAttempt(ctx))
	second := UpstreamCaptureFromContext(WithUpstreamCaptureAttempt(ctx))
	if first == nil || second == nil || first == second {
		t.Fatal("provider retries must have distinct capture instances")
	}
	first.Arm()
	first.RecordWebSocket("sent", 1, []byte(`{"previous_response_id":"r1"}`))
	first.RecordWebSocket("received", 1, []byte(`{"type":"response.reasoning.delta"}`))
	first.RecordWebSocket("received", 2, []byte{0, 255, 1})
	var got UpstreamSnapshot
	called := false
	first.OnComplete(func(snapshot UpstreamSnapshot) { got, called = snapshot, true })
	if called {
		t.Fatal("websocket turn ended before its final message")
	}
	first.FinishWebSocket(true)
	if !called || !got.Complete || len(got.WebSocket) != 3 ||
		got.WebSocket[0].Direction != "sent" || got.WebSocket[1].Direction != "received" ||
		got.WebSocket[2].Opcode != 2 || !bytes.Equal(got.WebSocket[2].Payload, []byte{0, 255, 1}) {
		t.Fatalf("websocket order or bytes changed: %+v", got)
	}
	var other UpstreamSnapshot
	second.OnComplete(func(snapshot UpstreamSnapshot) { other = snapshot })
	if len(other.WebSocket) != 0 {
		t.Fatal("retry inherited messages from a prior provider attempt")
	}
}

func TestUpstreamCaptureOrdersImmediateResponseBehindCompletedSend(t *testing.T) {
	ctx := WithUpstreamCaptureAttempt(WithUpstreamCaptureEnabled(context.Background()))
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	completeSend := capture.BeginWebSocketSend(1, []byte("request"))
	capture.RecordWebSocket("received", 1, []byte("immediate response"))
	completeSend(true)
	failedSend := capture.BeginWebSocketSend(1, []byte("never sent"))
	failedSend(false)
	capture.FinishWebSocket(true)
	var got UpstreamSnapshot
	capture.OnComplete(func(snapshot UpstreamSnapshot) { got = snapshot })
	if !got.Complete || len(got.WebSocket) != 2 ||
		got.WebSocket[0].Direction != "sent" || string(got.WebSocket[0].Payload) != "request" ||
		got.WebSocket[1].Direction != "received" || string(got.WebSocket[1].Payload) != "immediate response" {
		t.Fatalf("the provider response overtook its request or an unsent request was retained: complete=%t frames=%d", got.Complete, len(got.WebSocket))
	}
}

func TestUpstreamCaptureSeparatesDuplexTurnsBeforeSocketCloses(t *testing.T) {
	ctx := WithUpstreamCaptureAttempt(WithUpstreamCaptureEnabled(context.Background()))
	capture := UpstreamCaptureFromContext(ctx)
	capture.Arm()
	capture.RecordWebSocket("sent", 1, []byte("turn 1 input"))
	capture.RecordWebSocket("received", 1, []byte("turn 1 response"))
	firstCtx := CaptureWebSocketTurn(ctx, true)
	first, ok := WebSocketTurnSnapshotFromContext(firstCtx)
	if !ok || !first.Complete || len(first.WebSocket) != 2 {
		t.Fatalf("missing first duplex turn: complete=%t frames=%d", first.Complete, len(first.WebSocket))
	}
	capture.RecordWebSocket("sent", 1, []byte("turn 2 input"))
	capture.RecordWebSocket("received", 1, []byte("turn 2 response"))
	second, ok := WebSocketTurnSnapshotFromContext(CaptureWebSocketTurn(ctx, true))
	if !ok || !second.Complete || len(second.WebSocket) != 2 ||
		string(second.WebSocket[0].Payload) != "turn 2 input" || string(second.WebSocket[1].Payload) != "turn 2 response" ||
		string(first.WebSocket[0].Payload) != "turn 1 input" || string(first.WebSocket[1].Payload) != "turn 1 response" {
		t.Fatalf("duplex turns mixed: first=%d second=%d", len(first.WebSocket), len(second.WebSocket))
	}
	capture.FinishWebSocket(false)
}

func TestUpstreamCaptureNeverInterceptsDisabledContext(t *testing.T) {
	base := stubCaptureTransport{}
	if got := CaptureHTTPTransport(context.Background(), base); got != base {
		t.Fatal("disabled capture altered the selected HTTP transport")
	}
	if capture := UpstreamCaptureFromContext(WithUpstreamCaptureAttempt(context.Background())); capture != nil {
		t.Fatal("disabled context created a capture buffer")
	}
}
