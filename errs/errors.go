// Package errs is Behemoth's error taxonomy: DomainError, its categories
// and codes, the constructors of each kind of error, and the checks a caller
// branches on, such as IsNotFound and IsRejection.
package errs

import (
	"errors"
	"fmt"
	"time"
)

type Category string

const (
	CategoryValidation     Category = "validation"
	CategoryNotFound       Category = "not_found"
	CategoryUndefinedTable Category = "undefined_table" // the table itself doesn't exist — a schema condition, never "no rows"
	CategoryDuplicateKey   Category = "duplicate_key"
	CategoryForeignKey     Category = "foreign_key_violation"
	CategoryConflict       Category = "conflict"     // optimistic-lock / concurrent-update collisions
	CategoryUnauthorized   Category = "unauthorized" // not authenticated
	CategoryForbidden      Category = "forbidden"    // authenticated, not permitted
	CategoryRateLimited    Category = "rate_limited"
	// CategoryUnavailable is an operation the system has no room for at the
	// moment, such as a password hash while the hasher is at its bound. A
	// later attempt may succeed. Over HTTP it is a 503.
	CategoryUnavailable Category = "unavailable"
	// CategoryUnsupportedMedia is a request whose body is of a type the
	// operation does not accept. Over HTTP it is a 415.
	CategoryUnsupportedMedia Category = "unsupported_media_type"
	CategorySession          Category = "session"
	CategoryToken            Category = "token"
	CategoryTransaction      Category = "transaction"
	CategoryDatabase         Category = "database"        // infra failure, connection lost, timeout
	CategoryMigration        Category = "migration"       // migration errors
	CategoryInvalidKey       Category = "invalid_key"     // key is empty, malformed, or otherwise invalid for the operation
	CategoryKeyNotFound      Category = "key_not_found"   // key is valid but not present in the store
	CategoryNotImplemented   Category = "not_implemented" // interface not implemented
	CategoryConfiguration    Category = "configuration"   // boot and initialization problems
	CategorySecurity         Category = "security"        // runtime crypto/key failures. unknown version, decode failure, etc.
	CategoryInternal         Category = "internal"        // catch-all, should shrink over time as gaps get classified
)

// IsRejection reports whether an error of the category refuses the request,
// as opposed to reporting a failure of the system. A rejection is what a hook
// handler or a rate limit answers with to stop an operation: the caller did
// something that is not allowed, and the system worked. Every other category
// counts as a failure of the system, such as a database that is down.
//
// This is the one place that draws the line. Code that fires a failed hook
// point asks it, so that an outage is not counted or audited as a refused
// request.
func (c Category) IsRejection() bool {
	switch c {
	case CategoryValidation, CategoryConflict, CategoryUnauthorized,
		CategoryForbidden, CategoryRateLimited:
		return true
	}
	return false
}

const (
	CodeUnknown = "unknown_error"
)

type DomainError struct {
	Category Category
	Op       string
	Entity   string // "user" | "session" | "password" | ...
	Code     string

	// PublicMessage is the message safe to return to the API caller.
	PublicMessage   string
	InternalMessage string
	Original        error
	Retryable       bool // whether the operation that produced this error is worth retrying (e.g. connection timeout)
	RetryAfter      time.Duration

	// Columns are, for a duplicate key error, the canonical names of the
	// columns of the unique key the write broke, in the key's order. They
	// are nil when the adapter could not tell, and for every other kind of
	// error. Read them with [DuplicateKeyColumns].
	Columns []string
}

func (e *DomainError) Error() string {
	if e.InternalMessage != "" {
		return e.InternalMessage
	}
	return e.PublicMessage
}

func (e *DomainError) Unwrap() error { return e.Original } // preserves errors.Is/As chains through Original

// IsCategory reports whether err is a [DomainError] of category.
func IsCategory(err error, category Category) bool {
	if de, ok := errors.AsType[*DomainError](err); ok {
		return de.Category == category
	}
	return false
}

// IsRejection reports whether err is a typed refusal of the request (see
// [Category.IsRejection]). An error that is not a [DomainError] is not one:
// it is taken as a failure of the system, so a hook handler that means to
// refuse a request returns a typed error.
func IsRejection(err error) bool {
	if de, ok := errors.AsType[*DomainError](err); ok {
		return de.Category.IsRejection()
	}
	return false
}

func IsCode(err error, code string) bool {
	if de, ok := errors.AsType[*DomainError](err); ok {
		return de.Code == code
	}
	return false
}

func ClassifyCode(err error) string {
	if de, ok := errors.AsType[*DomainError](err); ok {
		return de.Code
	}
	return CodeUnknown
}

func NewRateLimited(op, ruleName string, retryAfter time.Duration) *DomainError {
	return &DomainError{
		Category:        CategoryRateLimited,
		Op:              op,
		Entity:          "rate_limit",
		Code:            "rate_limited",
		PublicMessage:   "too many requests, please try again later",
		InternalMessage: fmt.Sprintf("rule %q exceeded", ruleName),
		Retryable:       true,
		RetryAfter:      retryAfter,
	}
}

// CodeServerBusy is the code of an unavailable error for an operation
// refused because the server is at capacity, such as a password hash that
// waited too long for its turn.
const CodeServerBusy = "server_busy"

// NewUnavailable reports that the system can't take the operation now and
// that a later attempt may succeed. message is sent to the client.
// retryAfter, when positive, is how long the client should wait before it
// tries again. It maps to 503. An unavailable error is not a rejection (see
// [Category.IsRejection]): the request was not at fault.
func NewUnavailable(op, code, message string, retryAfter time.Duration) *DomainError {
	return &DomainError{
		Category:        CategoryUnavailable,
		Op:              op,
		Code:            code,
		PublicMessage:   message,
		InternalMessage: fmt.Sprintf("%s: %s", op, message),
		Retryable:       true,
		RetryAfter:      retryAfter,
	}
}

// NewUnauthorized reports that the caller could not be authenticated: wrong
// credentials at sign-in, for example. message is sent to the client, so it
// should not say which part was wrong. It maps to 401. A request that is
// authenticated but not allowed is CategoryForbidden instead.
func NewUnauthorized(op, code, message string) error {
	return &DomainError{
		Category:        CategoryUnauthorized,
		Op:              op,
		Code:            code,
		PublicMessage:   message,
		InternalMessage: fmt.Sprintf("%s: %s", op, message),
	}
}

// CodeInsufficientScope is the code of a forbidden error for a caller that is
// authenticated and doesn't hold the permission an operation requires. It is
// the code RFC 6750 gives a bearer token that lacks a scope.
const CodeInsufficientScope = "insufficient_scope"

// NewForbidden reports that the caller is authenticated and not allowed what
// it asked for, such as a credential without the permission an operation
// requires (CodeInsufficientScope). message is sent to the client. It maps to
// 403. A caller that could not be authenticated gets NewUnauthorized or a
// session error instead.
func NewForbidden(op, code, message string) error {
	return &DomainError{
		Category:        CategoryForbidden,
		Op:              op,
		Code:            code,
		PublicMessage:   message,
		InternalMessage: fmt.Sprintf("%s: %s", op, message),
	}
}

// NewInternalError classifies an unexpected failure inside the system
func NewInternalError(op string, original error) *DomainError {
	return &DomainError{
		Category:        CategoryInternal,
		Op:              op,
		Code:            "internal_error",
		PublicMessage:   "an internal error occurred",
		InternalMessage: fmt.Sprintf("%s: %v", op, original),
		Original:        original,
	}
}

func NewMigrationError(op, code string, original error) *DomainError {
	return &DomainError{
		Category: CategoryMigration, Op: op, Code: code,
		PublicMessage:   "migration failed", // never client-facing anyway — this surfaces to an operator/CLI, not an HTTP response
		InternalMessage: fmt.Sprintf("%s: %v", op, original),
		Original:        original,
	}
}
