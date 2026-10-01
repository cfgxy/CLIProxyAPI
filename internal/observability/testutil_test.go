package observability

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// newGinTestRouter builds a gin engine that mirrors the real server's
// middleware ordering (internal/api/server.go: GinLogrusLogger ->
// CPATraceIDMiddleware -> observability.Middleware), so request-id bridging
// behaves the same way it does in production.
func newGinTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(internallogging.GinLogrusLogger())
	r.Use(internallogging.CPATraceIDMiddleware())
	r.Use(Middleware())
	r.GET("/v1/models", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func performRequest(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
