package logcore

import (
	"bytes"
	"log"
	"testing"
)

func TestStdLogIsReportedWithoutChangingWhatTheAppWrites(t *testing.T) {
	// Arrange
	c, captured := newCapturing(t, defaultConfig())
	var appOutput bytes.Buffer
	log.SetOutput(&appOutput)
	restore := InstallStdLog(c)

	// Act
	log.Println("failed to get sql.DB: database is locked")
	restore()
	c.Close()

	// Assert — the line still reaches the app's own destination, unchanged.
	if got := appOutput.String(); !bytes.Contains([]byte(got), []byte("failed to get sql.DB")) {
		t.Errorf("the app's own log lost the line: %q", got)
	}
	entry := firstEntry(t, <-captured)
	if msg, _ := entry["message"].(string); msg != "failed to get sql.DB: database is locked" {
		t.Errorf("message = %q, want the line with its timestamp prefix stripped", msg)
	}
	labels, _ := entry["labels"].(map[string]any)
	if source, _ := labels["source"].(string); source != "stdlog" {
		t.Errorf("labels.source = %q, want stdlog", source)
	}
}

func TestRoutineStdLogLinesStayOnTheHost(t *testing.T) {
	// Arrange — the floor, not the text, decides what leaves the host.
	cfg := defaultConfig()
	cfg.StdLogSeverity = SeverityInfo
	c, captured := newCapturing(t, cfg)
	log.SetOutput(&bytes.Buffer{})
	restore := InstallStdLog(c)

	// Act
	log.Println("listening on :8080")
	restore()
	c.Close()

	// Assert
	select {
	case <-captured:
		t.Error("a routine line was shipped despite the ERROR floor")
	default:
	}
}

func TestTheTimestampPrefixIsStrippedSoOccurrencesGroup(t *testing.T) {
	// Arrange
	line := "2026/08/28 04:30:00 failed to persist article"

	// Act
	got := stripLogPrefix(line)

	// Assert
	if got != "failed to persist article" {
		t.Errorf("stripLogPrefix = %q, want the message without date and time", got)
	}
}

func TestInstallIsANoOpWhenLoggingIsDisabled(t *testing.T) {
	// Arrange
	var c *Client
	before := log.Writer()

	// Act
	restore := InstallStdLog(c)
	after := log.Writer()
	restore()

	// Assert — a disabled client must not take over the standard logger.
	if before != after {
		t.Error("the standard logger was rerouted despite logging being disabled")
	}
}

func TestLoweringTheFloorShipsRoutineLinesToo(t *testing.T) {
	// Arrange — the floor is the configured one, not a second hardcoded one.
	cfg := defaultConfig()
	cfg.MinSeverity = SeverityInfo
	cfg.StdLogSeverity = SeverityInfo
	c, captured := newCapturing(t, cfg)
	log.SetOutput(&bytes.Buffer{})
	restore := InstallStdLog(c)

	// Act
	log.Println("listening on :8080")
	restore()
	c.Close()

	// Assert
	select {
	case body := <-captured:
		entry := firstEntry(t, body)
		if sev, _ := entry["severity"].(string); sev != SeverityInfo {
			t.Errorf("severity = %q, want INFO", sev)
		}
	default:
		t.Error("an INFO line was dropped even though the floor is INFO")
	}
}

func TestAFatalLineIsFlushedBeforeTheProcessCouldExit(t *testing.T) {
	// Arrange — log.Fatal exits right after the write returns, so the entry
	// must already be gone by the time Write does.
	c, captured := newCapturing(t, defaultConfig())
	log.SetOutput(&bytes.Buffer{})
	restore := InstallStdLog(c)

	// Act
	log.Print("failed to start server: address in use")
	restore()

	// Assert — no Close() call: if delivery waited for it, this fails.
	select {
	case body := <-captured:
		entry := firstEntry(t, body)
		if sev, _ := entry["severity"].(string); sev != SeverityError {
			t.Errorf("severity = %q, want ERROR", sev)
		}
	default:
		t.Error("a log line was still buffered when the write returned")
	}
	c.Close()
}
