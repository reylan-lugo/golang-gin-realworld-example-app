package logcore

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newRouterWithCapture(t *testing.T) (*gin.Engine, *Client, chan []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, captured := newCapturing(t, defaultConfig())
	r := gin.New()
	r.Use(c.Middleware())
	return r, c, captured
}

func TestMiddlewareReportsAPanicAndStillAnswers500(t *testing.T) {
	// Arrange
	r, c, captured := newRouterWithCapture(t)
	r.GET("/api/articles", func(ctx *gin.Context) { panic("slug index is nil") })

	// Act
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/articles", nil))
	c.Close()

	// Assert
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	entry := firstEntry(t, <-captured)
	if sev, _ := entry["severity"].(string); sev != SeverityError {
		t.Errorf("severity = %q, want ERROR", sev)
	}
	errObj, ok := entry["error"].(map[string]any)
	if !ok {
		t.Fatal("the panic was reported without an error object")
	}
	if msg, _ := errObj["message"].(string); msg != "slug index is nil" {
		t.Errorf("error.message = %q, want the panic value", msg)
	}
	stack, _ := errObj["stack"].([]any)
	if len(stack) == 0 {
		t.Error("the panic was reported with no frames")
	}
}

func TestMiddlewareReportsA500AnsweredWithoutCtxError(t *testing.T) {
	// Arrange — this codebase's handlers answer failures with a status code
	// rather than ctx.Error, so this is the path that actually reports.
	r, c, captured := newRouterWithCapture(t)
	r.GET("/api/articles", func(ctx *gin.Context) {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": "database is locked"})
	})

	// Act
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/articles", nil))
	c.Close()

	// Assert
	entry := firstEntry(t, <-captured)
	if msg, _ := entry["message"].(string); msg != "GET /api/articles responded 500" {
		t.Errorf("message = %q, want the route and status it answered", msg)
	}
	// Grouping is logcore's to decide: no fingerprint is imposed here.
	if _, present := entry["fingerprint"]; present {
		t.Error("entry overrides grouping with a fingerprint")
	}
	if _, present := entry["error"]; present {
		t.Error("entry carries a synthesized error object")
	}
	ctxMap, ok := entry["context"].(map[string]any)
	if !ok {
		t.Fatal("entry carries no context")
	}
	if route, _ := ctxMap["route"].(string); route != "/api/articles" {
		t.Errorf("context.route = %q, want /api/articles", route)
	}
}

func TestMiddlewareStaysSilentOnASuccessfulRequest(t *testing.T) {
	// Arrange
	r, c, captured := newRouterWithCapture(t)
	r.GET("/api/ping", func(ctx *gin.Context) { ctx.JSON(http.StatusOK, gin.H{"message": "pong"}) })

	// Act
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	c.Close()

	// Assert
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	select {
	case <-captured:
		t.Error("a 200 response was reported as an error")
	default:
	}
}

func TestMiddlewareIsANoOpWhenLoggingIsDisabled(t *testing.T) {
	// Arrange — a nil client must not break the app.
	gin.SetMode(gin.TestMode)
	var c *Client
	r := gin.New()
	r.Use(c.Middleware())
	r.GET("/api/ping", func(ctx *gin.Context) { ctx.JSON(http.StatusOK, gin.H{"message": "pong"}) })

	// Act
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))

	// Assert
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
