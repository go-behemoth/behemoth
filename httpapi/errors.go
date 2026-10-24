package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
)

// ErrorMapper turns the error a route returned into the status and the JSON
// body the client receives. RouterConfig.ErrorMapper is one; the router uses
// DefaultErrorMapper when it is nil.
type ErrorMapper interface {
	Map(err error) (status int, body behemoth.M)
}

// DefaultErrorMapper answers an *errs.DomainError with the status of its
// category and a body of its public message and code:
//
//	{"error": "session expired", "code": "session_expired"}
//
// Any other error, and a category that has no status here, such as
// errs.CategoryConfiguration and errs.CategoryInternal, is a 500 with a
// generic body, so that nothing internal reaches the client.
type DefaultErrorMapper struct{}

// Map implements [ErrorMapper].
func (dem *DefaultErrorMapper) Map(err error) (status int, body behemoth.M) {
	de, ok := errors.AsType[*errs.DomainError](err)
	if !ok {
		return http.StatusInternalServerError, behemoth.M{"error": "internal server error"}
	}
	status, ok = categoryToStatus[de.Category]
	if !ok {
		return http.StatusInternalServerError, behemoth.M{"error": "internal server error"}
	}
	return status, behemoth.M{"error": de.PublicMessage, "code": de.Code}
}

// categoryToStatus is the status of each category a client may see. A
// category without an entry is answered with 500.
var categoryToStatus = map[errs.Category]int{
	errs.CategoryValidation:       http.StatusBadRequest,
	errs.CategoryNotFound:         http.StatusNotFound,
	errs.CategoryDuplicateKey:     http.StatusConflict,
	errs.CategoryForeignKey:       http.StatusConflict,
	errs.CategoryConflict:         http.StatusConflict,
	errs.CategoryUnauthorized:     http.StatusUnauthorized,
	errs.CategoryForbidden:        http.StatusForbidden,
	errs.CategoryRateLimited:      http.StatusTooManyRequests,
	errs.CategoryUnavailable:      http.StatusServiceUnavailable,
	errs.CategoryUnsupportedMedia: http.StatusUnsupportedMediaType,
	errs.CategorySession:          http.StatusUnauthorized,
	errs.CategoryToken:            http.StatusUnauthorized,
	errs.CategoryTransaction:      http.StatusInternalServerError,
	errs.CategoryDatabase:         http.StatusInternalServerError,
	errs.CategoryUndefinedTable:   http.StatusInternalServerError,
}
