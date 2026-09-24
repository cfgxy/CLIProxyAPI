package observability

import (
	"net/http"
	"regexp"
	"strings"
)

// redactedPlaceholder replaces any credential value before it can reach a span
// attribute, event, or captured payload.
const redactedPlaceholder = "[REDACTED]"

// sensitiveHeaderNames lists HTTP headers that must never be forwarded to a
// span verbatim under any circumstance, per the ADR-001 privacy requirements.
var sensitiveHeaderNames = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"x-goog-api-key":      {},
	"x-auth-token":        {},
}

// sensitiveValuePatterns matches credential-shaped substrings that may appear
// embedded inside otherwise-safe text (captured bodies, error messages), so
// redaction is applied even when the credential did not arrive via a
// known-sensitive header.
var sensitiveValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)basic\s+[a-z0-9+/=]+`),
	regexp.MustCompile(`sk-[a-zA-Z0-9_-]{10,}`),
	regexp.MustCompile(`ya29\.[a-zA-Z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|secret)\s*[:=]\s*["']?[a-z0-9._~+/=-]{8,}`),
}

// isSensitiveHeader reports whether name (any casing) must be stripped before
// it can reach a span.
func isSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// redactHeaders returns a copy of h with every sensitive header value replaced
// by a placeholder. Non-sensitive headers pass through unchanged.
func redactHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for name, values := range h {
		if isSensitiveHeader(name) {
			out[name] = redactedPlaceholder
			continue
		}
		out[name] = strings.Join(values, ",")
	}
	return out
}

// redactText scrubs credential-shaped substrings out of free-form text such as
// captured request/response bodies or error messages. It is applied
// unconditionally, regardless of whether payload capture is enabled.
func redactText(s string) string {
	if s == "" {
		return s
	}
	for _, pattern := range sensitiveValuePatterns {
		s = pattern.ReplaceAllString(s, redactedPlaceholder)
	}
	return s
}

// truncate bounds s to at most maxBytes bytes, appending a marker when
// truncation occurred.
func truncate(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "...[truncated]"
}
