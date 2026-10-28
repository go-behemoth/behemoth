package core

import (
	"strings"
	"testing"
	"time"
)

// A zero SessionConfig takes every documented default, and a field that was
// set is kept.
func TestSessionConfigDefaults(t *testing.T) {
	got := SessionConfig{}.WithDefaults()
	want := SessionConfig{
		ExpiresIn: 7 * 24 * time.Hour, PendingExpiresIn: 10 * time.Minute, FreshAge: 15 * time.Minute,
		UnversionedCacheTTL: time.Minute,
		// UpdateAge stays zero: rolling expiration is off unless asked for.
	}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}

	set := SessionConfig{
		ExpiresIn: time.Hour, PendingExpiresIn: time.Minute, UpdateAge: time.Second,
		FreshAge: 2 * time.Minute, MaxConcurrent: 3, CaptureIPAndAgent: true, UnversionedCacheTTL: 5 * time.Second,
	}
	if got := set.WithDefaults(); got != set {
		t.Errorf("WithDefaults changed a config that was set: %+v", got)
	}
}

// Validate refuses what no default can repair, and accepts zero fields.
func TestSessionConfigValidate(t *testing.T) {
	if err := (SessionConfig{}).Validate(); err != nil {
		t.Errorf("a zero config: %v", err)
	}
	for name, tc := range map[string]struct {
		cfg  SessionConfig
		want string
	}{
		"a negative lifetime":         {SessionConfig{ExpiresIn: -time.Hour}, "ExpiresIn"},
		"a negative pending lifetime": {SessionConfig{PendingExpiresIn: -time.Hour}, "PendingExpiresIn"},
		"a negative update age":       {SessionConfig{UpdateAge: -time.Hour}, "UpdateAge"},
		"a negative fresh age":        {SessionConfig{FreshAge: -time.Hour}, "FreshAge"},
		"a negative cache lifetime":   {SessionConfig{UnversionedCacheTTL: -time.Hour}, "UnversionedCacheTTL"},
	} {
		err := tc.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %s", name, err, tc.want)
		}
	}
}
