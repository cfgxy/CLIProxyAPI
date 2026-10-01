package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type xaiNoUsageCapturePlugin struct {
	records chan sdkusage.Record
}

func (p *xaiNoUsageCapturePlugin) HandleUsage(_ context.Context, record sdkusage.Record) {
	if record.Provider == "xai" && record.Model == "grok-no-usage" && p.records != nil {
		p.records <- record
	}
}

func TestXAIWebsocketPublishesGenerationWithoutProviderUsage(t *testing.T) {
	records := make(chan sdkusage.Record, 1)
	const pluginName = "test-xai-websocket-missing-usage"
	sdkusage.RegisterNamedPlugin(pluginName, &xaiNoUsageCapturePlugin{records: records})
	t.Cleanup(func() { sdkusage.RegisterNamedPlugin(pluginName, &xaiNoUsageCapturePlugin{}) })
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`)); err != nil {
			t.Errorf("write provider response: %v", err)
		}
	}))
	defer server.Close()
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "xai-auth-no-usage", Provider: "xai", Attributes: map[string]string{
		"base_url": server.URL, "websockets": "true",
	}, Metadata: map[string]any{"access_token": "xai-test"}}
	req := cliproxyexecutor.Request{Model: "grok-no-usage", Payload: []byte(`{"model":"grok-no-usage","input":"hello"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatCodex, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	select {
	case record := <-records:
		if record.Detail.TotalTokens != 0 || record.Failed {
			t.Fatalf("usage-free completion should still publish a successful generation: total=%d failed=%t", record.Detail.TotalTokens, record.Failed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("xAI completion without usage did not publish a generation")
	}
}

func TestXAIWebsocketCancellationPublishesPartialGeneration(t *testing.T) {
	records := make(chan sdkusage.Record, 1)
	const pluginName = "test-xai-websocket-cancel"
	sdkusage.RegisterNamedPlugin(pluginName, &xaiNoUsageCapturePlugin{records: records})
	t.Cleanup(func() { sdkusage.RegisterNamedPlugin(pluginName, &xaiNoUsageCapturePlugin{}) })
	requestSent := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		close(requestSent)
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(base))
	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "xai-auth-cancel", Provider: "xai", Attributes: map[string]string{
		"base_url": server.URL, "websockets": "true",
	}, Metadata: map[string]any{"access_token": "xai-test"}}
	req := cliproxyexecutor.Request{Model: "grok-no-usage", Payload: []byte(`{"model":"grok-no-usage","input":"cancel me"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatCodex, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestSent:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not receive request")
	}
	cancel()
	for range stream.Chunks {
	}
	select {
	case record := <-records:
		if !record.Failed {
			t.Fatal("canceled response should publish a failed generation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled xAI response did not publish a generation")
	}
	finished := make(chan sdkusage.UpstreamSnapshot, 1)
	sdkusage.UpstreamCaptureFromContext(ctx).OnComplete(func(snapshot sdkusage.UpstreamSnapshot) { finished <- snapshot })
	var got sdkusage.UpstreamSnapshot
	select {
	case got = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled websocket capture did not finish")
	}
	if got.Complete || len(got.WebSocket) != 1 || got.WebSocket[0].Direction != "sent" {
		t.Fatalf("canceled response must preserve the actual request as incomplete: complete=%t frames=%d", got.Complete, len(got.WebSocket))
	}
}

func TestXAIWebsocketStreamCapturesUnmodifiedProviderFrames(t *testing.T) {
	const reasoning = "  {\"type\":\"response.reasoning_text.delta\",\"delta\":\"thinking\"}  "
	const done = `{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		for _, event := range []string{reasoning, done} {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				t.Errorf("write provider event: %v", err)
				return
			}
		}
	}))
	defer server.Close()
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	capture := sdkusage.UpstreamCaptureFromContext(ctx)
	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "xai-auth-1", Provider: "xai", Attributes: map[string]string{
		"base_url": server.URL, "websockets": "true",
	}, Metadata: map[string]any{"access_token": "xai-test"}}
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatCodex, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	var got sdkusage.UpstreamSnapshot
	capture.OnComplete(func(snapshot sdkusage.UpstreamSnapshot) { got = snapshot })
	if !got.Complete || len(got.WebSocket) != 3 ||
		got.WebSocket[0].Direction != "sent" ||
		got.WebSocket[1].Direction != "received" || !bytes.Equal(got.WebSocket[1].Payload, []byte(reasoning)) ||
		!bytes.Equal(got.WebSocket[2].Payload, []byte(done)) {
		t.Fatalf("xAI raw websocket events differ: complete=%t frames=%d", got.Complete, len(got.WebSocket))
	}
}
