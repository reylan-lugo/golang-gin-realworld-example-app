package logcore

import (
	"errors"
	"fmt"
	"reflect"
)

// buildErrorInfo describes an error for the wire. `type` and `message` come
// from the error the service actually reported — that is what its own stdout
// says — while the frames come from where it was captured.
func buildErrorInfo(err error, stack []Frame) *ErrorInfo {
	if err == nil {
		return nil
	}
	if len(stack) > maxStackFrames {
		stack = stack[:maxStackFrames]
	}
	return &ErrorInfo{
		Type:    errorType(err),
		Message: clampMessage(errorMessage(err)),
		Stack:   stack,
	}
}

// errorType names the concrete type of the ROOT of the wrap chain. A wrapping
// layer relabels an error without being the thing that broke, and logcore
// groups on what it is told: stopping at the wrapper collapses every error
// that layer re-raises into a single issue.
func errorType(err error) string {
	root := err
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	t := reflect.TypeOf(root)
	if t == nil {
		return "error"
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Name() == "" {
		return "error"
	}
	if pkg := t.PkgPath(); pkg != "" {
		return fmt.Sprintf("%s.%s", shortPkg(pkg), t.Name())
	}
	return t.Name()
}

func shortPkg(pkg string) string {
	for i := len(pkg) - 1; i >= 0; i-- {
		if pkg[i] == '/' {
			return pkg[i+1:]
		}
	}
	return pkg
}

func errorMessage(err error) string {
	msg := err.Error()
	if msg == "" {
		return "error"
	}
	return msg
}
