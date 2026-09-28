package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type duplexCapturePlugin struct {
	turns chan sdkusage.UpstreamSnapshot
}

func (p *duplexCapturePlugin) HandleUsage(ctx context.Context, record sdkusage.Record) {
	if p == nil || p.turns == nil || record.Model != "gpt-6-astra" {
		return
	}
	if turn, ok := sdkusage.WebSocketTurnSnapshotFromContext(ctx); ok {
		p.turns <- turn
	}
}

func TestCodexWebsocketCapturesActualUpstreamMessages(t *testing.T) {
	const reasoning = "  {\"type\":\"response.reasoning.delta\",\"delta\":\"thinking\"} \n"
	const tool = `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"search","arguments":"{}"}}`
	const done = `{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	sent := make(chan []byte, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, actual, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		sent <- bytes.Clone(actual)
		for _, payload := range []string{reasoning, tool, done} {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write provider event: %v", err)
				return
			}
		}
	}))
	defer server.Close()

	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	capture := sdkusage.UpstreamCaptureFromContext(ctx)
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{
		"api_key": "sk-test", "base_url": server.URL, "auth_kind": "oauth",
	}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"` + strings.Repeat("context", 800) + `"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
	if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	actual := <-sent
	var got sdkusage.UpstreamSnapshot
	capture.OnComplete(func(snapshot sdkusage.UpstreamSnapshot) { got = snapshot })
	if !got.Complete || len(got.WebSocket) != 4 ||
		got.WebSocket[0].Direction != "sent" || got.WebSocket[0].Opcode != websocket.TextMessage || !bytes.Equal(got.WebSocket[0].Payload, actual) ||
		got.WebSocket[1].Direction != "received" || string(got.WebSocket[1].Payload) != reasoning ||
		got.WebSocket[2].Direction != "received" || string(got.WebSocket[2].Payload) != tool ||
		got.WebSocket[3].Direction != "received" || string(got.WebSocket[3].Payload) != done || len(actual) <= 4096 {
		t.Fatalf("upstream websocket bytes or order differ: complete=%t frames=%d provider_request_bytes=%d", got.Complete, len(got.WebSocket), len(actual))
	}
}

func TestCodexWebsocketFailureMarksUpstreamResponseIncomplete(t *testing.T) {
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
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0, 255, 1}); err != nil {
			t.Errorf("write provider response: %v", err)
		}
	}))
	defer server.Close()
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(context.Background()))
	capture := sdkusage.UpstreamCaptureFromContext(ctx)
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{
		"api_key": "sk-test", "base_url": server.URL, "auth_kind": "oauth",
	}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`)}
	if _, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}); err == nil {
		t.Fatal("unexpected binary response should fail")
	}
	var got sdkusage.UpstreamSnapshot
	capture.OnComplete(func(snapshot sdkusage.UpstreamSnapshot) { got = snapshot })
	if got.Complete || len(got.WebSocket) != 2 || got.WebSocket[1].Opcode != websocket.BinaryMessage ||
		!bytes.Equal(got.WebSocket[1].Payload, []byte{0, 255, 1}) {
		t.Fatalf("failed websocket turn must preserve the actual partial response: complete=%t frames=%d", got.Complete, len(got.WebSocket))
	}
}

func TestCodexWebsocketStreamCapturesProviderFrameUntilTerminal(t *testing.T) {
	const reasoning = " \n{\"type\":\"response.reasoning.delta\",\"delta\":\"thinking\"}\r\n "
	const done = `{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
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
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{
		"api_key": "sk-test", "base_url": server.URL, "auth_kind": "oauth",
	}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
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
	if !got.Complete || len(got.WebSocket) != 3 || string(got.WebSocket[1].Payload) != reasoning || string(got.WebSocket[2].Payload) != done {
		t.Fatalf("stream was not captured through its terminal event: complete=%t frames=%d", got.Complete, len(got.WebSocket))
	}
}

func TestCodexDuplexPublishesOnlyCurrentProviderTurn(t *testing.T) {
	turns := make(chan sdkusage.UpstreamSnapshot, 8)
	sdkusage.RegisterNamedPlugin("test-codex-duplex-upstream-capture", &duplexCapturePlugin{turns: turns})
	t.Cleanup(func() { sdkusage.RegisterNamedPlugin("test-codex-duplex-upstream-capture", &duplexCapturePlugin{}) })
	const firstDone = `{"type":"response.completed","response":{"id":"turn-1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
	const secondDone = `{"type":"response.completed","response":{"id":"turn-2","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
	created := []string{
		`{"type":"response.created","response":{"id":"turn-1","output":[]}}`,
		`{"type":"response.created","response":{"id":"turn-2","output":[]}}`,
	}
	actual := make(chan []byte, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for n, done := range []string{firstDone, secondDone} {
			_, input, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read turn %d: %v", n, err)
				return
			}
			actual <- bytes.Clone(input)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(created[n])); err != nil {
				t.Errorf("write created: %v", err)
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(done)); err != nil {
				t.Errorf("write completed: %v", err)
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan cliproxyexecutor.WebsocketInput, 1)
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(base), input)))
	exec := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{ResponseSteering: true}})
	auth := &cliproxyauth.Auth{ID: "duplex-capture-auth", Provider: "codex", Attributes: map[string]string{
		"api_key": "sk-test", "base_url": server.URL, "websockets": "true",
	}}
	req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"first"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if gjson.GetBytes(chunk.Payload, "type").String() == "response.completed" {
			if gjson.GetBytes(chunk.Payload, "response.id").String() == "turn-1" {
				input <- cliproxyexecutor.WebsocketInput{Payload: []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"turn-1","input":"second"}`)}
			} else {
				cancel()
			}
		}
	}
	for n, done := range []string{firstDone, secondDone} {
		select {
		case turn := <-turns:
			sent := <-actual
			if !turn.Complete || len(turn.WebSocket) != 3 || !bytes.Equal(turn.WebSocket[0].Payload, sent) ||
				!bytes.Equal(turn.WebSocket[2].Payload, []byte(done)) {
				t.Fatalf("provider turn %d mixed with another: complete=%t frames=%d", n+1, turn.Complete, len(turn.WebSocket))
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("provider turn %d was not published before socket exit", n+1)
		}
	}
}

func TestCodexDuplexCancellationPublishesIncompleteTurn(t *testing.T) {
	turns := make(chan sdkusage.UpstreamSnapshot, 2)
	const pluginName = "test-codex-duplex-cancel-capture"
	sdkusage.RegisterNamedPlugin(pluginName, &duplexCapturePlugin{turns: turns})
	t.Cleanup(func() { sdkusage.RegisterNamedPlugin(pluginName, &duplexCapturePlugin{}) })
	const created = `{"type":"response.created","response":{"id":"cancel-1","output":[]}}`
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
		if err := conn.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
			t.Errorf("write provider event: %v", err)
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan cliproxyexecutor.WebsocketInput)
	ctx := sdkusage.WithUpstreamCaptureAttempt(sdkusage.WithUpstreamCaptureEnabled(cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(base), input)))
	exec := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{ResponseSteering: true}})
	auth := &cliproxyauth.Auth{ID: "duplex-cancel-auth", Provider: "codex", Attributes: map[string]string{
		"api_key": "sk-test", "base_url": server.URL, "websockets": "true",
	}}
	req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"cancel"}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if gjson.GetBytes(chunk.Payload, "type").String() == "response.created" {
			cancel()
		}
	}
	select {
	case turn := <-turns:
		if turn.Complete || len(turn.WebSocket) < 2 || turn.WebSocket[0].Direction != "sent" ||
			turn.WebSocket[1].Direction != "received" || string(turn.WebSocket[1].Payload) != created {
			t.Fatalf("canceled turn should preserve actual provider messages as incomplete: complete=%t frames=%d", turn.Complete, len(turn.WebSocket))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Codex duplex turn did not publish a generation")
	}
}
