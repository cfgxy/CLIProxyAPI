package observability

import (
	"regexp"
)

// redactedPlaceholder replaces any credential value before it can reach a span
// attribute, event, or captured payload.
const redactedPlaceholder = "[REDACTED]"

// sensitiveValuePatterns matches credential-shaped substrings that may appear
// embedded inside otherwise-safe text (captured bodies, error messages), so
// redaction is applied even when the credential did not arrive via a
// known-sensitive header.
//
// The key/value pattern (last entry) allows an optional quote both before and
// after the key name so it also matches JSON-encoded forms such as
// `{"api_key": "..."}` or `{"key": "..."}`, not only bare `key=value` text; the
// value character class is case-insensitive (via the leading (?i)) so it also
// covers Google-style API keys, which are mixed case (see the dedicated AIza
// pattern below for the header/bare-value form of the same key shape).
var sensitiveValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)basic\s+[a-z0-9+/=]+`),
	regexp.MustCompile(`sk-[a-zA-Z0-9_-]{10,}`),
	regexp.MustCompile(`ya29\.[a-zA-Z0-9_-]{10,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|\bkey)["']?\s*[:=]\s*["']?[a-z0-9._~+/=-]{8,}`),
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
