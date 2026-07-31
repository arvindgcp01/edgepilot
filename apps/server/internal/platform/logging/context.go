package logging

import (
	"context"
	"log/slog"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	traceIDKey
	actorKey
	loggerKey
)

// Actor identifies whoever caused the work being logged.
//
// Carried in the context rather than threaded through every signature, because
// the alternative — passing it explicitly everywhere — means it gets dropped at
// the first function that did not think it needed it, and the audit trail
// thins out precisely in the deep code paths where it matters most.
type Actor struct {
	UserID   string
	OrgID    string
	DeviceID string
}

// WithRequestID returns a context carrying a request identifier. It is emitted
// on every log line made with that context, returned to the client in the
// response headers and in any Problem Details body, and recorded on the audit
// entry — so one identifier ties together everything a single request touched.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request identifier, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithTraceID returns a context carrying a distributed-trace identifier,
// propagated from the incoming traceparent header or from an agent stream
// message.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey, id)
}

// TraceIDFromContext returns the trace identifier, or "".
func TraceIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey).(string)
	return id
}

// WithActor returns a context carrying the acting principal.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}

// ActorFromContext returns the acting principal, if any.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey).(Actor)
	return a, ok
}

// WithLogger attaches a logger to a context, for the occasional case where a
// particular scope needs extra bound attributes.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the logger attached to ctx, falling back to the default.
// It never returns nil, so callers need no nil check — a logging call is not
// somewhere to introduce a panic.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// contextHandler decorates every record with the identifiers held in its
// context, and collapses groups logged under a sensitive name.
//
// The alternative to the first job is asking every call site to remember
// slog.String("request_id", ...), which works right up until the one place that
// forgets — invariably the error path, where the correlation was most needed.
//
// Groups are managed here rather than forwarded straight to the inner handler.
// Forwarding is the obvious implementation and it is wrong: once a caller opens
// a group with WithGroup, anything the handler adds lands *inside* that group,
// so `request_id` ends up nested under `op` and stops being greppable. Instead
// the group stack is tracked here, applied only to the record's own attributes,
// and the correlation identifiers are emitted at the top level where they
// belong.
type contextHandler struct {
	// base never has a group applied, so context attributes always land at the
	// top level.
	base slog.Handler
	// groups are those opened by callers via WithGroup, outermost first.
	groups []string
	// attrs are those bound via WithAttrs *after* the first group was opened,
	// and so belong inside the group stack rather than on base.
	attrs []slog.Attr
}

func (h contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	// Fast path: no groups open and nothing to collapse, so the record can be
	// decorated in place and passed straight through.
	if len(h.groups) == 0 && len(h.attrs) == 0 {
		if !recordHasSensitiveGroup(r) {
			addContextAttrs(ctx, &r)
			return h.base.Handle(ctx, r)
		}
	}

	recordAttrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	recordAttrs = append(recordAttrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		recordAttrs = append(recordAttrs, a)
		return true
	})
	recordAttrs, _ = sanitizeGroups(recordAttrs)

	// Nest the record's attributes back inside the caller's group stack,
	// innermost first. An empty group is dropped, matching slog's own
	// behaviour.
	for i := len(h.groups) - 1; i >= 0; i-- {
		if len(recordAttrs) == 0 {
			break
		}
		recordAttrs = []slog.Attr{{
			Key:   h.groups[i],
			Value: slog.GroupValue(recordAttrs...),
		}}
	}

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	addContextAttrs(ctx, &out)
	out.AddAttrs(recordAttrs...)
	return h.base.Handle(ctx, out)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// Before any group is opened these are top-level, so let the inner handler
	// pre-format them — that is the whole point of WithAttrs.
	if len(h.groups) == 0 {
		return contextHandler{base: h.base.WithAttrs(attrs)}
	}
	next := h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return next
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h
	next.groups = append(append([]string(nil), h.groups...), name)
	return next
}

func addContextAttrs(ctx context.Context, r *slog.Record) {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if id := TraceIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	a, ok := ActorFromContext(ctx)
	if !ok {
		return
	}
	if a.UserID != "" {
		r.AddAttrs(slog.String("user_id", a.UserID))
	}
	if a.OrgID != "" {
		r.AddAttrs(slog.String("org_id", a.OrgID))
	}
	if a.DeviceID != "" {
		r.AddAttrs(slog.String("device_id", a.DeviceID))
	}
}

func recordHasSensitiveGroup(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindGroup &&
			(IsSensitiveKey(a.Key) || anySensitiveGroup(a.Value.Group())) {
			found = true
			return false
		}
		return true
	})
	return found
}
