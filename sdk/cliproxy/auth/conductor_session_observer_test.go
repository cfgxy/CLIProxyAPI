package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

type sessionObserverStub struct {
	calls           int
	sessionID       string
	parentSessionID string
}

func (o *sessionObserverStub) ObserveSession(sessionID, parentSessionID string) {
	o.calls++
	o.sessionID = sessionID
	o.parentSessionID = parentSessionID
}

// TestSyncMetadataSessionToContext_NotifiesSessionObserver pins the only
// synchronous point at which the session the request is actually routed on
// becomes known while the inbound HTTP request is still open. Consumers that
// must report that identity before the request ends (the observability root
// span) depend on this notification.
func TestSyncMetadataSessionToContext_NotifiesSessionObserver(t *testing.T) {
	observer := &sessionObserverStub{}
	ctx := logging.WithRequestIdentity(context.Background(), observer)

	metadata := map[string]any{
		cliproxyexecutor.CanonicalSessionIDMetadataKey: "ctx:v1:deerflow-thread-42",
		cliproxyexecutor.ParentSessionIDMetadataKey:    "parent-session-id",
	}
	syncMetadataSessionToContext(ctx, metadata)

	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want 1", observer.calls)
	}
	wantSession := cliproxysession.BoundSessionIdentity("ctx:v1:deerflow-thread-42")
	if observer.sessionID != wantSession {
		t.Fatalf("observed sessionID = %q, want %q", observer.sessionID, wantSession)
	}
	wantParent := cliproxysession.BoundSessionIdentity("parent-session-id")
	if observer.parentSessionID != wantParent {
		t.Fatalf("observed parentSessionID = %q, want %q", observer.parentSessionID, wantParent)
	}
}

// TestSyncMetadataSessionToContext_WithoutSessionKeepsObserverSilent asserts the
// notification is not fired with an empty identity, which would otherwise erase
// the session the ingress side already recognized.
func TestSyncMetadataSessionToContext_WithoutSessionKeepsObserverSilent(t *testing.T) {
	observer := &sessionObserverStub{}
	ctx := logging.WithRequestIdentity(context.Background(), observer)

	syncMetadataSessionToContext(ctx, nil)

	if observer.calls != 0 {
		t.Fatalf("observer calls = %d, want 0", observer.calls)
	}
}
