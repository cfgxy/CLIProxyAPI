package observability

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// readAndRestoreBody buffers the request body in memory and restores it so
// downstream handlers can still read it in full. It returns nil when there is
// nothing to read. Both payload capture and session-identity extraction go
// through this helper so a request body is never read twice.
func readAndRestoreBody(c *gin.Context) []byte {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil
	}
	if closeErr := c.Request.Body.Close(); closeErr != nil {
		// Best-effort close; the body is fully buffered in memory regardless.
		_ = closeErr
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}

// bodyCarriesIdentity reports whether a request is shaped like a JSON model
// call, which is the only case where reading the body can yield a session or
// account identity. Management, static, and streaming-download routes are
// skipped so tracing never buffers a body it cannot use.
func bodyCarriesIdentity(request *http.Request) bool {
	if request == nil || request.Body == nil {
		return false
	}
	switch request.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	return strings.Contains(strings.ToLower(request.Header.Get("Content-Type")), "json")
}

// captureRequestBody renders an already-buffered request body for optional
// span capture. Redaction is applied unconditionally to the captured text.
func captureRequestBody(raw []byte, maxBytes int) string {
	if len(raw) == 0 {
		return ""
	}
	return redactText(truncate(string(raw), maxBytes))
}

// captureResponseWriter wraps gin.ResponseWriter to mirror up to maxBytes of
// written response bytes into an in-memory buffer for optional span capture,
// without altering what is written to the real client connection.
type captureResponseWriter struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	maxBytes int
}

func newCaptureResponseWriter(w gin.ResponseWriter, maxBytes int) *captureResponseWriter {
	return &captureResponseWriter{ResponseWriter: w, maxBytes: maxBytes}
}

func (w *captureResponseWriter) Write(b []byte) (int, error) {
	if w.buf.Len() < w.maxBytes {
		remaining := w.maxBytes - w.buf.Len()
		if remaining > len(b) {
			remaining = len(b)
		}
		w.buf.Write(b[:remaining])
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *captureResponseWriter) captured() string {
	return redactText(truncate(w.buf.String(), w.maxBytes))
}

var _ http.ResponseWriter = (*captureResponseWriter)(nil)
