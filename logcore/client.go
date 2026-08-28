package logcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport rules from the gateway contract. A logger that blocks the request
// path or retries forever is worse than no logging.
const (
	flushInterval     = 5 * time.Second
	flushSize         = 20
	maxEntriesPerReq  = 200
	bufferCap         = 1000
	maxRetries        = 3
	retryBaseDelay    = 250 * time.Millisecond
	breakerThreshold  = 10
	ingestPath        = "/v1/logs"
	defaultHTTPTimout = 10 * time.Second
)

// Config is read from the environment by ConfigFromEnv.
type Config struct {
	Enabled     bool
	Endpoint    string
	APIKey      string
	Service     string
	Env         string
	MinSeverity string
	// StdLogSeverity is the severity given to standard-library log lines,
	// which carry none of their own. Declared rather than guessed from the
	// text: this codebase logs only failures that way.
	StdLogSeverity string
}

// ConfigFromEnv reads the LOGCORE_* variables. The key is a server secret: it
// belongs in the deployment's environment or its secret manager, never in
// committed code.
func ConfigFromEnv() Config {
	enabled := true
	if v := os.Getenv("LOGCORE_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			enabled = parsed
		}
	}
	minSeverity := strings.ToUpper(os.Getenv("LOGCORE_MIN_SEVERITY"))
	if _, ok := severityRank[minSeverity]; !ok {
		minSeverity = SeverityError
	}
	env := os.Getenv("LOGCORE_ENV")
	if env == "" {
		env = "dev"
	}
	stdLogSeverity := strings.ToUpper(os.Getenv("LOGCORE_STDLOG_SEVERITY"))
	if _, ok := severityRank[stdLogSeverity]; !ok {
		stdLogSeverity = SeverityError
	}
	return Config{
		Enabled:        enabled,
		Endpoint:       strings.TrimRight(os.Getenv("LOGCORE_URL"), "/"),
		APIKey:         os.Getenv("LOGCORE_KEY"),
		Service:        os.Getenv("LOGCORE_SERVICE"),
		Env:            env,
		MinSeverity:    minSeverity,
		StdLogSeverity: stdLogSeverity,
	}
}

// Options lets tests substitute the send boundary and the clock without a
// network or credentials.
type Options struct {
	HTTPClient *http.Client
	// Send replaces the HTTP call entirely. When set, Endpoint and APIKey are
	// unused. Returning an error marks the flush as failed.
	Send func(body []byte) error
	Now  func() time.Time
}

// Client buffers entries and ships them to the gateway off the calling
// goroutine. The zero value is not usable; build one with New.
type Client struct {
	cfg     Config
	opts    Options
	minRank int

	mu     sync.Mutex
	buf    []Entry
	closed bool

	wake   chan struct{}
	done   chan struct{}
	stop   chan struct{}
	closer sync.Once

	failures int
	tripped  bool
}

// New builds a client. It returns nil when logging is disabled or the
// configuration is incomplete, and a nil *Client is safe to call: every method
// is a no-op on it, so callers never need a guard.
func New(cfg Config, opts Options) *Client {
	if !cfg.Enabled || cfg.Service == "" {
		return nil
	}
	if opts.Send == nil && (cfg.Endpoint == "" || cfg.APIKey == "") {
		return nil
	}
	if _, ok := severityRank[cfg.StdLogSeverity]; !ok {
		cfg.StdLogSeverity = SeverityError
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimout}
	}
	c := &Client{
		cfg:     cfg,
		opts:    opts,
		minRank: severityRank[cfg.MinSeverity],
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	go c.run()
	return c
}

// IngestURL is the transport's own endpoint. HTTP instrumentation must skip
// it, or a failed POST logs an error that POSTs again — the loop that takes a
// process down.
func (c *Client) IngestURL() string {
	if c == nil {
		return ""
	}
	return c.cfg.Endpoint + ingestPath
}

// Reports says whether an entry of this severity would be shipped, given the
// configured floor. Callers use it to skip work — capturing a stack, building
// a context map — for an entry that would be discarded anyway.
func (c *Client) Reports(severity string) bool {
	if c == nil {
		return false
	}
	rank, ok := severityRank[severity]
	return ok && rank >= c.minRank
}

// LogOptions carries everything an entry can hold beyond severity and message.
// Err is optional: the bugs that hurt most raise nothing, and the app itself
// is the only thing positioned to report them.
type LogOptions struct {
	Err error
	// Stack overrides the frames captured for Err. It is only ever sent as
	// part of the `error` object, so it is ignored when Err is nil.
	Stack       []Frame
	Trace       string
	Labels      map[string]string
	Context     map[string]any
	Fingerprint string
}

// Log buffers one entry. It never blocks and never returns an error: a
// delivery problem must not surface in the caller's flow.
func (c *Client) Log(severity, message string, opt LogOptions) {
	if c == nil {
		return
	}
	rank, ok := severityRank[severity]
	if !ok || rank < c.minRank {
		return
	}

	ts := formatTimestamp(c.opts.Now())
	message = clampMessage(message)
	entry := Entry{
		Timestamp:   ts,
		Severity:    severity,
		Message:     message,
		Service:     c.cfg.Service,
		Env:         c.cfg.Env,
		InsertID:    insertID(ts, c.cfg.Service, severity, message, opt.Context),
		Trace:       opt.Trace,
		Labels:      clampLabels(opt.Labels),
		Context:     opt.Context,
		Fingerprint: opt.Fingerprint,
	}
	if opt.Err != nil {
		entry.Error = buildErrorInfo(opt.Err, opt.Stack)
	}

	c.enqueue(entry)
}

// LogError is the common case: an ERROR entry carrying an error value.
func (c *Client) LogError(message string, err error, opt LogOptions) {
	opt.Err = err
	if opt.Stack == nil {
		opt.Stack = captureStack(1)
	}
	c.Log(SeverityError, message, opt)
}

func (c *Client) enqueue(e Entry) {
	c.mu.Lock()
	if c.closed || c.tripped {
		c.mu.Unlock()
		return
	}
	if len(c.buf) >= bufferCap {
		// Drop the oldest: a burst of new errors describes the incident
		// better than the first entries of it.
		c.buf = c.buf[1:]
	}
	c.buf = append(c.buf, e)
	full := len(c.buf) >= flushSize
	c.mu.Unlock()

	if full {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *Client) run() {
	defer close(c.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			c.flush()
			return
		case <-ticker.C:
			c.flush()
		case <-c.wake:
			c.flush()
		}
	}
}

func (c *Client) take() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 || c.tripped {
		return nil
	}
	n := len(c.buf)
	if n > maxEntriesPerReq {
		n = maxEntriesPerReq
	}
	batch := c.buf[:n]
	c.buf = append([]Entry(nil), c.buf[n:]...)
	return batch
}

func (c *Client) flush() {
	for {
		batch := c.take()
		if len(batch) == 0 {
			return
		}
		body, err := json.Marshal(envelope{SchemaVersion: 1, Entries: batch})
		if err != nil {
			reportf("logcore: dropping batch, cannot encode: %v", err)
			continue
		}
		if c.deliver(body) {
			c.recordSuccess()
		} else {
			c.recordFailure()
			return
		}
	}
}

// deliver retries transient failures only. Any other 4xx is permanent: the
// batch is dropped and the server's reason echoed to stderr, because a schema
// mismatch otherwise looks exactly like a healthy silent transport.
func (c *Client) deliver(body []byte) bool {
	for attempt := 0; ; attempt++ {
		retryable, err := c.send(body)
		if err == nil {
			return true
		}
		if !retryable || attempt >= maxRetries {
			reportf("logcore: %v", err)
			return false
		}
		time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
	}
}

// send returns (retryable, error). A nil error means the batch was accepted.
func (c *Client) send(body []byte) (bool, error) {
	if c.opts.Send != nil {
		if err := c.opts.Send(body); err != nil {
			return true, err
		}
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IngestURL(), bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("dropping batch, bad request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("delivery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return true, fmt.Errorf("delivery failed with %s", resp.Status)
	}
	var reason bytes.Buffer
	_, _ = reason.ReadFrom(resp.Body)
	return false, fmt.Errorf("dropping batch, server rejected it with %s: %s",
		resp.Status, strings.TrimSpace(reason.String()))
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	c.failures = 0
	c.mu.Unlock()
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	c.failures++
	if c.failures >= breakerThreshold && !c.tripped {
		c.tripped = true
		c.buf = nil
		c.mu.Unlock()
		reportf("logcore: %d consecutive failed flushes, giving up on delivery", breakerThreshold)
		return
	}
	c.mu.Unlock()
}

// FlushSync drains the buffer on the calling goroutine, giving up after
// timeout. It is the one place the transport is allowed to block: it exists
// for the moment the process is about to die — log.Fatal, an unrecovered
// panic — where the alternative is losing the entry that explains why.
func (c *Client) FlushSync(timeout time.Duration) {
	if c == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.flush()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Close flushes what is buffered and stops the worker. It is the uninstall
// side of New and is safe to call more than once.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closer.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.stop)
		<-c.done
	})
}

// reportf writes the transport's own failures to stderr. They must never go
// through the logging tree this transport ships: that feeds itself.
func reportf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
