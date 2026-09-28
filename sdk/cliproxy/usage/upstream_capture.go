package usage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
)

type upstreamCaptureEnabledKey struct{}
type upstreamCaptureKey struct{}
type upstreamTurnSnapshotKey struct{}

// WithUpstreamCaptureEnabled opts an inbound request into provider-body capture.
// The separate attempt context prevents a retry from inheriting the last call's body.
func WithUpstreamCaptureEnabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamCaptureEnabledKey{}, true)
}

// PropagateUpstreamCaptureEnabled carries only the request's opt-in setting,
// never a prior provider attempt's buffered payload, across a detached context.
func PropagateUpstreamCaptureEnabled(ctx, requestCtx context.Context) context.Context {
	if requestCtx == nil || requestCtx.Value(upstreamCaptureEnabledKey{}) != true {
		return ctx
	}
	return WithUpstreamCaptureEnabled(ctx)
}

func WithUpstreamCaptureAttempt(ctx context.Context) context.Context {
	if ctx == nil || ctx.Value(upstreamCaptureEnabledKey{}) != true {
		return ctx
	}
	return context.WithValue(ctx, upstreamCaptureKey{}, &UpstreamCapture{})
}

func UpstreamCaptureFromContext(ctx context.Context) *UpstreamCapture {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(upstreamCaptureKey{}).(*UpstreamCapture)
	return capture
}

// WebSocketTurnSnapshotFromContext returns a provider turn captured at the
// moment its usage record was published, independent of later socket messages.
func WebSocketTurnSnapshotFromContext(ctx context.Context) (UpstreamSnapshot, bool) {
	if ctx == nil {
		return UpstreamSnapshot{}, false
	}
	snapshot, ok := ctx.Value(upstreamTurnSnapshotKey{}).(UpstreamSnapshot)
	return snapshot, ok
}

// CapturedHTTP describes exactly the application-level bytes consumed by the
// transport and the caller, before any response translation or event parsing.
type CapturedHTTP struct {
	Method           string
	Status           int
	Request          []byte
	Response         []byte
	RequestComplete  bool
	ResponseComplete bool
}

type CapturedWebSocketMessage struct {
	Direction string
	Opcode    int
	Payload   []byte
}

type UpstreamSnapshot struct {
	HTTP      []CapturedHTTP
	WebSocket []CapturedWebSocketMessage
	Complete  bool
}

// UpstreamCapture is scoped to one routed provider attempt. It never stores
// authentication headers, and its capture path is inert until a reporter arms it.
type UpstreamCapture struct {
	mu         sync.Mutex
	armed      bool
	http       []*CapturedHTTP
	websocket  []*CapturedWebSocketMessage
	wsOpen     bool
	wsComplete bool
	active     int
	listeners  []func(UpstreamSnapshot)
}

func (c *UpstreamCapture) Arm() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *UpstreamCapture) isArmed() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed
}

func (c *UpstreamCapture) beginHTTP(method string, contentLength int64, hasBody bool) *capturedHTTPCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	call := &CapturedHTTP{Method: method, RequestComplete: !hasBody}
	c.http = append(c.http, call)
	c.active++
	return &capturedHTTPCall{capture: c, value: call, contentLength: contentLength}
}

// OnComplete invokes fn when all observed HTTP bodies and the active WebSocket
// turn have finished. It never blocks the provider's read or write operations.
func (c *UpstreamCapture) OnComplete(fn func(UpstreamSnapshot)) {
	if c == nil || fn == nil {
		return
	}
	c.mu.Lock()
	if c.active != 0 {
		c.listeners = append(c.listeners, fn)
		c.mu.Unlock()
		return
	}
	snapshot := c.snapshotLocked()
	c.mu.Unlock()
	fn(snapshot)
}

func (c *UpstreamCapture) snapshotLocked() UpstreamSnapshot {
	snapshot := UpstreamSnapshot{Complete: true}
	for _, call := range c.http {
		copyCall := *call
		copyCall.Request = bytes.Clone(call.Request)
		copyCall.Response = bytes.Clone(call.Response)
		snapshot.HTTP = append(snapshot.HTTP, copyCall)
		if !call.RequestComplete || !call.ResponseComplete {
			snapshot.Complete = false
		}
	}
	for _, frame := range c.websocket {
		copyFrame := *frame
		copyFrame.Payload = bytes.Clone(frame.Payload)
		snapshot.WebSocket = append(snapshot.WebSocket, copyFrame)
	}
	if c.active != 0 {
		snapshot.Complete = false
	}
	if len(c.websocket) > 0 && !c.wsComplete {
		snapshot.Complete = false
	}
	return snapshot
}

func (c *UpstreamCapture) notifyFinished() {
	c.mu.Lock()
	if c.active != 0 || len(c.listeners) == 0 {
		c.mu.Unlock()
		return
	}
	snapshot := c.snapshotLocked()
	listeners := c.listeners
	c.listeners = nil
	c.mu.Unlock()
	for _, listener := range listeners {
		listener(snapshot)
	}
}

func (c *UpstreamCapture) RecordWebSocket(direction string, opcode int, payload []byte) {
	if !c.isArmed() {
		return
	}
	c.mu.Lock()
	if !c.wsOpen {
		c.wsOpen = true
		c.active++
	}
	c.websocket = append(c.websocket, &CapturedWebSocketMessage{
		Direction: direction, Opcode: opcode, Payload: bytes.Clone(payload),
	})
	c.mu.Unlock()
}

// BeginWebSocketSend reserves the send position before the network write. A
// concurrent provider response cannot appear before the request that caused it.
// The returned callback discards a send that the websocket never accepted.
func (c *UpstreamCapture) BeginWebSocketSend(opcode int, payload []byte) func(bool) {
	if !c.isArmed() {
		return func(bool) {}
	}
	c.mu.Lock()
	if !c.wsOpen {
		c.wsOpen = true
		c.active++
	}
	frame := &CapturedWebSocketMessage{Direction: "sent", Opcode: opcode, Payload: bytes.Clone(payload)}
	c.websocket = append(c.websocket, frame)
	c.mu.Unlock()
	return func(sent bool) {
		if sent {
			return
		}
		c.mu.Lock()
		for i, item := range c.websocket {
			if item == frame {
				c.websocket = append(c.websocket[:i], c.websocket[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}
}

// CaptureWebSocketTurn attaches the messages since the previous terminal event
// to exactly one usage record while the duplex connection remains open.
func CaptureWebSocketTurn(ctx context.Context, complete bool) context.Context {
	c := UpstreamCaptureFromContext(ctx)
	if c == nil {
		return ctx
	}
	c.mu.Lock()
	if len(c.websocket) == 0 {
		c.mu.Unlock()
		return ctx
	}
	snapshot := UpstreamSnapshot{Complete: complete}
	for _, frame := range c.websocket {
		copyFrame := *frame
		copyFrame.Payload = bytes.Clone(frame.Payload)
		snapshot.WebSocket = append(snapshot.WebSocket, copyFrame)
	}
	c.websocket = nil
	c.mu.Unlock()
	return context.WithValue(ctx, upstreamTurnSnapshotKey{}, snapshot)
}

func (c *UpstreamCapture) FinishWebSocket(complete bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.wsOpen {
		c.mu.Unlock()
		return
	}
	c.wsOpen = false
	c.wsComplete = complete
	c.active--
	c.mu.Unlock()
	c.notifyFinished()
}

// CaptureHTTPTransport wraps the selected transport without reading ahead,
// mutating a request payload, or changing the response body seen by the caller.
func CaptureHTTPTransport(ctx context.Context, base http.RoundTripper) http.RoundTripper {
	capture := UpstreamCaptureFromContext(ctx)
	if capture == nil {
		return base
	}
	selected := base
	if base == nil {
		base = http.DefaultTransport
	}
	return upstreamCaptureTransport{base: base, selected: selected, capture: capture}
}

// UnwrapCapturedHTTPTransport returns the selected transport so callers that
// customize its protocol can do so before installing capture on the final client.
func UnwrapCapturedHTTPTransport(transport http.RoundTripper) http.RoundTripper {
	if wrapped, ok := transport.(upstreamCaptureTransport); ok {
		return wrapped.selected
	}
	return transport
}

type upstreamCaptureTransport struct {
	base     http.RoundTripper
	selected http.RoundTripper
	capture  *UpstreamCapture
}

func (t upstreamCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.capture.isArmed() {
		return t.base.RoundTrip(req)
	}
	call := t.capture.beginHTTP(req.Method, req.ContentLength, req.Body != nil && req.Body != http.NoBody)
	outbound := *req
	if !call.value.RequestComplete {
		outbound.Body = &capturingReadCloser{ReadCloser: req.Body, onRead: call.addRequest, onEnd: call.finishRequest}
	}
	resp, err := t.base.RoundTrip(&outbound)
	if err != nil || resp == nil || resp.Body == nil {
		call.finishResponse(false)
		return resp, err
	}
	call.setStatus(resp.StatusCode)
	resp.Body = &capturingReadCloser{ReadCloser: resp.Body, onRead: call.addResponse, onEnd: call.finishResponse}
	return resp, nil
}

type capturedHTTPCall struct {
	capture       *UpstreamCapture
	value         *CapturedHTTP
	contentLength int64
	once          sync.Once
}

func (c *capturedHTTPCall) addRequest(data []byte) {
	c.capture.mu.Lock()
	c.value.Request = append(c.value.Request, data...)
	c.capture.mu.Unlock()
}

func (c *capturedHTTPCall) finishRequest(eof bool) {
	c.capture.mu.Lock()
	c.value.RequestComplete = c.value.RequestComplete || eof || (c.contentLength >= 0 && int64(len(c.value.Request)) == c.contentLength)
	c.capture.mu.Unlock()
}

func (c *capturedHTTPCall) addResponse(data []byte) {
	c.capture.mu.Lock()
	c.value.Response = append(c.value.Response, data...)
	c.capture.mu.Unlock()
}

func (c *capturedHTTPCall) setStatus(status int) {
	c.capture.mu.Lock()
	c.value.Status = status
	c.capture.mu.Unlock()
}

func (c *capturedHTTPCall) finishResponse(eof bool) {
	c.once.Do(func() {
		c.capture.mu.Lock()
		c.value.ResponseComplete = eof
		c.capture.active--
		c.capture.mu.Unlock()
		c.capture.notifyFinished()
	})
}

type capturingReadCloser struct {
	io.ReadCloser
	onRead func([]byte)
	onEnd  func(bool)
}

func (r *capturingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.onRead(p[:n])
	}
	if err != nil {
		r.onEnd(err == io.EOF)
	}
	return n, err
}

func (r *capturingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.onEnd(false)
	return err
}
