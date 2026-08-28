package logcore

import (
	"fmt"
	"net/http"
	"runtime"

	"github.com/gin-gonic/gin"
)

// traceHeader is the W3C trace header. Binding the incoming one is what makes
// a request that crosses services one incident instead of several.
const traceHeader = "traceparent"

// Middleware reports panics and handler errors to logcore, then hands the
// request back to the next middleware in the chain. It reports; it does not
// change the response, except for the panic case, where it aborts with 500
// exactly as gin.Recovery does.
func (c *Client) Middleware() gin.HandlerFunc {
	if c == nil {
		return func(ctx *gin.Context) { ctx.Next() }
	}
	return func(ctx *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				c.LogError(
					fmt.Sprintf("panic serving %s %s", ctx.Request.Method, ctx.FullPath()),
					panicError(r),
					LogOptions{
						Stack:   panicStack(),
						Trace:   ctx.GetHeader(traceHeader),
						Context: requestContext(ctx),
					},
				)
				ctx.AbortWithStatus(http.StatusInternalServerError)
			}
		}()

		ctx.Next()

		for _, ginErr := range ctx.Errors {
			c.LogError(
				fmt.Sprintf("%s %s failed", ctx.Request.Method, ctx.FullPath()),
				ginErr.Err,
				LogOptions{
					Trace:   ctx.GetHeader(traceHeader),
					Context: requestContext(ctx),
				},
			)
		}

		// A handler that answers 5xx without calling ctx.Error has still
		// failed, and this codebase reports its failures that way. Without
		// this the middleware would only ever see panics.
		//
		// Neither frames nor a fingerprint are sent. The handler has already
		// returned, so capturing here would yield this middleware's own stack
		// rather than the line that failed — and with no frames to mislead
		// it, logcore groups these on its own terms. Overriding that would
		// only restate what the message already carries.
		if len(ctx.Errors) == 0 && ctx.Writer.Status() >= http.StatusInternalServerError {
			c.Log(SeverityError,
				fmt.Sprintf("%s %s responded %d", ctx.Request.Method, ctx.FullPath(), ctx.Writer.Status()),
				LogOptions{
					Trace:   ctx.GetHeader(traceHeader),
					Context: requestContext(ctx),
				},
			)
		}
	}
}

// requestContext carries only routing and status. Never the body, the query
// string, or any header — those hold tokens and personal data.
func requestContext(ctx *gin.Context) map[string]any {
	return map[string]any{
		"method": ctx.Request.Method,
		"route":  ctx.FullPath(),
		"status": ctx.Writer.Status(),
	}
}

func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}

// panicStack walks the frames of the panicking goroutine rather than the
// deferred function's own caller chain, so index 0 is the line that panicked.
func panicStack() []Frame {
	pcs := make([]uintptr, maxStackFrames)
	// Skip runtime.Callers, panicStack and the deferred closure itself.
	n := runtime.Callers(4, pcs)
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
