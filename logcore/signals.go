package logcore

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// shutdownFlushTimeout bounds how long a terminating process waits for the
// buffer to drain. Orchestrators grant a grace period before SIGKILL, so this
// stays well inside the smallest common one.
const shutdownFlushTimeout = 3 * time.Second

// InstallSignalFlush drains the buffer when the process is asked to terminate,
// and returns the function that undoes it.
//
// Go does not run deferred functions on SIGTERM or SIGINT, so without this the
// flush on exit only ever happens when main returns on its own — never on
// `docker stop`, a systemd restart, or Ctrl+C, which is how a service actually
// goes down. Everything still buffered would go with it.
//
// The signal keeps its original meaning: once the buffer is drained, the
// handler is removed and the signal re-raised, so the process dies exactly as
// it would have.
func InstallSignalFlush(c *Client) func() {
	if c == nil {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	done := make(chan struct{})
	go func() {
		select {
		case received := <-signals:
			c.FlushSync(shutdownFlushTimeout)
			// Hand the signal back to the default disposition rather than
			// deciding this process's fate here.
			signal.Stop(signals)
			if p, err := os.FindProcess(os.Getpid()); err == nil {
				_ = p.Signal(received)
			}
		case <-done:
		}
	}()

	return func() {
		signal.Stop(signals)
		close(done)
	}
}
