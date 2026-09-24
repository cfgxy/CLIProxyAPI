package observability

import (
	"net/http"
	"strings"
	"testing"
)

func TestRedactText_StripsBearerToken(t *testing.T) {
	in := "calling upstream with Authorization: Bearer sk-live-abcdef1234567890 please forward"
	out := redactText(in)
	if strings.Contains(out, "abcdef1234567890") {
		t.Fatalf("expected bearer token to be redacted, got %q", out)
	}
}

func TestRedactText_StripsBasicAuthValue(t *testing.T) {
	in := "Authorization: Basic cHVibGljOnNlY3JldA=="
	out := redactText(in)
	if strings.Contains(out, "cHVibGljOnNlY3JldA==") {
		t.Fatalf("expected basic auth value to be redacted, got %q", out)
	}
}

func TestRedactText_StripsOAuthAccessToken(t *testing.T) {
	in := "token ya29.a0AfH6SMC1234567890abcdefghijklmno leaked in body"
	out := redactText(in)
	if strings.Contains(out, "ya29.a0AfH6SMC1234567890abcdefghijklmno") {
		t.Fatalf("expected google oauth token to be redacted, got %q", out)
	}
}

func TestRedactText_StripsApiKeyAssignment(t *testing.T) {
	in := `{"api_key":"sk-proj-abcdefghij1234567890"}`
	out := redactText(in)
	if strings.Contains(out, "abcdefghij1234567890") {
		t.Fatalf("expected api_key value to be redacted, got %q", out)
	}
}

func TestRedactHeaders_MasksSensitiveHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer super-secret-token")
	h.Set("Cookie", "session=abc123")
	h.Set("X-Request-Id", "req-1")

	out := redactHeaders(h)
	if out["Authorization"] != redactedPlaceholder {
		t.Fatalf("expected Authorization header to be redacted, got %q", out["Authorization"])
	}
	if out["Cookie"] != redactedPlaceholder {
		t.Fatalf("expected Cookie header to be redacted, got %q", out["Cookie"])
	}
	if out["X-Request-Id"] != "req-1" {
		t.Fatalf("expected non-sensitive header to pass through, got %q", out["X-Request-Id"])
	}
}

func TestTruncate_BoundsLength(t *testing.T) {
	in := strings.Repeat("a", 100)
	out := truncate(in, 10)
	if len(out) <= 10 {
		t.Fatalf("expected truncated marker to be appended")
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 10)) {
		t.Fatalf("expected first 10 bytes to be preserved, got %q", out)
	}
}
