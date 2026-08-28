// Package logcore ships this service's errors to the logcore gateway.
//
// The service keeps writing its own logs to stdout exactly as before: this
// package replaces one hop — how an error reaches the platform — and nothing
// else. Only ERROR and above travel over the wire.
package logcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Severity levels accepted by the gateway, in ascending order.
const (
	SeverityDebug    = "DEBUG"
	SeverityInfo     = "INFO"
	SeverityWarning  = "WARNING"
	SeverityError    = "ERROR"
	SeverityCritical = "CRITICAL"
)

var severityRank = map[string]int{
	SeverityDebug:    0,
	SeverityInfo:     1,
	SeverityWarning:  2,
	SeverityError:    3,
	SeverityCritical: 4,
}

// Wire clamps enforced by the gateway. Clamping here keeps a single oversized
// entry from having the whole batch rejected.
const (
	maxMessageChars = 65536
	maxLabels       = 32
	maxLabelChars   = 1024
	maxStackFrames  = 50
)

// Frame is one parsed stack frame. The throw site is index 0 — logcore takes
// the first in-app frame as the issue's top location, so the order decides
// what errors group on.
type Frame struct {
	Function string `json:"function"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	InApp    bool   `json:"inApp"`
}

// ErrorInfo is the `error` object of an entry. Stack is a list of frames,
// never a raw traceback string.
type ErrorInfo struct {
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Stack   []Frame `json:"stack,omitempty"`
}

// Entry is one log entry on the http transport. Every field is top-level:
// this path never touches Cloud Logging, so nothing is nested or promoted.
// Identity (repo, service_id, project_id) is resolved server-side from the
// API key, which is why no service_id and no source_project are sent.
type Entry struct {
	Timestamp   string            `json:"timestamp"`
	Severity    string            `json:"severity"`
	Message     string            `json:"message"`
	Service     string            `json:"service"`
	Env         string            `json:"env"`
	InsertID    string            `json:"insert_id"`
	Trace       string            `json:"trace,omitempty"`
	Error       *ErrorInfo        `json:"error,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Context     map[string]any    `json:"context,omitempty"`
	Fingerprint string            `json:"fingerprint,omitempty"`
}

type envelope struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// insertID is derived, never random: the transport retries, so a batch the
// server accepted but whose response was lost arrives twice, and only a
// deterministic id lets logcore recognize the duplicate instead of counting
// the error again.
func insertID(timestamp, service, severity, message string, ctx map[string]any) string {
	h := sha256.New()
	for _, part := range []string{timestamp, service, severity, message, canonicalJSON(ctx)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// canonicalJSON renders a context map with its keys in a stable order, so the
// same context always yields the same insert id. Go's encoding/json already
// sorts map keys, but nested maps inside `any` values only sort when they are
// map[string]any — which is what callers build.
func canonicalJSON(ctx map[string]any) string {
	if len(ctx) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(ctx[k])
		if err != nil {
			vb = []byte(`null`)
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

func clampMessage(msg string) string {
	if len(msg) <= maxMessageChars {
		return msg
	}
	return msg[:maxMessageChars]
}

func clampLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxLabels {
		keys = keys[:maxLabels]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v := labels[k]
		if len(v) > maxLabelChars {
			v = v[:maxLabelChars]
		}
		out[k] = v
	}
	return out
}

// captureStack walks the caller chain, innermost first, which is the order
// runtime.CallersFrames already yields.
func captureStack(skip int) []Frame {
	pcs := make([]uintptr, maxStackFrames)
	n := runtime.Callers(skip+2, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]Frame, 0, n)
	for {
		f, more := frames.Next()
		out = append(out, Frame{
			Function: f.Function,
			File:     f.File,
			Line:     f.Line,
			InApp:    isInApp(f.File, f.Function),
		})
		if !more || len(out) >= maxStackFrames {
			break
		}
	}
	return out
}

// modulePath is this repository's Go module, used to tell the project's own
// code apart from its dependencies.
const modulePath = "github.com/gothinkster/golang-gin-realworld-example-app"

func isInApp(file, function string) bool {
	if strings.Contains(file, "/pkg/mod/") || strings.Contains(file, "/go/src/runtime/") {
		return false
	}
	if strings.HasPrefix(function, modulePath) {
		return true
	}
	return !strings.Contains(function, ".") || strings.HasPrefix(function, "main.")
}
