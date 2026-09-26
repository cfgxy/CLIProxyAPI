package observability

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
)

// Langfuse reads exactly two span attributes to populate its Sessions and
// Users views. Everything else this file emits is an extra trace dimension
// under the cpa.* namespace.
const (
	attrSessionID = "session.id"
	attrUserID    = "user.id"
)

// anonymousUserID terminates the user.id fallback chain. The attribute is
// never emitted empty, otherwise Langfuse drops the call from its Users view
// entirely.
const anonymousUserID = "anonymous"

// user.id source levels, in resolution order.
const (
	userIDSourceHeader      = "explicit"
	userIDSourceAccount     = "client-account"
	userIDSourceCallerScope = "caller-scope"
	userIDSourceAnonymous   = "anonymous"
)

// explicitUserIDHeaders lists, in priority order, the headers a caller can use
// to pin the reported end-user identity.
var explicitUserIDHeaders = []string{
	"X-Langfuse-User-Id",
	"X-User-Id",
	"X-End-User-Id",
}

// builtinAccountHeaders lists the headers first-party clients already send
// without any client-side change. Codex sends the ChatGPT account and, for
// signed-out installs, an installation identifier.
var builtinAccountHeaders = []string{
	"Chatgpt-Account-Id",
	"X-Codex-Installation-Id",
}

// userIDDigestPrefix and callerScopeDigestPrefix keep the two hashed user.id
// flavours visually distinguishable in the Langfuse Users list.
const (
	userIDDigestPrefix      = "u-"
	callerScopeDigestPrefix = "key-"
)

// identity holds the caller and session dimensions resolved for one inbound
// request. Raw (pre-digest) identifiers only ever live in this struct; what
// reaches a span is decided by resolveUserID.
type identity struct {
	sessionID       string
	parentSessionID string
	clientType      string
	agentName       string
	isSubagent      bool
	isFork          bool

	// explicitUserID is level 1: an end-user identity the caller pinned itself.
	explicitUserID string
	// accountID is level 2: an account identity a first-party client already ships.
	accountID string
	// callerScope is level 3: the irreversible digest of the downstream API key.
	callerScope string
}

// identityHolder carries the per-request identity across the middleware /
// usage-plugin boundary. It has to be mutable: the generation span is produced
// inside c.Next(), the caller credential only exists after the auth middleware
// has run, and the session identity the executor actually routed on is only
// known once the usage record is published.
type identityHolder struct {
	mu     sync.Mutex
	id     identity
	ginCtx *gin.Context
	scoped bool
}

type identityHolderKey struct{}

// newIdentityHolder binds a holder to the gin context so the caller scope can
// be resolved lazily, after the auth middleware has stored the API key.
func newIdentityHolder(c *gin.Context) *identityHolder {
	return &identityHolder{ginCtx: c}
}

func withIdentityHolder(ctx context.Context, holder *identityHolder) context.Context {
	return context.WithValue(ctx, identityHolderKey{}, holder)
}

func identityHolderFrom(ctx context.Context) *identityHolder {
	if ctx == nil {
		return nil
	}
	holder, _ := ctx.Value(identityHolderKey{}).(*identityHolder)
	return holder
}

// seed stores the identity recognized from the inbound request itself.
func (h *identityHolder) seed(id identity) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	scope := h.id.callerScope
	h.id = id
	if h.id.callerScope == "" {
		h.id.callerScope = scope
	}
}

// observeSession records the session identity the request was actually routed
// on, so the HTTP root span reports the same value as the generation span.
func (h *identityHolder) observeSession(sessionID, parentSessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id.sessionID = sessionID
	if parentSessionID != "" {
		h.id.parentSessionID = parentSessionID
	}
}

// snapshot returns the current identity, resolving the caller scope on first
// read because the auth middleware runs after the observability middleware has
// already started the root span.
func (h *identityHolder) snapshot() identity {
	if h == nil {
		return identity{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.scoped {
		if scope := callerScopeFromGin(h.ginCtx); scope != "" {
			h.id.callerScope = scope
		}
		h.scoped = true
	}
	return h.id
}

// extractIdentity derives the session dimensions from an inbound request by
// reusing the existing session recognition subsystem, then layers the user.id
// source candidates on top. It never fails: an unrecognized request simply
// yields an identity whose user.id resolves to the anonymous fallback.
func extractIdentity(headers http.Header, payload []byte) identity {
	var id identity
	if info, ok := coresession.ExtractSessionInfo(headers, payload, nil); ok {
		id.sessionID = coresession.NormalizeToCanonicalUUID(info.SessionID)
		id.parentSessionID = coresession.NormalizeToCanonicalUUID(info.ParentSessionID)
		if id.sessionID != "" && id.sessionID == id.parentSessionID {
			id.parentSessionID = ""
		}
		id.clientType = info.ClientType
		id.agentName = info.AgentName
		id.isSubagent = info.IsSubagent
		id.isFork = info.IsFork
	}
	id.explicitUserID = explicitUserID(headers, payload)
	id.accountID = builtinAccountID(headers, payload)
	return id
}

// explicitUserID reads a caller-pinned end-user identity from the dedicated
// headers, falling back to the OpenAI-compatible top-level "user" body field.
func explicitUserID(headers http.Header, payload []byte) string {
	for _, name := range explicitUserIDHeaders {
		if value := coresession.NormalizeExplicitID(headers.Get(name)); value != "" {
			return value
		}
	}
	if len(payload) > 0 {
		if value := coresession.NormalizeExplicitID(gjson.GetBytes(payload, "user").String()); value != "" {
			return value
		}
	}
	return ""
}

// builtinAccountID extracts the account identity a first-party client already
// sends: Claude Code ships it inside the metadata.user_id blob, Codex sends it
// as a request header.
func builtinAccountID(headers http.Header, payload []byte) string {
	if value := claudeAccountID(payload); value != "" {
		return value
	}
	for _, name := range builtinAccountHeaders {
		if value := coresession.NormalizeExplicitID(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// claudeAccountID reads the account identity out of the Claude Code
// metadata.user_id field, which is either a JSON object carrying account_uuid
// or the legacy "user_<hash>_account_<hash>_session_<uuid>" string whose
// prefix identifies the account.
func claudeAccountID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	raw := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
	if raw == "" {
		raw = strings.TrimSpace(gjson.GetBytes(payload, "request.metadata.user_id").String())
	}
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "{") {
		parsed := gjson.Parse(raw)
		for _, key := range []string{"account_uuid", "account_id", "organization_uuid", "device_id"} {
			if value := coresession.NormalizeExplicitID(parsed.Get(key).String()); value != "" {
				return value
			}
		}
		return ""
	}
	if idx := strings.Index(raw, "_session_"); idx > 0 {
		return coresession.NormalizeExplicitID(raw[:idx])
	}
	return ""
}

// callerScopeFromGin derives the irreversible caller scope from the API key the
// auth middleware stored on the gin context. The raw key is never retained.
func callerScopeFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	raw, ok := value.(string)
	if !ok {
		raw = fmt.Sprint(value)
	}
	return coresession.CallerScope(raw)
}

// resolveUserID applies the four-level user.id fallback chain and reports both
// the emitted value and the source level that produced it:
//
//  1. an end-user identity the caller pinned explicitly;
//  2. the account identity a first-party client already ships;
//  3. the irreversible scope of the downstream API key;
//  4. a fixed anonymous literal.
//
// Levels 1 and 2 are digested unless plaintext output is explicitly enabled.
// Level 3 is already irreversible and is unaffected by that switch; level 4
// carries no caller information and is emitted as-is.
func resolveUserID(id identity, plaintext bool) (value, source string) {
	switch {
	case id.explicitUserID != "":
		return maskUserID(id.explicitUserID, plaintext), userIDSourceHeader
	case id.accountID != "":
		return maskUserID(id.accountID, plaintext), userIDSourceAccount
	case id.callerScope != "":
		return callerScopeDigestPrefix + shortDigest(id.callerScope), userIDSourceCallerScope
	default:
		return anonymousUserID, userIDSourceAnonymous
	}
}

// maskUserID digests a raw caller identifier unless plaintext output is enabled.
func maskUserID(raw string, plaintext bool) string {
	if plaintext {
		return raw
	}
	return userIDDigestPrefix + shortDigest(coresession.CallerScope(raw))
}

// shortDigest keeps the leading 128 bits of a hex digest, which stays
// collision-free in practice while keeping the Langfuse Users list readable.
func shortDigest(digest string) string {
	const keep = 32
	if len(digest) > keep {
		return digest[:keep]
	}
	return digest
}

// identityAttributes renders a resolved identity as span attributes. user.id is
// always present; the remaining dimensions are omitted when unknown so Langfuse
// does not display empty facets.
func identityAttributes(id identity, plaintext bool) []attribute.KeyValue {
	userID, source := resolveUserID(id, plaintext)
	attrs := []attribute.KeyValue{
		attribute.String(attrUserID, userID),
		attribute.String("cpa.user.id_source", source),
	}
	if id.sessionID != "" {
		attrs = append(attrs, attribute.String(attrSessionID, id.sessionID))
	}
	if id.parentSessionID != "" {
		attrs = append(attrs, attribute.String("cpa.session.parent_id", id.parentSessionID))
	}
	if id.clientType != "" {
		attrs = append(attrs, attribute.String("cpa.client.type", id.clientType))
	}
	if id.agentName != "" {
		attrs = append(attrs, attribute.String("cpa.agent.name", id.agentName))
	}
	if id.isSubagent {
		attrs = append(attrs, attribute.Bool("cpa.agent.subagent", true))
	}
	if id.isFork {
		attrs = append(attrs, attribute.Bool("cpa.session.fork", true))
	}
	return attrs
}
