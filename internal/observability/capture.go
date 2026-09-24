package observability

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// captureRequestBody reads up to maxBytes of the request body for optional
// span capture, then restores the body so downstream handlers can still read
// it in full. Redaction is applied unconditionally to the captured text.
func captureRequestBody(c *gin.Context, maxBytes int) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return ""
	}
	if closeErr := c.Request.Body.Close(); closeErr != nil {
		// Best-effort close; the body is fully buffered in memory regardless.
		_ = closeErr
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
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
