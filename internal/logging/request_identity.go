package logging

import "context"

type requestIdentityKey struct{}

// RequestIdentity is the request-scoped carrier the observability layer
// registers on the inbound request context. It exists because the two consumers
// of a request's caller and session dimensions live on opposite sides of the
// request lifetime: the HTTP root span ends as soon as the handler chain
// returns, while the usage record describing the same call is handed to the
// usage manager's worker goroutine afterwards.
//
// The interface only exposes the write the execution path performs. Consumers
// that need to read the accumulated identity resolve the carrier with
// RequestIdentityFrom and assert their own concrete type.
type RequestIdentity interface {
	// ObserveSession records the canonical session identity the request was
	// actually routed on. It is called while the request is still open.
	ObserveSession(sessionID, parentSessionID string)
}

// WithRequestIdentity stores the request-scoped identity carrier in ctx. A nil
// carrier leaves ctx untouched so a disabled observability layer registers
// nothing.
func WithRequestIdentity(ctx context.Context, identity RequestIdentity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if identity == nil {
		return ctx
	}
	return context.WithValue(ctx, requestIdentityKey{}, identity)
}

// RequestIdentityFrom returns the identity carrier stored in ctx, or nil when
// the context did not originate from an instrumented HTTP request.
func RequestIdentityFrom(ctx context.Context) RequestIdentity {
	if ctx == nil {
		return nil
	}
	identity, _ := ctx.Value(requestIdentityKey{}).(RequestIdentity)
	return identity
}

// ObserveSession publishes the routed session identity to the carrier stored in
// ctx. It is a no-op when no carrier is registered or the session is empty, so
// the execution path can call it unconditionally.
func ObserveSession(ctx context.Context, sessionID, parentSessionID string) {
	if sessionID == "" {
		return
	}
	if identity := RequestIdentityFrom(ctx); identity != nil {
		identity.ObserveSession(sessionID, parentSessionID)
	}
}
