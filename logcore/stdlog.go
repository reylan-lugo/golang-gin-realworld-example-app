package logcore

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// fatalFlushTimeout bounds the one blocking path in the transport: a process
// on its way out is worth waiting on, but not indefinitely.
const fatalFlushTimeout = 2 * time.Second

// stdLogWriter tees the standard library's log output: every line still goes
// to the destination it always went to, and a copy is reported to logcore.
// This is what makes the integration plug-and-play — `log.Printf` call sites
// all over the codebase are covered without one of them being rewritten.
type stdLogWriter struct {
	client   *Client
	next     io.Writer
	severity string

	mu sync.Mutex
}

// InstallStdLog routes the standard logger through logcore and returns the
// function that undoes it. The app keeps writing exactly what it wrote before;
// only the reporting hop is added.
//
// It does NOT capture GORM's SQL log or gin's access log: both hold their own
// writers, and neither is an error channel worth shipping.
func InstallStdLog(c *Client) func() {
	if c == nil {
		return func() {}
	}
	previous := log.Writer()
	log.SetOutput(&stdLogWriter{client: c, next: previous, severity: c.cfg.StdLogSeverity})
	return func() { log.SetOutput(previous) }
}

func (w *stdLogWriter) Write(p []byte) (int, error) {
	// The original destination comes first and its result is what the caller
	// sees: reporting must never change what the app logs, nor fail its write.
	w.mu.Lock()
	n, err := w.next.Write(p)
	w.mu.Unlock()

	line := strings.TrimRight(string(p), "\n")
	if line != "" && w.client.Reports(w.severity) {
		// No `error` object, and so no frames either: a log line is text, and
		// parsing an exception type out of it would invent one that never
		// existed. Frames only reach the wire inside that object, so
		// capturing a stack here would cost every log line a walk up the
		// call chain and then be dropped.
		w.client.Log(w.severity, stripLogPrefix(line), LogOptions{
			Labels: map[string]string{"source": "stdlog"},
		})
		// The next statement may be os.Exit — log.Fatal calls it as soon as
		// this write returns — and then no deferred Close and no ticker would
		// ever run. These lines are rare, so paying a bounded wait on each is
		// cheaper than losing the one that explains why the process died.
		w.client.FlushSync(fatalFlushTimeout)
	}
	return n, err
}

// stripLogPrefix removes the date and time the standard logger prepends, so
// two occurrences of the same failure carry the same message and group
// together instead of becoming one issue per second.
func stripLogPrefix(line string) string {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 3 {
		return line
	}
	if looksLikeDate(fields[0]) && looksLikeTime(fields[1]) {
		return fields[2]
	}
	return line
}

func looksLikeDate(s string) bool {
	return len(s) == 10 && s[4] == '/' && s[7] == '/'
}

func looksLikeTime(s string) bool {
	return len(s) >= 8 && s[2] == ':' && s[5] == ':'
}

// StderrWriter tees writes to stderr the same way, for libraries that are
// pointed at a writer rather than at the standard logger — gin's
// DefaultErrorWriter, for one.
func (c *Client) StderrWriter() io.Writer {
	if c == nil {
		return os.Stderr
	}
	return &stdLogWriter{client: c, next: os.Stderr, severity: c.cfg.StdLogSeverity}
}
