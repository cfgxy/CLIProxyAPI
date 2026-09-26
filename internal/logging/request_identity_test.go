package logging

import (
	"context"
	"sync"
	"testing"
)

type recordingRequestIdentity struct {
	mu              sync.Mutex
	calls           int
	sessionID       string
	parentSessionID string
}

func (o *recordingRequestIdentity) ObserveSession(sessionID, parentSessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	o.sessionID = sessionID
	o.parentSessionID = parentSessionID
}

// TestObserveSession_DeliversRoutedSessionToCarrier covers the synchronous
// hand-off the HTTP root span depends on: the session a request is actually
// routed on is resolved deep inside the execution path, while the span that has
// to report it lives in the middleware and ends when the request ends.
func TestObserveSession_DeliversRoutedSessionToCarrier(t *testing.T) {
	identity := &recordingRequestIdentity{}
	ctx := WithRequestIdentity(context.Background(), identity)

	ObserveSession(ctx, "routed-session", "routed-parent")

	if identity.calls != 1 {
		t.Fatalf("ObserveSession calls = %d, want 1", identity.calls)
	}
	if identity.sessionID != "routed-session" {
		t.Fatalf("sessionID = %q, want %q", identity.sessionID, "routed-session")
	}
	if identity.parentSessionID != "routed-parent" {
		t.Fatalf("parentSessionID = %q, want %q", identity.parentSessionID, "routed-parent")
	}
}

// TestRequestIdentityFrom_ReturnsTheRegisteredCarrier is the retrieval half of
// the bridge: asynchronous consumers resolve the carrier from an execution
// context that was derived from the inbound request.
func TestRequestIdentityFrom_ReturnsTheRegisteredCarrier(t *testing.T) {
	identity := &recordingRequestIdentity{}
	ctx := WithRequestIdentity(context.Background(), identity)

	if got := RequestIdentityFrom(ctx); got != RequestIdentity(identity) {
		t.Fatalf("RequestIdentityFrom returned %#v, want the registered carrier", got)
	}
	if got := RequestIdentityFrom(context.Background()); got != nil {
		t.Fatalf("RequestIdentityFrom on a bare context = %#v, want nil", got)
	}
	if got := RequestIdentityFrom(nil); got != nil {
		t.Fatalf("RequestIdentityFrom(nil) = %#v, want nil", got)
	}
}

// TestObserveSession_WithoutCarrierIsNoop is the fail-open assertion: the
// execution path publishes unconditionally, so every context shape that carries
// no identity must stay silent instead of panicking.
func TestObserveSession_WithoutCarrierIsNoop(t *testing.T) {
	ObserveSession(context.Background(), "routed-session", "")
	ObserveSession(nil, "routed-session", "")

	identity := &recordingRequestIdentity{}
	ctx := WithRequestIdentity(context.Background(), identity)
	ObserveSession(ctx, "", "routed-parent")
	if identity.calls != 0 {
		t.Fatalf("an empty session must not notify the carrier, calls = %d", identity.calls)
	}
}

// TestWithRequestIdentity_NilCarrierLeavesContextUsable guards the seeding side
// of the bridge: a disabled observability layer registers nothing.
func TestWithRequestIdentity_NilCarrierLeavesContextUsable(t *testing.T) {
	ctx := WithRequestIdentity(context.Background(), nil)
	if got := RequestIdentityFrom(ctx); got != nil {
		t.Fatalf("RequestIdentityFrom = %#v, want nil", got)
	}
	ObserveSession(ctx, "routed-session", "")
}
