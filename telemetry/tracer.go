package telemetry

import (
	"context"
	"errors"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
)

// Tracer starts spans. It is the tracing counterpart of Logger and Metrics:
// behemoth names the spans and decides where they start and end, and a
// backend records them. The OpenTelemetry adapter
// (telemetry/adapters/otel) is the backend for OTLP collectors, Jaeger
// among them.
//
// Start returns a context carrying the new span. Work done with that
// context nests under it, behemoth's own spans and the backend's alike.
// behemoth reads no trace headers: the application's HTTP middleware puts
// the incoming trace on the request's context, and behemoth's first span is
// started from that context.
type Tracer interface {
	Start(ctx context.Context, name string, attrs behemoth.M) (context.Context, Span)
}

// Span is one timed piece of work. End must be called exactly once.
type Span interface {
	// SetAttributes adds attributes known only after the span started,
	// such as a response status.
	SetAttributes(attrs behemoth.M)
	// RecordError marks the span as failed with err. A nil err is ignored.
	RecordError(err error)
	End()
}

// NoOpTracer starts spans that record nothing. New uses it when no tracer
// is given.
type NoOpTracer struct{}

func (NoOpTracer) Start(ctx context.Context, _ string, _ behemoth.M) (context.Context, Span) {
	return ctx, noOpSpan{}
}

type noOpSpan struct{}

func (noOpSpan) SetAttributes(behemoth.M) {}
func (noOpSpan) RecordError(error)        {}
func (noOpSpan) End()                     {}

// WithTracer sets the Telemetry's tracer. It is an option and not a fourth
// argument of New because most applications that trace also log and measure
// through the same backend, and build all three from one place.
func WithTracer(t Tracer) Option {
	return func(o *options) { o.tracer = t }
}

// Span names. Like the metric names they are part of behemoth's public
// contract. docs/api/telemetry.md lists each with its attributes, which use
// the Attr keys of the metrics.
const (
	SpanRequest = "behemoth.request" // one request to a behemoth route

	SpanHookChain   = "behemoth.hook.chain"   // the handlers of one hook point
	SpanHookHandler = "behemoth.hook.handler" // one handler call

	// SpanStorePrefix is followed by the database operation:
	// "behemoth.store.find_one", "behemoth.store.transaction".
	SpanStorePrefix = "behemoth.store."

	SpanSessionCreate   = "behemoth.session.create"
	SpanSessionValidate = "behemoth.session.validate"
	SpanSessionRevoke   = "behemoth.session.revoke"

	SpanTokenIssue   = "behemoth.token.issue"
	SpanTokenConsume = "behemoth.token.consume"

	SpanPasswordHash   = "behemoth.password.hash"
	SpanPasswordVerify = "behemoth.password.verify"

	SpanRateLimitCheck = "behemoth.ratelimit.check"

	SpanMigrationApply = "behemoth.migration.apply"

	// SpanOutboundRequest is one call behemoth makes to another service,
	// through a client of package outbound. It ends when the answer's body
	// is closed.
	SpanOutboundRequest = "behemoth.outbound.request"
)

// AttrRequestID is the span attribute holding the request ID, the same
// value logs carry as FieldRequestID and audit events as RequestID.
const AttrRequestID = "request_id"

// AttrErrorCode is the span attribute holding a failed operation's code.
const AttrErrorCode = "error_code"

// TracingEnabled reports whether spans go anywhere. A call site on a hot
// path checks it before it builds attributes.
func (t *Telemetry) TracingEnabled() bool {
	if t == nil || t.Tracer == nil {
		return false
	}
	_, off := t.Tracer.(NoOpTracer)
	return !off
}

type spanKey struct{}

// StartSpan starts a span and returns the context carrying it. It is safe
// on a nil Telemetry, which starts nothing.
//
// The span is also kept on the context for SpanFrom, so code further down
// that only has the context can add to it.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs behemoth.M) (context.Context, Span) {
	if !t.TracingEnabled() {
		return ctx, noOpSpan{}
	}
	ctx, span := t.Tracer.Start(ctx, name, attrs)
	return context.WithValue(ctx, spanKey{}, span), span
}

// SpanFrom returns the innermost span StartSpan put on ctx, or a span that
// records nothing. The router uses it to mark a request's span as failed
// from the place the error is mapped.
func SpanFrom(ctx context.Context) Span {
	if span, ok := ctx.Value(spanKey{}).(Span); ok {
		return span
	}
	return noOpSpan{}
}

// FinishSpan ends span, describing err on it first. A failure of the system
// marks the span as failed (RecordError). A rejection of the request, such
// as an expired session or a hook handler's veto, only adds its code and
// category as attributes: it is an ordinary outcome, and a trace view that
// highlights errors should not light up for a wrong password. The split is
// IsSystemFailure, the same one the router uses for its log level.
func FinishSpan(span Span, err error) {
	if err != nil {
		attrs := behemoth.M{AttrErrorCode: errs.ClassifyCode(err)}
		if de, ok := errors.AsType[*errs.DomainError](err); ok {
			attrs[AttrErrorCategory] = string(de.Category)
		}
		span.SetAttributes(attrs)
		if IsSystemFailure(err) {
			span.RecordError(err)
		}
	}
	span.End()
}

// IsSystemFailure reports whether err means behemoth or something it
// depends on failed, as opposed to the request being refused. An error
// outside the taxonomy counts as a failure, like the router's mapping of it
// to a 500. An unavailable error (errs.CategoryUnavailable) does not: the
// system refused work it had no room for, as it is set up to.
func IsSystemFailure(err error) bool {
	if err == nil {
		return false
	}
	de, ok := errors.AsType[*errs.DomainError](err)
	if !ok {
		return true
	}
	switch de.Category {
	case errs.CategoryDatabase, errs.CategoryTransaction, errs.CategoryUndefinedTable,
		errs.CategoryInternal, errs.CategoryConfiguration, errs.CategorySecurity,
		errs.CategoryNotImplemented, errs.CategoryMigration:
		return true
	}
	return false
}
