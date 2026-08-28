package logcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2026, 8, 28, 4, 30, 0, 0, time.UTC)
}

// newCapturing builds a client whose send boundary is stubbed, so the wire
// format can be asserted with no network and no credentials.
func newCapturing(t *testing.T, cfg Config) (*Client, chan []byte) {
	t.Helper()
	captured := make(chan []byte, 8)
	c := New(cfg, Options{
		Send: func(b []byte) error { captured <- b; return nil },
		Now:  fixedNow,
	})
	if c == nil {
		t.Fatal("expected a client")
	}
	return c, captured
}

func defaultConfig() Config {
	return Config{
		Enabled:        true,
		Service:        "golang-gin-realworld-example-app",
		Env:            "dev",
		MinSeverity:    SeverityError,
		StdLogSeverity: SeverityError,
	}
}

func firstEntry(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		SchemaVersion int              `json:"schema_version"`
		Entries       []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	if len(env.Entries) == 0 {
		t.Fatal("envelope carries no entries")
	}
	return env.Entries[0]
}

func TestEmittedEntryMatchesTheHTTPWireShape(t *testing.T) {
	// Arrange
	c, captured := newCapturing(t, defaultConfig())

	// Act
	c.LogError("failed to persist article", errors.New("database is locked"), LogOptions{
		Context: map[string]any{"route": "/api/articles"},
	})
	c.Close()

	// Assert
	entry := firstEntry(t, <-captured)
	for field, want := range map[string]string{
		"timestamp": "2026-08-28T04:30:00.000Z",
		"severity":  SeverityError,
		"message":   "failed to persist article",
		"service":   "golang-gin-realworld-example-app",
		"env":       "dev",
	} {
		if got, _ := entry[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	// Identity is resolved from the API key; sending it would be overridden.
	for _, forbidden := range []string{"service_id", "source_project"} {
		if _, present := entry[forbidden]; present {
			t.Errorf("entry carries %q, which this transport must not send", forbidden)
		}
	}
}

func TestInsertIDIsDeterministicAndCorrectlyShaped(t *testing.T) {
	// Arrange
	emit := func() string {
		c, captured := newCapturing(t, defaultConfig())
		c.LogError("boom", errors.New("boom"), LogOptions{
			Context: map[string]any{"b": 2, "a": 1},
		})
		c.Close()
		id, _ := firstEntry(t, <-captured)["insert_id"].(string)
		return id
	}

	// Act
	first, second := emit(), emit()

	// Assert
	if first != second {
		t.Errorf("insert_id is not deterministic: %q vs %q", first, second)
	}
	if len(first) != 32 {
		t.Errorf("insert_id has %d chars, want 32", len(first))
	}
	for _, r := range first {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("insert_id %q is not lowercase hex", first)
		}
	}
}

func TestStackIsParsedFramesInnermostFirst(t *testing.T) {
	// Arrange
	c, captured := newCapturing(t, defaultConfig())

	// Act
	c.LogError("boom", errors.New("boom"), LogOptions{})
	c.Close()

	// Assert
	errObj, ok := firstEntry(t, <-captured)["error"].(map[string]any)
	if !ok {
		t.Fatal("entry carries no error object")
	}
	stack, ok := errObj["stack"].([]any)
	if !ok || len(stack) == 0 {
		t.Fatal("error.stack must be a non-empty list of frames")
	}
	top, _ := stack[0].(map[string]any)
	if _, present := top["inApp"]; !present {
		t.Error("frames must use the camelCase key inApp")
	}
	fn, _ := top["function"].(string)
	if fn == "" {
		t.Error("the innermost frame has no function name")
	}
	if inApp, _ := top["inApp"].(bool); !inApp {
		t.Errorf("innermost frame %q should be in-app", fn)
	}
}

func TestErrorTypeNamesTheRootOfTheWrapChain(t *testing.T) {
	// Arrange
	root := &net.AddrError{Err: "unreachable", Addr: "db:5432"}
	wrapped := fmt.Errorf("saving article: %w", root)

	// Act
	info := buildErrorInfo(wrapped, nil)

	// Assert
	if info.Type != "net.AddrError" {
		t.Errorf("type = %q, want the root type net.AddrError", info.Type)
	}
	if info.Message != wrapped.Error() {
		t.Errorf("message = %q, want the reported message %q", info.Message, wrapped.Error())
	}
}

func TestEntriesBelowTheSeverityFloorNeverLeave(t *testing.T) {
	// Arrange
	c, captured := newCapturing(t, defaultConfig())

	// Act
	c.Log(SeverityInfo, "a routine request", LogOptions{})
	c.Close()

	// Assert
	select {
	case <-captured:
		t.Error("an INFO entry was shipped despite the ERROR floor")
	default:
	}
}

func TestPermanentRejectionDropsTheBatchWithoutRetrying(t *testing.T) {
	// Arrange
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, `{"detail":"env must be one of prod, staging, dev"}`, http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.Endpoint = server.URL
	cfg.APIKey = "test-key"
	c := New(cfg, Options{Now: fixedNow})

	// Act
	c.LogError("boom", errors.New("boom"), LogOptions{})
	c.Close()

	// Assert
	if attempts != 1 {
		t.Errorf("a 422 was attempted %d times, want exactly 1", attempts)
	}
}

func TestLoggingIsANoOpWithoutConfiguration(t *testing.T) {
	// Arrange / Act
	c := New(Config{Enabled: false, Service: "svc"}, Options{})

	// Assert — a nil client must stay safe to call.
	c.LogError("boom", errors.New("boom"), LogOptions{})
	c.Close()
	if c != nil {
		t.Error("a disabled configuration must not produce a client")
	}
}
