package httpapi

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
)

// A typed error is answered with its category's status and its public
// message and code. An untyped error, or a category a client must not see,
// is a 500 that says nothing more.
func TestDefaultErrorMapper(t *testing.T) {
	internal := behemoth.M{"error": "internal server error"}
	for name, tc := range map[string]struct {
		err    error
		status int
		body   behemoth.M
	}{
		"a session error": {errs.NewSessionError("test", errs.CodeSessionExpired, nil), http.StatusUnauthorized,
			behemoth.M{"error": "session expired", "code": errs.CodeSessionExpired}},
		"a rate limit": {errs.NewRateLimited("test", "rule", 0), http.StatusTooManyRequests, nil},
		"a busy server": {errs.NewUnavailable("test", errs.CodeServerBusy, "server busy", time.Second), http.StatusServiceUnavailable,
			behemoth.M{"error": "server busy", "code": errs.CodeServerBusy}},
		"a configuration error": {errs.NewConfigurationError("test", "a mistake", nil), http.StatusInternalServerError, internal},
		"an untyped error":      {errors.New("boom"), http.StatusInternalServerError, internal},
		"a wrapped typed error": {errors.Join(errors.New("context"), errs.NewForbidden("test", errs.CodeInsufficientScope, "no")), http.StatusForbidden, nil},
	} {
		status, body := (&DefaultErrorMapper{}).Map(tc.err)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d", name, status, tc.status)
		}
		if tc.body != nil && (len(body) != len(tc.body) || body["error"] != tc.body["error"] || body["code"] != tc.body["code"]) {
			t.Errorf("%s: body %v, want %v", name, body, tc.body)
		}
	}
}
