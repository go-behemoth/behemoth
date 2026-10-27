package main

import (
	"context"
	"fmt"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
)

// signup creates a user and signs them in without an HTTP request, by
// calling the email/password plugin's exported flows. The same hook
// handlers run as for the routes; their caller has no transport, and the
// session records no IP address or user agent. The command names itself
// as the caller, so the audit log credits it by name.
//
// invite is passed as input, exactly like the route's "inviteCode" field.
// The application's handler on auth.signUp.before picks it up from the
// payload (hooks.go).
func signup(ctx context.Context, email, password, invite string) error {
	obs, err := newObservability(ctx)
	if err != nil {
		return err
	}
	defer obs.shutdown(ctx) // a short-lived command has to flush before it exits
	_, plugs, sqlDB, err := boot(ctx, nil, obs.tel)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	ctx = identity.WithCaller(ctx, identity.System("example-signup"))
	input := behemoth.M{"email": email, "password": password}
	if invite != "" {
		input["inviteCode"] = invite
	}
	user, err := plugs.passwords.SignUp(ctx, input)
	if err != nil {
		return fmt.Errorf("sign-up: %w", err)
	}
	fmt.Printf("created user %s (%s)\n", user.ID, user.Email)

	result, err := plugs.passwords.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: email, Password: password})
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	fmt.Printf("signed in: session %s, state %s, ip %q\n", result.Session.ID, result.Session.State, result.Session.IPAddress)
	return nil
}
