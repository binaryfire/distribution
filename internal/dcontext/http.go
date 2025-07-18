package dcontext

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/distribution/distribution/v3/internal/requestutil"
	"github.com/distribution/distribution/v3/internal/uuid"
	"github.com/gorilla/mux"
)

// Common errors used with this package.
var (
	ErrNoRequestContext        = errors.New("no http request in context")
	ErrNoResponseWriterContext = errors.New("no http response in context")
)

// WithRequest places the request on the context. The context of the request
// is assigned a unique id, available at "http.request.id". The request itself
// is available at "http.request". Other common attributes are available under
// the prefix "http.request.". If a request is already present on the context,
// this method will panic.
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	if ctx.Value("http.request") != nil {
		// NOTE(stevvooe): This needs to be considered a programming error. It
		// is unlikely that we'd want to have more than one request in
		// context.
		panic("only one request per context")
	}

	return &httpRequestContext{
		Context:   ctx,
		startedAt: time.Now(),
		id:        uuid.NewString(),
		r:         r,
	}
}

// GetRequestID attempts to resolve the current request id, if possible. An
// error is return if it is not available on the context.
func GetRequestID(ctx context.Context) string {
	return GetStringValue(ctx, "http.request.id")
}

// WithResponseWriter returns a new context and response writer that makes
// interesting response statistics available within the context.
func WithResponseWriter(ctx context.Context, w http.ResponseWriter) (context.Context, http.ResponseWriter) {
	irw := instrumentedResponseWriter{
		ResponseWriter: w,
		Context:        ctx,
	}
	return &irw, &irw
}

// GetResponseWriter returns the http.ResponseWriter from the provided
// context. If not present, ErrNoResponseWriterContext is returned. The
// returned instance provides instrumentation in the context.
func GetResponseWriter(ctx context.Context) (http.ResponseWriter, error) {
	v := ctx.Value("http.response")

	rw, ok := v.(http.ResponseWriter)
	if !ok || rw == nil {
		return nil, ErrNoResponseWriterContext
	}

	return rw, nil
}

// getVarsFromRequest let's us change request vars implementation for testing
// and maybe future changes.
var getVarsFromRequest = mux.Vars

// WithVars extracts gorilla/mux vars and makes them available on the returned
// context. Variables are available at keys with the prefix "vars.". For
// example, if looking for the variable "name", it can be accessed as
// "vars.name". Implementations that are accessing values need not know that
// the underlying context is implemented with gorilla/mux vars.
func WithVars(ctx context.Context, r *http.Request) context.Context {
	return &muxVarsContext{
		Context: ctx,
		vars:    getVarsFromRequest(r),
	}
}

// GetRequestLogger returns a logger that contains fields from the request in
// the current context. If the request is not available in the context, no
// fields will display. Request loggers can safely be pushed onto the context.
func GetRequestLogger(ctx context.Context) Logger {
	return GetLogger(ctx,
		"http.request.id",
		"http.request.method",
		"http.request.host",
		"http.request.uri",
		"http.request.referer",
		"http.request.useragent",
		"http.request.remoteaddr",
		"http.request.contenttype")
}

// GetResponseLogger reads the current response stats and builds a logger.
// Because the values are read at call time, pushing a logger returned from
// this function on the context will lead to missing or invalid data. Only
// call this at the end of a request, after the response has been written.
func GetResponseLogger(ctx context.Context) Logger {
	l := getLogrusLogger(ctx,
		"http.response.written",
		"http.response.status",
		"http.response.contenttype")

	duration := Since(ctx, "http.request.startedat")

	if duration > 0 {
		l = l.WithField("http.response.duration", duration.String())
	}

	return l
}

// httpRequestContext makes information about a request available to context.
type httpRequestContext struct {
	context.Context

	startedAt time.Time
	id        string
	r         *http.Request
}

// Value returns a keyed element of the request for use in the context. To get
// the request itself, query "request". For other components, access them as
// "request.<component>". For example, r.RequestURI
func (ctx *httpRequestContext) Value(key interface{}) interface{} {
	if keyStr, ok := key.(string); ok {
		switch keyStr {
		case "http.request":
			return ctx.r
		case "http.request.uri":
			return ctx.r.RequestURI
		case "http.request.remoteaddr":
			return requestutil.RemoteAddr(ctx.r)
		case "http.request.method":
			return ctx.r.Method
		case "http.request.host":
			return ctx.r.Host
		case "http.request.referer":
			referer := ctx.r.Referer()
			if referer != "" {
				return referer
			}
		case "http.request.useragent":
			return ctx.r.UserAgent()
		case "http.request.id":
			return ctx.id
		case "http.request.startedat":
			return ctx.startedAt
		case "http.request.contenttype":
			if ct := ctx.r.Header.Get("Content-Type"); ct != "" {
				return ct
			}
		default:
			// no match; fall back to standard behavior below
		}
	}

	return ctx.Context.Value(key)
}

type muxVarsContext struct {
	context.Context
	vars map[string]string
}

func (ctx *muxVarsContext) Value(key interface{}) interface{} {
	if keyStr, ok := key.(string); ok {
		if keyStr == "vars" {
			return ctx.vars
		}
		// TODO(thaJeztah): this considers "vars.FOO" and "FOO" to be equal.
		// We need to check if that's intentional (could be a bug).
		if v, ok := ctx.vars[strings.TrimPrefix(keyStr, "vars.")]; ok {
			return v
		}
	}

	return ctx.Context.Value(key)
}

// instrumentedResponseWriter provides response writer information in a
// context. This variant is only used in the case where CloseNotifier is not
// implemented by the parent ResponseWriter.
type instrumentedResponseWriter struct {
	http.ResponseWriter
	context.Context

	mu      sync.Mutex
	status  int
	written int64
}

func (irw *instrumentedResponseWriter) Write(p []byte) (n int, err error) {
	n, err = irw.ResponseWriter.Write(p)

	irw.mu.Lock()
	irw.written += int64(n)

	// Guess the likely status if not set.
	if irw.status == 0 {
		irw.status = http.StatusOK
	}

	irw.mu.Unlock()

	return
}

func (irw *instrumentedResponseWriter) WriteHeader(status int) {
	irw.ResponseWriter.WriteHeader(status)

	irw.mu.Lock()
	irw.status = status
	irw.mu.Unlock()
}

func (irw *instrumentedResponseWriter) Flush() {
	if flusher, ok := irw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (irw *instrumentedResponseWriter) Value(key interface{}) interface{} {
	if keyStr, ok := key.(string); ok {
		switch keyStr {
		case "http.response":
			return irw
		case "http.response.written":
			irw.mu.Lock()
			defer irw.mu.Unlock()
			return irw.written
		case "http.response.status":
			irw.mu.Lock()
			defer irw.mu.Unlock()
			return irw.status
		case "http.response.contenttype":
			if ct := irw.Header().Get("Content-Type"); ct != "" {
				return ct
			}
		default:
			// no match; fall back to standard behavior below
		}
	}

	return irw.Context.Value(key)
}
