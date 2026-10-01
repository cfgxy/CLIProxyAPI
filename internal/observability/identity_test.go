package observability

import (
	"net/http"
	"strings"
	"testing"

	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const (
	testExplicitUserID = "alice@example.com"
	testAccountUUID    = "11111111-2222-3333-4444-555555555555"
	testCallerScope    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// TestResolveUserID_FallbackChainLevels pins the four-level user.id chain. Each
// case states which sources are present; the "escalated" variant re-adds the
// next higher-priority source, so reordering or collapsing the chain makes the
// test fail instead of silently changing which identity Langfuse groups on.
func TestResolveUserID_FallbackChainLevels(t *testing.T) {
	cases := []struct {
		name       string
		id         identity
		wantSource string
	}{
		{
			name:       "level4 anonymous when nothing is known",
			id:         identity{},
			wantSource: userIDSourceAnonymous,
		},
		{
			name:       "level3 caller scope when no account identity exists",
			id:         identity{callerScope: testCallerScope},
			wantSource: userIDSourceCallerScope,
		},
		{
			name:       "level2 account identity when no explicit header exists",
			id:         identity{accountID: testAccountUUID, callerScope: testCallerScope},
			wantSource: userIDSourceAccount,
		},
		{
			name:       "level1 explicit identity wins over everything",
			id:         identity{explicitUserID: testExplicitUserID, accountID: testAccountUUID, callerScope: testCallerScope},
			wantSource: userIDSourceHeader,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, source := resolveUserID(tc.id, false)
			if source != tc.wantSource {
				t.Fatalf("source = %q, want %q", source, tc.wantSource)
			}
			if value == "" {
				t.Fatal("user.id must never resolve to an empty value")
			}
		})
	}
}

// TestResolveUserID_LevelRequiresHigherLevelsAbsent asserts the "only reached
// when the previous level is missing" property directly: restoring the removed
// higher-priority source must change the resolved source.
func TestResolveUserID_LevelRequiresHigherLevelsAbsent(t *testing.T) {
	steps := []struct {
		name      string
		lower     identity
		lowerSrc  string
		escalated identity
		higherSrc string
	}{
		{
			name:      "caller scope only reached without account identity",
			lower:     identity{callerScope: testCallerScope},
			lowerSrc:  userIDSourceCallerScope,
			escalated: identity{callerScope: testCallerScope, accountID: testAccountUUID},
			higherSrc: userIDSourceAccount,
		},
		{
			name:      "account identity only reached without explicit header",
			lower:     identity{accountID: testAccountUUID},
			lowerSrc:  userIDSourceAccount,
			escalated: identity{accountID: testAccountUUID, explicitUserID: testExplicitUserID},
			higherSrc: userIDSourceHeader,
		},
		{
			name:      "anonymous only reached without caller scope",
			lower:     identity{},
			lowerSrc:  userIDSourceAnonymous,
			escalated: identity{callerScope: testCallerScope},
			higherSrc: userIDSourceCallerScope,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			lowerValue, lowerSource := resolveUserID(step.lower, false)
			if lowerSource != step.lowerSrc {
				t.Fatalf("lower source = %q, want %q", lowerSource, step.lowerSrc)
			}
			higherValue, higherSource := resolveUserID(step.escalated, false)
			if higherSource != step.higherSrc {
				t.Fatalf("escalated source = %q, want %q", higherSource, step.higherSrc)
			}
			if lowerValue == higherValue {
				t.Fatalf("restoring the higher-priority source did not change user.id (%q)", lowerValue)
			}
		})
	}
}

// TestResolveUserID_DefaultsToIrreversibleDigest is the security assertion:
// with the switch off, no raw caller identifier reaches the span; with it on,
// the raw value is emitted verbatim. Removing the switch condition from
// maskUserID makes one of the two halves fail.
func TestResolveUserID_DefaultsToIrreversibleDigest(t *testing.T) {
	for _, id := range []identity{
		{explicitUserID: testExplicitUserID},
		{accountID: testAccountUUID},
	} {
		raw := id.explicitUserID
		if raw == "" {
			raw = id.accountID
		}

		hashed, _ := resolveUserID(id, false)
		if strings.Contains(hashed, raw) {
			t.Fatalf("default output %q leaks the raw identifier %q", hashed, raw)
		}
		if hashed == "" {
			t.Fatal("hashed user.id must not be empty")
		}
		if again, _ := resolveUserID(id, false); again != hashed {
			t.Fatalf("hashing is not deterministic: %q != %q", again, hashed)
		}

		plain, _ := resolveUserID(id, true)
		if plain != raw {
			t.Fatalf("plaintext output = %q, want %q", plain, raw)
		}
		if plain == hashed {
			t.Fatal("plaintext switch produced the same value as the default hash")
		}
	}
}

// TestResolveUserID_CallerScopeNeverPlaintext guards the level-3 value: it is
// already an irreversible digest, and the plaintext switch must not turn it
// back into anything resembling the raw API key.
func TestResolveUserID_CallerScopeNeverPlaintext(t *testing.T) {
	scope := coresession.CallerScope("sk-test-caller-key")
	id := identity{callerScope: scope}

	hashed, _ := resolveUserID(id, false)
	plain, _ := resolveUserID(id, true)
	if hashed != plain {
		t.Fatalf("caller scope changed with the plaintext switch: %q vs %q", hashed, plain)
	}
	if strings.Contains(hashed, "sk-test-caller-key") {
		t.Fatalf("caller scope output %q leaks the API key", hashed)
	}
}

// TestResolveUserID_DistinguishesCallers covers acceptance item 9: two API keys
// must not collapse into one Langfuse user.
func TestResolveUserID_DistinguishesCallers(t *testing.T) {
	first, _ := resolveUserID(identity{callerScope: coresession.CallerScope("key-a")}, false)
	second, _ := resolveUserID(identity{callerScope: coresession.CallerScope("key-b")}, false)
	if first == second {
		t.Fatalf("different API keys collapsed into the same user.id %q", first)
	}
}

func TestExtractIdentity_ClaudeCodeMetadata(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4","metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"` + testAccountUUID + `\",\"session_id\":\"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\"}"}}`)

	id := extractIdentity(http.Header{}, payload)
	if id.sessionID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("sessionID = %q", id.sessionID)
	}
	if id.accountID != testAccountUUID {
		t.Fatalf("accountID = %q, want %q", id.accountID, testAccountUUID)
	}
	if id.clientType != "claude" {
		t.Fatalf("clientType = %q, want claude", id.clientType)
	}
	if _, source := resolveUserID(id, false); source != userIDSourceAccount {
		t.Fatalf("user.id source = %q, want %q", source, userIDSourceAccount)
	}
}

func TestExtractIdentity_ClaudeCodeLegacyMetadata(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"user_abc123_account_def456_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`)

	id := extractIdentity(http.Header{}, payload)
	if id.accountID != "user_abc123_account_def456" {
		t.Fatalf("accountID = %q", id.accountID)
	}
}

func TestExtractIdentity_CodexHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Session-Id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	headers.Set("Chatgpt-Account-Id", "acct-9")

	id := extractIdentity(headers, nil)
	if id.sessionID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("sessionID = %q", id.sessionID)
	}
	if id.accountID != "acct-9" {
		t.Fatalf("accountID = %q", id.accountID)
	}
	if id.clientType != "codex" {
		t.Fatalf("clientType = %q, want codex", id.clientType)
	}
}

func TestExtractIdentity_ExplicitHeaderWins(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Langfuse-User-Id", testExplicitUserID)
	headers.Set("Chatgpt-Account-Id", "acct-9")

	id := extractIdentity(headers, nil)
	if id.explicitUserID != testExplicitUserID {
		t.Fatalf("explicitUserID = %q", id.explicitUserID)
	}
	if _, source := resolveUserID(id, false); source != userIDSourceHeader {
		t.Fatalf("user.id source = %q, want %q", source, userIDSourceHeader)
	}
}

func TestExtractIdentity_OpenAIBodyUserField(t *testing.T) {
	id := extractIdentity(http.Header{}, []byte(`{"model":"gpt-4o","user":"`+testExplicitUserID+`"}`))
	if id.explicitUserID != testExplicitUserID {
		t.Fatalf("explicitUserID = %q", id.explicitUserID)
	}
}

// TestExtractIdentity_UnrecognizedRequestIsFailOpen covers the fail-open
// requirement at the mapping layer: an unrecognizable request yields no
// session dimension but still produces a usable, non-empty user.id.
func TestExtractIdentity_UnrecognizedRequestIsFailOpen(t *testing.T) {
	id := extractIdentity(nil, nil)
	if id.sessionID != "" {
		t.Fatalf("sessionID = %q, want empty", id.sessionID)
	}
	value, source := resolveUserID(id, false)
	if source != userIDSourceAnonymous || value != anonymousUserID {
		t.Fatalf("got (%q,%q), want (%q,%q)", value, source, anonymousUserID, userIDSourceAnonymous)
	}
	if attrs := identityAttributes(id, false); len(attrs) == 0 {
		t.Fatal("identityAttributes must always emit user.id")
	}
}

// TestSessionIDNormalization asserts the two properties acceptance item 2 asks
// for: canonical UUIDs pass through unchanged, and non-UUID identifiers map
// stably to the same canonical UUID on every request.
func TestSessionIDNormalization(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Session-Id", "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	uuidID := extractIdentity(headers, nil)
	if uuidID.sessionID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("canonical UUID not preserved: %q", uuidID.sessionID)
	}
	if again := coresession.NormalizeToCanonicalUUID(uuidID.sessionID); again != uuidID.sessionID {
		t.Fatalf("normalization is not idempotent: %q != %q", again, uuidID.sessionID)
	}

	opaque := http.Header{}
	opaque.Set("X-Session-Id", "deerflow-thread-42")
	first := extractIdentity(opaque, nil)
	second := extractIdentity(opaque, nil)
	if first.sessionID == "" {
		t.Fatal("opaque session identifier was dropped instead of being projected")
	}
	if first.sessionID != second.sessionID {
		t.Fatalf("opaque mapping is unstable: %q != %q", first.sessionID, second.sessionID)
	}
	if first.sessionID == "deerflow-thread-42" {
		t.Fatal("opaque session identifier was not normalized to a canonical UUID")
	}

	other := http.Header{}
	other.Set("X-Session-Id", "deerflow-thread-43")
	if extractIdentity(other, nil).sessionID == first.sessionID {
		t.Fatal("distinct sessions collapsed into one canonical UUID")
	}
}

func TestIdentityAttributes_EmitsLangfuseDimensions(t *testing.T) {
	id := identity{
		sessionID:       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		parentSessionID: "11111111-1111-1111-1111-111111111111",
		clientType:      "claude",
		agentName:       "subagent",
		isSubagent:      true,
		isFork:          true,
		callerScope:     testCallerScope,
	}

	got := map[string]string{}
	for _, kv := range identityAttributes(id, false) {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for key, want := range map[string]string{
		"session.id":            id.sessionID,
		"cpa.session.parent_id": id.parentSessionID,
		"cpa.client.type":       "claude",
		"cpa.agent.name":        "subagent",
		"cpa.agent.subagent":    "true",
		"cpa.session.fork":      "true",
		"cpa.user.id_source":    userIDSourceCallerScope,
	} {
		if got[key] != want {
			t.Fatalf("attribute %s = %q, want %q", key, got[key], want)
		}
	}
	if got["user.id"] == "" {
		t.Fatal("user.id attribute missing")
	}
}
