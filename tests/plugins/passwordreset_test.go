package plugins_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	resetEmail       = "ada@example.com"
	resetOldPassword = "correct horse battery"
	resetNewPassword = "a brand new staple"
)

type resetApp struct {
	plugin  *emailpassword.Plugin
	ac      *core.AuthContext
	outbox  *outbox
	rec     *telemetrytest.Recorder
	mounted mountedRoutes
}

// bootReset boots the email/password plugin with reset enabled over a fresh
// database, and signs up "ada@example.com", whose email is not verified.
// The request waits for the sender unless opts says otherwise, so a test
// reads the outbox without polling.
func bootReset(t *testing.T, opts emailpassword.ResetOptions, hookFn func(reg core.HookRegistry) error) (*resetApp, *models.User) {
	t.Helper()
	return bootResetWith(t, opts, core.TokenConfig{}, resetDBParams, hookFn)
}

// The connection parameters of the SQLite database of a reset test. A test
// that calls the flows from several goroutines needs the second set, which
// is what bootRace uses: a write waits for the lock instead of failing, and
// a transaction takes it when it begins, since one that reads first and
// writes after is refused at once when another holds the lock.
const (
	resetDBParams           = "?_foreign_keys=on"
	resetConcurrentDBParams = "?_foreign_keys=on&_busy_timeout=10000&_txlock=immediate"
)

// bootResetWith is bootReset with the token configuration of BootConfig.Token
// and the connection parameters of the database.
func bootResetWith(t *testing.T, opts emailpassword.ResetOptions, tokens core.TokenConfig, dbParams string, hookFn func(reg core.HookRegistry) error) (*resetApp, *models.User) {
	t.Helper()
	return bootResetPlugin(t, emailpassword.Options{Reset: opts}, tokens, dbParams, nil, hookFn)
}

// bootResetPlugin is bootResetWith for a plugin with options of its own,
// such as password rules, and a wrapper for the database, which a test uses
// to make a call fail. A nil wrapDB leaves the database as it is. Its Reset
// options get the default link URL.
func bootResetPlugin(t *testing.T, pluginOpts emailpassword.Options, tokens core.TokenConfig, dbParams string, wrapDB func(behemoth.Database) behemoth.Database, hookFn func(reg core.HookRegistry) error) (*resetApp, *models.User) {
	t.Helper()
	ctx := context.Background()
	opts := pluginOpts.Reset
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "reset.db")+dbParams)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(magicLinkSchema)
	require.NoError(t, err)

	app := &resetApp{outbox: &outbox{}, mounted: mountedRoutes{}}
	if opts.LinkURL == "" {
		opts.LinkURL = "https://app.example.com/auth/reset?from=email"
	}
	pluginOpts.Reset = opts
	app.plugin = emailpassword.New(pluginOpts)
	tel, rec := telemetrytest.New()
	app.rec = rec

	prepared, err := engine.Prepare([]core.Plugin{app.plugin}, engine.PrepareConfig{})
	require.NoError(t, err)
	var database behemoth.Database = sqliteAdapter.NewSQLiteAdapter(db, nil)
	if wrapDB != nil {
		database = wrapDB(database)
	}
	app.ac, err = engine.Boot(ctx, prepared, database, engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:    httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		Mail:      core.MailConfig{Sender: app.outbox},
		Token:     tokens,
		Telemetry: tel,
		HTTP:      app.mounted,
		Hooks:     hookFn,
	})
	require.NoError(t, err)
	t.Cleanup(func() { app.ac.Mailer.Close(context.Background()) })

	user, err := app.plugin.SignUp(ctx, behemoth.M{"email": resetEmail, "password": resetOldPassword})
	require.NoError(t, err)
	return app, user
}

func (app *resetApp) signIn(password string) (*emailpassword.SignInResult, error) {
	return app.plugin.SignIn(context.Background(), emailpassword.EmailAndPasswordCredentials{Email: resetEmail, Password: password})
}

// link requests a reset for Ada and returns the message that was sent.
func (app *resetApp) link(t *testing.T) core.MailMessage {
	t.Helper()
	_, err := app.plugin.RequestPasswordReset(context.Background(), emailpassword.ResetRequest{Email: resetEmail})
	require.NoError(t, err)
	return app.outbox.last(t)
}

// expireLink moves the expiry of the stored link with id into the past. Boot
// takes no clock, so the row is edited instead of waiting for the time to
// pass.
func (app *resetApp) expireLink(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, app.ac.DB.UpdateOne(context.Background(), &models.Token{},
		clause.Expression{Conditions: []clause.Condition{{Field: models.TokenID, Operator: clause.OpEqual, Value: id}}},
		behemoth.M{models.TokenExpiresAt: time.Now().UTC().Add(-time.Minute)}))
}

func invalidResetToken(err error) bool {
	de, ok := errors.AsType[*errs.DomainError](err)
	return ok && de.Category == errs.CategoryUnauthorized && de.Code == emailpassword.ErrorCodeInvalidResetToken
}

// The two flows called from code: a link is sent to a known email only, a
// new link replaces the old one, and a link sets the password once, ends the
// user's sessions and marks the email verified.
func TestPasswordResetRequestAndConfirm(t *testing.T) {
	ctx := context.Background()
	var befores []behemoth.M
	var afters []any
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(emailpassword.HookResetBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			befores = append(befores, payload)
			return behemoth.M{"password": "swapped by a handler"}, nil
		}, nil); err != nil {
			return err
		}
		return reg.OnAfter(emailpassword.HookResetAfter, func(_ *core.HookContext, result any) error {
			afters = append(afters, result)
			return nil
		}, nil)
	})
	p := app.plugin

	_, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "nobody@example.com"})
	assert.ErrorIs(t, err, emailpassword.ErrNoAccount)
	_, err = p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "not an email"})
	assert.True(t, errs.IsValidationError(err), "%v", err)
	require.Zero(t, app.outbox.count(), "neither sent a link")

	before, err := app.signIn(resetOldPassword)
	require.NoError(t, err)

	sent, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "  Ada@Example.com ", Metadata: behemoth.M{"locale": "fr"}})
	require.NoError(t, err)
	first := app.outbox.last(t)
	assert.Equal(t, core.MailPasswordReset, first.Kind)
	assert.Equal(t, resetEmail, first.To, "normalized")
	assert.Equal(t, behemoth.M{"locale": "fr"}, first.Metadata, "passed to the sender as given")
	assert.Equal(t, user.ID, first.User.ID)
	assert.WithinDuration(t, time.Now().Add(emailpassword.DefaultResetTTL), first.ExpiresAt, time.Minute)
	assert.Equal(t, &emailpassword.ResetRequestResult{UserID: user.ID, Email: resetEmail, TokenID: sent.TokenID, ExpiresAt: first.ExpiresAt}, sent)
	link, err := url.Parse(first.URL)
	require.NoError(t, err)
	assert.Equal(t, "https://app.example.com/auth/reset", link.Scheme+"://"+link.Host+link.Path)
	assert.Equal(t, first.Token, link.Query().Get("token"))
	assert.Equal(t, "email", link.Query().Get("from"), "the query LinkURL came with is kept")

	// Asking for a link changes nothing yet.
	_, err = app.ac.SessionManager.Validate(ctx, before.RawToken)
	require.NoError(t, err, "the session is still live")

	// A second request replaces the first link.
	second := app.link(t)
	_, err = p.ResetPassword(ctx, first.Token, resetNewPassword)
	assert.True(t, invalidResetToken(err), "the replaced link: %v", err)

	// A password the rules refuse does not use the link up.
	_, err = p.ResetPassword(ctx, second.Token, "short")
	assert.True(t, errs.IsValidationError(err), "%v", err)

	reset, err := p.ResetPassword(ctx, second.Token, resetNewPassword)
	require.NoError(t, err)
	assert.Equal(t, user.ID, reset.ID)
	assert.True(t, reset.EmailVerified, "following the link verifies the address")
	stored, err := app.ac.Store.FindUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.True(t, stored.EmailVerified)

	_, err = app.signIn(resetOldPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the old password: %v", err)
	_, err = app.signIn("swapped by a handler")
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "a before handler can't swap the password: %v", err)
	_, err = app.signIn(resetNewPassword)
	require.NoError(t, err, "the new password signs in")
	_, err = app.ac.SessionManager.Validate(ctx, before.RawToken)
	assert.Error(t, err, "the session from before the reset has ended")

	// The owner is told, in the background, by a message without a link.
	require.Eventually(t, func() bool { return app.outbox.last(t).Kind == core.MailPasswordChanged }, 5*time.Second, time.Millisecond)
	notice := app.outbox.last(t)
	assert.Equal(t, resetEmail, notice.To)
	assert.Equal(t, user.ID, notice.User.ID)
	assert.Empty(t, notice.Token)
	assert.Empty(t, notice.URL)
	assert.Equal(t, 3, app.outbox.count(), "two links and one notice: a refused reset sends none")

	_, err = p.ResetPassword(ctx, second.Token, "yet another password")
	assert.True(t, invalidResetToken(err), "a link works once: %v", err)
	_, err = p.ResetPassword(ctx, "not-a-token", "yet another password")
	assert.True(t, invalidResetToken(err), "an unknown token gets the same answer: %v", err)

	// auth.passwordReset.before saw the email and the password, never the
	// token. The call with the refused password went through it too.
	require.Len(t, befores, 5)
	assert.Equal(t, behemoth.M{"email": resetEmail, "password": resetNewPassword}, befores[2])
	assert.Equal(t, behemoth.M{"password": "yet another password"}, befores[4], "no email for a token that does not check out")
	require.Len(t, afters, 1)
	assert.Same(t, reset, afters[0])

	// Both flows are audited under the plugin's point names.
	requested := app.rec.Audit.OfType(string(emailpassword.HookResetRequestAfter))
	require.Len(t, requested, 2)
	assert.Equal(t, user.ID, requested[0].SubjectID)
	requestFailed := app.rec.Audit.OfType(string(emailpassword.HookResetRequestFailed))
	require.Len(t, requestFailed, 1)
	assert.Equal(t, "user_not_found", requestFailed[0].Metadata["code"])
	done := app.rec.Audit.OfType(string(emailpassword.HookResetAfter))
	require.Len(t, done, 1)
	assert.Equal(t, user.ID, done[0].SubjectID)
	failed := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.Len(t, failed, 3)
	assert.Equal(t, "invalid_token", failed[0].Metadata["code"])
}

// The routes: the request route answers the same for a known email, an
// unknown one and a failed send, and the confirm route sets the password
// without signing the user in.
func TestPasswordResetRoutes(t *testing.T) {
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
	request := app.mounted["/api/auth"+emailpassword.PathResetRequest]
	confirm := app.mounted["/api/auth"+emailpassword.PathResetConfirm]
	require.NotNil(t, request.Handler)
	require.NotNil(t, confirm.Handler)
	assert.Equal(t, http.MethodPost, confirm.Method)

	known := call(t, app.ac, request, `{"email":"ada@example.com","metadata":{"locale":"fr"}}`)
	require.Equal(t, http.StatusOK, known.Code, known.Body.String())
	link := app.outbox.last(t)
	assert.Equal(t, behemoth.M{"locale": "fr"}, link.Metadata)

	unknown := call(t, app.ac, request, `{"email":"nobody@example.com"}`)
	assert.Equal(t, known.Code, unknown.Code)
	assert.Equal(t, known.Body.String(), unknown.Body.String(), "the response does not tell which emails have an account")
	assert.Equal(t, 1, app.outbox.count())

	app.outbox.err = errors.New("smtp down")
	failed := call(t, app.ac, request, `{"email":"ada@example.com"}`)
	assert.Equal(t, known.Code, failed.Code)
	assert.Equal(t, known.Body.String(), failed.Body.String(), "a failed send is not told apart either")
	assert.NotEmpty(t, app.rec.Logger.At(slog.LevelError), "it is logged instead")
	unsent := app.outbox.last(t)
	app.outbox.err = nil
	w := call(t, app.ac, confirm, `{"token":"`+unsent.Token+`","password":"`+resetNewPassword+`"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the link that was not sent is revoked")

	for body, want := range map[string]int{
		`{`:                        http.StatusBadRequest,
		`{"email":42}`:             http.StatusBadRequest,
		`{"email":"not an email"}`: http.StatusBadRequest,
		`{"email":"ada@example.com","metadata":"x"}`: http.StatusBadRequest,
	} {
		assert.Equal(t, want, call(t, app.ac, request, body).Code, "request body %s", body)
	}

	require.Equal(t, http.StatusOK, call(t, app.ac, request, `{"email":"ada@example.com"}`).Code)
	link = app.outbox.last(t)
	for body, why := range map[string]string{
		`{`: "not JSON",
		`{"password":"` + resetNewPassword + `"}`:           "no token",
		`{"token":"` + link.Token + `"}`:                    "no password",
		`{"token":"` + link.Token + `","password":"short"}`: "a password the rules refuse",
	} {
		assert.Equal(t, http.StatusBadRequest, call(t, app.ac, confirm, body).Code, why)
	}

	w = call(t, app.ac, confirm, `{"token":"`+link.Token+`","password":"`+resetNewPassword+`"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"status":"password_reset"}`, w.Body.String())
	assert.Empty(t, w.Header().Get(httpapi.SessionTokenHeader), "a reset does not sign the user in")
	_, err := app.signIn(resetNewPassword)
	require.NoError(t, err)

	w = call(t, app.ac, confirm, `{"token":"`+link.Token+`","password":"`+resetNewPassword+`"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), emailpassword.ErrorCodeInvalidResetToken)
}

// Reset requests are limited per email from any caller, and the limit
// answers the same for an email without an account.
func TestPasswordResetRequestsAreLimitedPerEmail(t *testing.T) {
	ctx := context.Background()
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true, RequestLimit: core.Limit{Max: 2, Window: time.Minute}}, nil)
	limited := func(email string) bool {
		_, err := app.plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: email})
		return errs.IsCategory(err, errs.CategoryRateLimited)
	}
	for _, email := range []string{resetEmail, "nobody@example.com"} {
		assert.False(t, limited(email), email)
		assert.False(t, limited(strings.ToUpper(email)), "%s: the second request, in another spelling", email)
		assert.True(t, limited(email), "%s: the third request is refused", email)
	}
	assert.Equal(t, 2, app.outbox.count(), "only the known email was sent links")

	events := app.rec.Audit.OfType(telemetry.AuditRateLimitExceeded)
	require.Len(t, events, 2)
	assert.Equal(t, emailpassword.RuleResetRequestEmail, events[0].Metadata["rule"])
}

// overPeer returns a context whose caller is at the client address ip, over a
// transport that is not HTTP, as a gRPC handler would pass it. A caller with
// no address, such as code in the process, is not counted by the per-address
// rules.
func overPeer(ip string) context.Context {
	return identity.WithCaller(context.Background(), identity.Anonymous(identity.Peer{Transport: "grpc", IPAddress: ip}))
}

// callFrom posts body to a mounted route as a client at addr, through the
// router that wraps the route. The router resolves the client address from
// the request.
func callFrom(t *testing.T, route httpapi.Route, addr, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, route.Path, strings.NewReader(body))
	req.RemoteAddr = addr
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
	require.NoError(t, route.Handler(rctx))
	w := httptest.NewRecorder()
	rctx.Response.Flush(w)
	return w
}

// Requests for a link, and attempts to use one, are limited to ten a minute
// per client address. The limits sit in the flows, so a caller over any
// transport is counted by its address. A refused call changes nothing: it
// sends no link and leaves a valid link unused. The two flows count
// separately, and another address has its own count.
func TestPasswordResetIsLimitedPerAddress(t *testing.T) {
	const noisy, quiet = "203.0.113.7", "198.51.100.9"

	t.Run("requests", func(t *testing.T) {
		app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
		request := func(ip, email string) error {
			_, err := app.plugin.RequestPasswordReset(overPeer(ip), emailpassword.ResetRequest{Email: email})
			return err
		}

		require.NoError(t, request(noisy, resetEmail))
		for i := 2; i <= 10; i++ {
			// A request for an email with no account counts as well, so the
			// limit does not tell which addresses have one.
			err := request(noisy, fmt.Sprintf("nobody%d@example.com", i))
			require.ErrorIs(t, err, emailpassword.ErrNoAccount, "request %d reaches the flow", i)
		}
		err := request(noisy, resetEmail)
		assert.True(t, errs.IsCategory(err, errs.CategoryRateLimited), "the eleventh request: %v", err)
		assert.Equal(t, 1, app.outbox.count(), "the refused request sent no link")

		require.NoError(t, request(quiet, resetEmail), "another address has its own count")
		assert.Equal(t, 2, app.outbox.count())

		events := app.rec.Audit.OfType(telemetry.AuditRateLimitExceeded)
		require.Len(t, events, 1)
		assert.Equal(t, emailpassword.RuleResetRequestAddress, events[0].Metadata["rule"])
		assert.Equal(t, noisy, events[0].IPAddress)
	})

	t.Run("confirmations", func(t *testing.T) {
		app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
		link := app.link(t) // asked for in-process: no address, so not counted
		confirm := func(ip, token string) error {
			_, err := app.plugin.ResetPassword(overPeer(ip), token, resetNewPassword)
			return err
		}

		for i := 1; i <= 10; i++ {
			err := confirm(noisy, fmt.Sprintf("guess-%d", i))
			require.True(t, invalidResetToken(err), "attempt %d reaches the flow: %v", i, err)
		}
		err := confirm(noisy, link.Token)
		assert.True(t, errs.IsCategory(err, errs.CategoryRateLimited), "the eleventh attempt, with the right link: %v", err)

		// The refusal came before the link was looked at, so it is still
		// usable, from an address with its own count.
		_, err = app.plugin.ResetPassword(overPeer(quiet), link.Token, resetNewPassword)
		require.NoError(t, err, "the refused attempt did not use the link up")
		// The address that used up its attempts can still ask for a link.
		_, err = app.plugin.RequestPasswordReset(overPeer(noisy), emailpassword.ResetRequest{Email: resetEmail})
		require.NoError(t, err, "requests are counted apart from attempts")

		events := app.rec.Audit.OfType(telemetry.AuditRateLimitExceeded)
		require.Len(t, events, 1)
		assert.Equal(t, emailpassword.RuleResetConfirmAddress, events[0].Metadata["rule"])
		assert.Equal(t, noisy, events[0].IPAddress)
	})
}

// Through the routes the eleventh call from an address is answered 429 with
// the time left in the window. The address is the one of the connection. The
// two routes count separately, and another address is not affected.
func TestPasswordResetRoutesAreLimitedPerAddress(t *testing.T) {
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)

	// The same address is used up on both routes in turn, which shows that
	// they don't share a count.
	for _, tc := range []struct {
		name    string
		route   httpapi.Route
		body    func(i int) string
		reaches int // the status of a call the limit lets through
	}{
		{
			"request", app.mounted["/api/auth"+emailpassword.PathResetRequest],
			func(i int) string { return fmt.Sprintf(`{"email":"nobody%d@example.com"}`, i) },
			http.StatusOK,
		},
		{
			"confirm", app.mounted["/api/auth"+emailpassword.PathResetConfirm],
			func(i int) string { return fmt.Sprintf(`{"token":"guess-%d","password":%q}`, i, resetNewPassword) },
			http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.route.Handler)
			for i := 1; i <= 10; i++ {
				require.Equal(t, tc.reaches, callFrom(t, tc.route, "203.0.113.7:50000", tc.body(i)).Code, "call %d reaches the route", i)
			}
			w := callFrom(t, tc.route, "203.0.113.7:50001", tc.body(11))
			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
			wait, err := strconv.Atoi(w.Header().Get("Retry-After"))
			require.NoError(t, err, "Retry-After: %q", w.Header().Get("Retry-After"))
			assert.Greater(t, wait, 0)
			assert.LessOrEqual(t, wait, 60, "the time left in the window")

			assert.Equal(t, tc.reaches, callFrom(t, tc.route, "198.51.100.9:50000", tc.body(12)).Code, "another address has its own count")
		})
	}
}

// A link stops working when the account it was sent for is no longer the
// one at that address.
func TestPasswordResetRefusals(t *testing.T) {
	ctx := context.Background()
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
	p := app.plugin

	// The email changes after the link was sent.
	stale := app.link(t)
	_, err := app.ac.Store.UpdateUser(ctx, user.ID, behemoth.M{models.UserEmail: "ada@new.example.com"})
	require.NoError(t, err)
	_, err = p.ResetPassword(ctx, stale.Token, resetNewPassword)
	assert.True(t, invalidResetToken(err), "a link sent to the old address: %v", err)
	failures := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.NotEmpty(t, failures)
	assert.Equal(t, "email_changed", failures[len(failures)-1].Metadata["code"])
	assert.Equal(t, user.ID, failures[len(failures)-1].SubjectID)

	// The user is deleted after the link was sent: the link goes with it.
	_, err = p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "ada@new.example.com"})
	require.NoError(t, err)
	orphan := app.outbox.last(t)
	require.NoError(t, app.ac.Store.DeleteUser(ctx, user.ID))
	_, err = p.ResetPassword(ctx, orphan.Token, resetNewPassword)
	assert.True(t, invalidResetToken(err), "a deleted user's link: %v", err)
}

// An expired link is refused like one nobody issued, and the refusal changes
// nothing: the password, the user's sessions and the verified flag stay as
// they were, and no notice goes out. Asking again gives a link that works.
func TestPasswordResetExpiredLink(t *testing.T) {
	ctx := context.Background()
	var reasons []core.FailureReason
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		return reg.OnFailed(emailpassword.HookResetFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			reasons = append(reasons, reason)
			return nil
		}, nil)
	})
	p := app.plugin
	confirm := app.mounted["/api/auth"+emailpassword.PathResetConfirm]
	require.NotNil(t, confirm.Handler)

	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)

	sent, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
	require.NoError(t, err)
	expired := app.outbox.last(t)
	app.expireLink(t, sent.TokenID)

	_, err = p.ResetPassword(ctx, expired.Token, resetNewPassword)
	assert.True(t, invalidResetToken(err), "an expired link: %v", err)

	// The route gives the same answer for the expired link as for a token
	// that was never issued, so a caller can't tell the two apart.
	body := func(token string) string { return `{"token":"` + token + `","password":"` + resetNewPassword + `"}` }
	late := call(t, app.ac, confirm, body(expired.Token))
	unknown := call(t, app.ac, confirm, body("not-a-token"))
	assert.Equal(t, http.StatusUnauthorized, late.Code)
	assert.Equal(t, unknown.Code, late.Code)
	assert.Equal(t, unknown.Body.String(), late.Body.String(), "expired and unknown links look the same")
	assert.Contains(t, late.Body.String(), emailpassword.ErrorCodeInvalidResetToken)

	// All three refusals went through auth.passwordReset.failed. The two for
	// the expired link say why, and the one for the unknown token does not.
	require.Len(t, reasons, 3)
	for i, reason := range reasons[:2] {
		assert.Equal(t, "invalid_token", reason.Code, "refusal %d", i)
		assert.True(t, errs.IsCode(reason.Cause, errs.CodeTokenExpired), "refusal %d: %v", i, reason.Cause)
	}
	assert.Equal(t, "invalid_token", reasons[2].Code)
	assert.False(t, errs.IsCode(reasons[2].Cause, errs.CodeTokenExpired), "an unknown token is not an expired one: %v", reasons[2].Cause)

	_, err = app.signIn(resetNewPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the password was not set: %v", err)
	_, err = app.signIn(resetOldPassword)
	require.NoError(t, err, "the old password still works")
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	require.NoError(t, err, "the user's session is still live")
	stored, err := app.ac.Store.FindUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.False(t, stored.EmailVerified, "an expired link does not verify the address")

	// A new request replaces the expired link, and the new one works.
	fresh := app.link(t)
	assert.NotEqual(t, expired.Token, fresh.Token)
	reset, err := p.ResetPassword(ctx, fresh.Token, resetNewPassword)
	require.NoError(t, err)
	assert.Equal(t, user.ID, reset.ID)
	_, err = app.signIn(resetNewPassword)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return app.outbox.last(t).Kind == core.MailPasswordChanged }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 3, app.outbox.count(), "two links and one notice: the refused attempts sent none")
}

// Two resets with one link: the one that consumes it first sets the password,
// and the other is refused like any link that can't be used and changes
// nothing. The race is made deterministic by a before handler that completes
// a whole reset in the gap between the flow's check of the link and its
// consuming it, so the slower reset has passed every check that comes before
// the link is consumed.
func TestPasswordResetLinkUsedInTheGap(t *testing.T) {
	const (
		firstPassword  = "the first to arrive"
		secondPassword = "the second to arrive"
	)
	ctx := context.Background()
	var (
		app       *resetApp
		user      *models.User
		link      core.MailMessage
		racing    bool
		first     *models.User
		firstErr  error
		failures  []core.FailureReason
		afterRuns int
	)
	app, user = bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(emailpassword.HookResetBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			if racing {
				racing = false // the reset in the gap passes through this handler too
				first, firstErr = app.plugin.ResetPassword(context.Background(), link.Token, firstPassword)
			}
			return payload, nil
		}, nil); err != nil {
			return err
		}
		if err := reg.OnAfter(emailpassword.HookResetAfter, func(*core.HookContext, any) error {
			afterRuns++
			return nil
		}, nil); err != nil {
			return err
		}
		return reg.OnFailed(emailpassword.HookResetFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			failures = append(failures, reason)
			return nil
		}, nil)
	})
	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	link = app.link(t)

	racing = true
	_, err = app.plugin.ResetPassword(ctx, link.Token, secondPassword)
	require.False(t, racing, "the handler ran the first reset in the gap")
	require.NoError(t, firstErr, "the reset that consumed the link first")
	assert.Equal(t, user.ID, first.ID)
	assert.True(t, invalidResetToken(err), "the one that came second: %v", err)

	// The second reset changed nothing: the password is the first one's.
	_, err = app.signIn(secondPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the second password was not set: %v", err)
	_, err = app.signIn(resetOldPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the old password is gone: %v", err)
	_, err = app.signIn(firstPassword)
	require.NoError(t, err, "the first password was set")
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	assert.Error(t, err, "the first reset ended the user's sessions")

	// Only the first reset was a success. The second one failed as a link
	// that is no longer usable, after it had named the user.
	assert.Equal(t, 1, afterRuns)
	require.Len(t, failures, 1)
	assert.Equal(t, "invalid_token", failures[0].Code)
	assert.True(t, errs.IsCode(failures[0].Cause, errs.CodeTokenAlreadyConsumed), "the cause: %v", failures[0].Cause)
	assert.Equal(t, user.ID, failures[0].SubjectID)
	done := app.rec.Audit.OfType(string(emailpassword.HookResetAfter))
	require.Len(t, done, 1)
	assert.Equal(t, user.ID, done[0].SubjectID)
	failed := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.Len(t, failed, 1)
	assert.Equal(t, user.ID, failed[0].SubjectID)

	// The user is told once, of the reset that happened.
	require.Eventually(t, func() bool { return app.outbox.last(t).Kind == core.MailPasswordChanged }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 2, app.outbox.count(), "the link and one notice: the refused reset sent none")
}

// Several resets with one link at the same time, from goroutines: one of them
// succeeds and the others are refused. Whichever one wins, its password is
// the one that works, the user's sessions are ended and the user is told
// once. It checks the outcome and not which interleaving produced it, so it
// holds whether a loser was stopped when the link was checked or when it was
// consumed.
func TestPasswordResetLinkUsedAtTheSameTime(t *testing.T) {
	const attempts = 8
	ctx := context.Background()
	app, user := bootResetWith(t, emailpassword.ResetOptions{WaitForSend: true}, core.TokenConfig{}, resetConcurrentDBParams, nil)
	link := app.link(t)
	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)

	passwords := make([]string, attempts)
	for i := range passwords {
		passwords[i] = fmt.Sprintf("password number %d, chosen alone", i)
	}
	results := make([]error, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = app.plugin.ResetPassword(ctx, link.Token, passwords[i])
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range results {
		if err == nil {
			require.Equal(t, -1, winner, "attempts %d and %d both succeeded", winner, i)
			winner = i
			continue
		}
		assert.True(t, invalidResetToken(err), "attempt %d: %v", i, err)
	}
	require.NotEqual(t, -1, winner, "one of them succeeds")

	for i, password := range passwords {
		_, err := app.signIn(password)
		if i == winner {
			assert.NoError(t, err, "the password of the attempt that won")
			continue
		}
		assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the password of attempt %d was not set: %v", i, err)
	}
	_, err = app.signIn(resetOldPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the old password is gone: %v", err)
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	assert.Error(t, err, "the session from before the reset has ended")

	done := app.rec.Audit.OfType(string(emailpassword.HookResetAfter))
	require.Len(t, done, 1, "one reset happened")
	assert.Equal(t, user.ID, done[0].SubjectID)
	failed := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.Len(t, failed, attempts-1, "every other attempt was refused")
	for _, event := range failed {
		assert.Equal(t, "invalid_token", event.Metadata["code"])
	}
	require.Eventually(t, func() bool { return app.outbox.last(t).Kind == core.MailPasswordChanged }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 2, app.outbox.count(), "the link and one notice: the refused resets sent none")
}

// The user is deleted after the flow has checked the link and before it looks
// the user up. Deleting a user also deletes their links, so a link of a user
// who is gone is usually refused as an invalid token (TestPasswordResetRefusals);
// this is the other case, where the flow holds a link that was valid when it
// was checked. A before handler deletes the user in the gap. The reset is
// refused like any link that can't be used, the reason says the user was
// gone, and nothing is written for the user or sent to them.
func TestPasswordResetUserDeletedInTheGap(t *testing.T) {
	ctx := context.Background()
	var (
		app       *resetApp
		user      *models.User
		deleting  bool
		deleteErr error
		failures  []core.FailureReason
		afterRuns int
	)
	app, user = bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(emailpassword.HookResetBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			if deleting {
				deleting = false
				deleteErr = app.ac.Store.DeleteUser(context.Background(), user.ID)
			}
			return payload, nil
		}, nil); err != nil {
			return err
		}
		if err := reg.OnAfter(emailpassword.HookResetAfter, func(*core.HookContext, any) error {
			afterRuns++
			return nil
		}, nil); err != nil {
			return err
		}
		return reg.OnFailed(emailpassword.HookResetFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			failures = append(failures, reason)
			return nil
		}, nil)
	})
	link := app.link(t)

	deleting = true
	_, err := app.plugin.ResetPassword(ctx, link.Token, resetNewPassword)
	require.False(t, deleting, "the handler deleted the user in the gap")
	require.NoError(t, deleteErr)
	assert.True(t, invalidResetToken(err), "the answer is the one for any unusable link: %v", err)

	// The failure says why, and names the user who was gone.
	require.Len(t, failures, 1)
	assert.Equal(t, "user_not_found", failures[0].Code, "not invalid_token: the link was valid when it was checked")
	assert.NoError(t, failures[0].Cause)
	assert.Equal(t, models.UserTable, failures[0].SubjectType)
	assert.Equal(t, user.ID, failures[0].SubjectID)
	failed := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.Len(t, failed, 1)
	assert.Equal(t, "user_not_found", failed[0].Metadata["code"])
	assert.Equal(t, user.ID, failed[0].SubjectID)

	// Nothing was written for the user who is gone.
	_, err = app.ac.Store.FindUserByID(ctx, user.ID)
	assert.True(t, errs.IsNotFound(err), "the user is still gone: %v", err)
	_, err = app.ac.Store.FindAccount(ctx, models.ProviderCredential, user.ID)
	assert.True(t, errs.IsNotFound(err), "no credential account was created for them: %v", err)
	_, err = app.signIn(resetNewPassword)
	assert.Error(t, err, "the password was not set")

	// The reset did not happen, so nobody is told it did. Closing the mailer
	// waits for the notices that are queued, so none is still on its way.
	require.NoError(t, app.ac.Mailer.Close(ctx))
	assert.Zero(t, afterRuns, "auth.passwordReset.after did not fire")
	assert.Equal(t, 1, app.outbox.count(), "only the link: the refused reset sent no notice")
}

// The code of the typed error that the handlers of the tests below refuse a
// reset with. A handler that means to refuse a request returns a typed error:
// core takes any other error as a failure of the system.
const resetRefusedCode = "blocked_by_policy"

func resetRefusal() error {
	return errs.NewForbidden("test.hook", resetRefusedCode, "password resets are closed")
}

// A before handler that returns a typed error refuses a request for a link.
// The caller gets the handler's own error, for an email with an account and
// for one without alike, so the refusal does not tell which have one. It
// comes before the flow does anything: no link is sent and the user's
// earlier link is not replaced. The failed point fires with
// core.FailureRejectedByHook and the handler's error as the cause.
func TestPasswordResetRequestRefusedByAHook(t *testing.T) {
	ctx := context.Background()
	var refusing bool
	var failures []core.FailureReason
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(emailpassword.HookResetRequestBefore, func(*core.HookContext, behemoth.M) (behemoth.M, error) {
			if refusing {
				return nil, resetRefusal()
			}
			return nil, nil
		}, nil); err != nil {
			return err
		}
		return reg.OnFailed(emailpassword.HookResetRequestFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			failures = append(failures, reason)
			return nil
		}, nil)
	})
	p := app.plugin
	earlier := app.link(t)

	refusing = true
	for _, email := range []string{resetEmail, "nobody@example.com"} {
		_, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: email})
		assert.True(t, errs.IsCode(err, resetRefusedCode), "%s: the handler's own error: %v", email, err)
	}
	assert.Equal(t, 1, app.outbox.count(), "no link was sent: there is only the earlier one")

	// The route reports it, as it does a validation error, and in the same
	// way for both emails.
	request := app.mounted["/api/auth"+emailpassword.PathResetRequest]
	require.NotNil(t, request.Handler)
	known := call(t, app.ac, request, `{"email":"ada@example.com"}`)
	unknown := call(t, app.ac, request, `{"email":"nobody@example.com"}`)
	assert.Equal(t, http.StatusForbidden, known.Code, known.Body.String())
	assert.Equal(t, known.Code, unknown.Code)
	assert.Equal(t, known.Body.String(), unknown.Body.String(), "the refusal does not tell which emails have an account")
	assert.Contains(t, known.Body.String(), resetRefusedCode)
	assert.Equal(t, 1, app.outbox.count())

	// Four refusals, all of the same kind. None is "user_not_found": the
	// handler refused before the user was looked up.
	require.Len(t, failures, 4)
	for i, reason := range failures {
		assert.Equal(t, core.FailureRejectedByHook, reason.Code, "refusal %d", i)
		assert.True(t, errs.IsCode(reason.Cause, resetRefusedCode), "refusal %d: the cause: %v", i, reason.Cause)
	}
	events := app.rec.Audit.OfType(string(emailpassword.HookResetRequestFailed))
	require.Len(t, events, 4)
	for i, event := range events {
		assert.Equal(t, core.FailureRejectedByHook, event.Metadata["code"], "event %d", i)
		assert.Equal(t, resetRefusedCode, event.Metadata["causeCode"], "event %d: the handler's own code", i)
	}

	// The earlier link was not revoked by the refused requests.
	refusing = false
	_, err := p.ResetPassword(ctx, earlier.Token, resetNewPassword)
	require.NoError(t, err, "the user's earlier link still works")
}

// A before handler that returns a typed error refuses an attempt to use a
// link. The caller gets the handler's own error and not the answer for an
// unusable link, nothing changes, and the link is not used up: it works once
// the handler lets the reset through.
func TestPasswordResetRefusedByAHook(t *testing.T) {
	ctx := context.Background()
	var refusing bool
	var failures []core.FailureReason
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(emailpassword.HookResetBefore, func(*core.HookContext, behemoth.M) (behemoth.M, error) {
			if refusing {
				return nil, resetRefusal()
			}
			return nil, nil
		}, nil); err != nil {
			return err
		}
		return reg.OnFailed(emailpassword.HookResetFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			failures = append(failures, reason)
			return nil
		}, nil)
	})
	p := app.plugin
	confirm := app.mounted["/api/auth"+emailpassword.PathResetConfirm]
	require.NotNil(t, confirm.Handler)
	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	link := app.link(t)

	refusing = true
	_, err = p.ResetPassword(ctx, link.Token, resetNewPassword)
	assert.True(t, errs.IsCode(err, resetRefusedCode), "the handler's own error, not the answer for an unusable link: %v", err)
	w := call(t, app.ac, confirm, `{"token":"`+link.Token+`","password":"`+resetNewPassword+`"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), resetRefusedCode)

	// Nothing changed.
	_, err = app.signIn(resetNewPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the password was not set: %v", err)
	_, err = app.signIn(resetOldPassword)
	require.NoError(t, err, "the old password still works")
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	require.NoError(t, err, "the user's session is still live")
	stored, err := app.ac.Store.FindUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.False(t, stored.EmailVerified)

	require.Len(t, failures, 2)
	for i, reason := range failures {
		assert.Equal(t, core.FailureRejectedByHook, reason.Code, "refusal %d", i)
		assert.True(t, errs.IsCode(reason.Cause, resetRefusedCode), "refusal %d: the cause: %v", i, reason.Cause)
	}
	events := app.rec.Audit.OfType(string(emailpassword.HookResetFailed))
	require.Len(t, events, 2)
	for i, event := range events {
		assert.Equal(t, core.FailureRejectedByHook, event.Metadata["code"], "event %d", i)
		assert.Equal(t, resetRefusedCode, event.Metadata["causeCode"], "event %d: the handler's own code", i)
	}

	// The link was not used up by the refused attempts.
	refusing = false
	reset, err := p.ResetPassword(ctx, link.Token, resetNewPassword)
	require.NoError(t, err, "the link works once the handler lets the reset through")
	assert.Equal(t, user.ID, reset.ID)
	require.Eventually(t, func() bool { return app.outbox.last(t).Kind == core.MailPasswordChanged }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 2, app.outbox.count(), "the link and one notice: the refused attempts sent none")
}

// A before handler that fails for a reason of the system, an error that is
// not a typed one, stops the flow with its error and fires no failed point:
// nothing refused the caller. The reset flows' before points behave like the
// sign-in's (TestHandlerSystemErrorFiresNoFailedPoint). The link is neither
// replaced nor used up.
func TestPasswordResetHandlerSystemErrorFiresNoFailedPoint(t *testing.T) {
	ctx := context.Background()
	errDown := errors.New("lookup: connection refused")
	var down bool
	var failures []core.FailureReason
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestBefore, emailpassword.HookResetBefore} {
			if err := reg.OnBefore(point, func(*core.HookContext, behemoth.M) (behemoth.M, error) {
				if down {
					return nil, errDown
				}
				return nil, nil
			}, nil); err != nil {
				return err
			}
		}
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestFailed, emailpassword.HookResetFailed} {
			if err := reg.OnFailed(point, func(_ *core.HookContext, reason core.FailureReason) error {
				failures = append(failures, reason)
				return nil
			}, nil); err != nil {
				return err
			}
		}
		return nil
	})
	p := app.plugin
	link := app.link(t)

	down = true
	_, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
	require.ErrorIs(t, err, errDown)
	_, err = p.ResetPassword(ctx, link.Token, resetNewPassword)
	require.ErrorIs(t, err, errDown)

	assert.Empty(t, failures, "no failed handler ran")
	assert.Empty(t, app.rec.Audit.OfType(string(emailpassword.HookResetRequestFailed)))
	assert.Empty(t, app.rec.Audit.OfType(string(emailpassword.HookResetFailed)))
	assert.Equal(t, 1, app.outbox.count(), "no new link was sent")

	down = false
	_, err = p.ResetPassword(ctx, link.Token, resetNewPassword)
	require.NoError(t, err, "the link was neither replaced nor used up")
}

// Handlers on the failed and after points observe, and one that returns an
// error is logged and changes nothing: a refusal keeps its answer and its
// audit event, and a request or a reset that happened still stands.
func TestPasswordResetObserverErrorsChangeNothing(t *testing.T) {
	ctx := context.Background()
	errObserver := errors.New("the audit pipeline is down")
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, func(reg core.HookRegistry) error {
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestFailed, emailpassword.HookResetFailed} {
			if err := reg.OnFailed(point, func(*core.HookContext, core.FailureReason) error { return errObserver }, nil); err != nil {
				return err
			}
		}
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestAfter, emailpassword.HookResetAfter} {
			if err := reg.OnAfter(point, func(*core.HookContext, any) error { return errObserver }, nil); err != nil {
				return err
			}
		}
		return nil
	})
	p := app.plugin

	// A refusal keeps its answer, and is still audited.
	_, err := p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "nobody@example.com"})
	assert.ErrorIs(t, err, emailpassword.ErrNoAccount)
	_, err = p.ResetPassword(ctx, "not-a-token", resetNewPassword)
	assert.True(t, invalidResetToken(err), "%v", err)
	assert.Len(t, app.rec.Audit.OfType(string(emailpassword.HookResetRequestFailed)), 1)
	assert.Len(t, app.rec.Audit.OfType(string(emailpassword.HookResetFailed)), 1)

	// What happened stands: the link was sent, and it set the password.
	_, err = p.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
	require.NoError(t, err, "a failing after handler does not fail the request")
	require.Equal(t, 1, app.outbox.count())
	reset, err := p.ResetPassword(ctx, app.outbox.last(t).Token, resetNewPassword)
	require.NoError(t, err, "a failing after handler does not fail the reset")
	assert.Equal(t, user.ID, reset.ID)
	_, err = app.signIn(resetNewPassword)
	require.NoError(t, err, "the password was set")
	assert.Len(t, app.rec.Audit.OfType(string(emailpassword.HookResetRequestAfter)), 1)
	assert.Len(t, app.rec.Audit.OfType(string(emailpassword.HookResetAfter)), 1)

	// Each failing handler was reported, in the log.
	logged := func(message string) (n int) {
		for _, entry := range app.rec.Logger.At(slog.LevelError) {
			if entry.Message == message {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 2, logged("failed-hook handler errored"), "one for each refusal")
	assert.Equal(t, 2, logged("after-hook failed"), "one for the request and one for the reset")
}

// A link is valid for ResetOptions.TTL, and TokenConfig.TTLOverrides for the
// password reset kind takes precedence over it, shorter or longer. The expiry
// is checked on the stored link, which is the one the token manager enforces,
// and on the result the caller gets.
func TestPasswordResetLinkLifetime(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		opts      emailpassword.ResetOptions
		overrides map[models.TokenKind]time.Duration
		want      time.Duration
	}{
		"the option":                    {emailpassword.ResetOptions{TTL: 15 * time.Minute}, nil, 15 * time.Minute},
		"an override beats the option":  {emailpassword.ResetOptions{TTL: 15 * time.Minute}, map[models.TokenKind]time.Duration{core.TokenKindPasswordReset: 5 * time.Minute}, 5 * time.Minute},
		"an override beats the default": {emailpassword.ResetOptions{}, map[models.TokenKind]time.Duration{core.TokenKindPasswordReset: 3 * time.Hour}, 3 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.WaitForSend = true
			app, _ := bootResetWith(t, opts, core.TokenConfig{TTLOverrides: tc.overrides}, resetDBParams, nil)

			sent, err := app.plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
			require.NoError(t, err)
			stored, err := app.ac.Store.FindTokenByID(ctx, sent.TokenID)
			require.NoError(t, err)

			// The tolerance is far smaller than the gap between any two cases.
			expiry := time.Now().Add(tc.want)
			assert.WithinDuration(t, expiry, sent.ExpiresAt, 10*time.Second, "the result")
			assert.WithinDuration(t, expiry, app.outbox.last(t).ExpiresAt, 10*time.Second, "the message")
			assert.WithinDuration(t, expiry, stored.ExpiresAt, 10*time.Second, "the stored link")
		})
	}
}

// A user who never had a password gets one from a reset, and with
// LeaveEmailUnverified the reset does not touch the verified flag.
func TestPasswordResetWithoutAPasswordAndLeavingEmailUnverified(t *testing.T) {
	ctx := context.Background()
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true, LeaveEmailUnverified: true}, nil)

	grace := &models.User{Email: "grace@example.com"}
	require.NoError(t, app.ac.Store.CreateUser(ctx, grace))
	_, err := app.plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: grace.Email})
	require.NoError(t, err)

	reset, err := app.plugin.ResetPassword(ctx, app.outbox.last(t).Token, resetNewPassword)
	require.NoError(t, err)
	assert.False(t, reset.EmailVerified, "the option leaves the flag alone")
	stored, err := app.ac.Store.FindUserByID(ctx, grace.ID)
	require.NoError(t, err)
	assert.False(t, stored.EmailVerified)

	result, err := app.plugin.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: grace.Email, Password: resetNewPassword})
	require.NoError(t, err, "the reset created the credential account")
	assert.Equal(t, grace.ID, result.User.ID)
}

// Reset is off without a link URL: no routes, no mail sender needed. Turned
// on, it needs a sender and an absolute URL, and a missing one stops Boot.
func TestPasswordResetOptions(t *testing.T) {
	boot := func(t *testing.T, opts emailpassword.ResetOptions, sender core.MailSender) (*emailpassword.Plugin, mountedRoutes, error) {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "reset-opts.db")+"?_foreign_keys=on")
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		_, err = db.Exec(magicLinkSchema)
		require.NoError(t, err)
		p := emailpassword.New(emailpassword.Options{Reset: opts})
		mounted := mountedRoutes{}
		prepared, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
		require.NoError(t, err)
		_, err = engine.Boot(context.Background(), prepared, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
			Crypto: crypto.Config{
				Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
			},
			Mail: core.MailConfig{Sender: sender},
			HTTP: mounted,
		})
		return p, mounted, err
	}
	sender := core.MailSenderFunc(func(context.Context, core.MailMessage) error { return nil })

	t.Run("off by default", func(t *testing.T) {
		p, mounted, err := boot(t, emailpassword.ResetOptions{}, nil)
		require.NoError(t, err, "no sender is needed")
		assert.NotContains(t, mounted, "/api/auth"+emailpassword.PathResetRequest)
		assert.NotContains(t, mounted, "/api/auth"+emailpassword.PathResetConfirm)
		_, err = p.RequestPasswordReset(context.Background(), emailpassword.ResetRequest{Email: resetEmail})
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
		_, err = p.ResetPassword(context.Background(), "token", resetNewPassword)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	})
	for name, tc := range map[string]struct {
		opts   emailpassword.ResetOptions
		sender core.MailSender
	}{
		"no sender":           {emailpassword.ResetOptions{LinkURL: "https://app.example.com/auth/reset"}, nil},
		"a relative link URL": {emailpassword.ResetOptions{LinkURL: "/auth/reset"}, sender},
		"a negative TTL":      {emailpassword.ResetOptions{LinkURL: "https://app.example.com/auth/reset", TTL: -time.Minute}, sender},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := boot(t, tc.opts, tc.sender)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "emailpassword")
		})
	}
}

// By default the request does not wait for the sender: it returns while the
// sender is still blocked, and a failed send leaves the link valid.
func TestPasswordResetSendsInBackground(t *testing.T) {
	ctx := context.Background()
	app, user := bootReset(t, emailpassword.ResetOptions{}, nil)
	app.outbox.err = errors.New("smtp down")

	app.outbox.mu.Lock() // the sender blocks until the request has returned
	_, err := app.plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
	app.outbox.mu.Unlock()
	require.NoError(t, err, "the request did not wait for the sender, and does not see its error")

	require.Eventually(t, func() bool { return app.outbox.count() == 1 }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(app.rec.Logger.At(slog.LevelError)) == 1 }, 5*time.Second, time.Millisecond,
		"the mailer logs the failed send")
	reset, err := app.plugin.ResetPassword(ctx, app.outbox.last(t).Token, resetNewPassword)
	require.NoError(t, err, "the link was not revoked: nobody was waiting for the send")
	assert.Equal(t, user.ID, reset.ID)
}

// A reset ends every session of the user, a pending one that waits for a
// second factor included, and records why. It starts none: the user signs in
// again. The sessions of other users are not touched, and an attempt that
// does not set the password ends nothing.
func TestPasswordResetEndsTheUsersSessions(t *testing.T) {
	ctx := context.Background()
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
	p := app.plugin
	grace, err := p.SignUp(ctx, behemoth.M{"email": "grace@example.com", "password": resetOldPassword})
	require.NoError(t, err)
	graceIn, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: grace.Email, Password: resetOldPassword})
	require.NoError(t, err)

	first, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	second, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	pending, _, err := app.ac.SessionManager.Create(ctx, user.ID, core.SessionMeta{State: models.SessionPending, Factor: core.FactorPassword})
	require.NoError(t, err)
	sessions := map[string]*models.Session{"first": first.Session, "second": second.Session, "pending": pending}
	liveOf := func(userID string) int {
		t.Helper()
		live, err := app.ac.Store.ListLiveSessions(ctx, userID, "", 0)
		require.NoError(t, err)
		return len(live)
	}
	link := app.link(t)

	// A password the rules refuse and a token nobody issued end nothing.
	_, err = p.ResetPassword(ctx, link.Token, "short")
	require.Error(t, err)
	_, err = p.ResetPassword(ctx, "not-a-token", resetNewPassword)
	require.Error(t, err)
	require.Equal(t, 3, liveOf(user.ID), "the three sessions are live")

	_, err = p.ResetPassword(ctx, link.Token, resetNewPassword)
	require.NoError(t, err)

	// Each session was ended, with the reason a client can read in the row.
	assert.Equal(t, "password_reset", emailpassword.SessionRevokedPasswordReset, "the value stored with each ended session")
	for name, session := range sessions {
		ended, err := app.ac.Store.FindSessionByID(ctx, session.ID)
		require.NoError(t, err, name)
		assert.NotNil(t, ended.RevokedAt, "the %s session", name)
		assert.Equal(t, emailpassword.SessionRevokedPasswordReset, ended.RevokedReason, "the %s session", name)
	}
	assert.Zero(t, liveOf(user.ID), "the reset did not sign the user in")

	// Another user's session was left alone.
	_, err = app.ac.SessionManager.Validate(ctx, graceIn.RawToken)
	require.NoError(t, err, "Grace's session is still live")
	assert.Equal(t, 1, liveOf(grace.ID))
}

// A new password has to follow the rules of the plugin, the ones sign-up
// follows: a length of Options.MinPasswordLength to MaxPasswordLength
// characters, and then Options.ValidatePassword. Length counts characters
// and not bytes, and the validator is asked only about a password whose
// length is allowed. A refused password uses up no link: the link of the
// refusals is the one the next password is set with.
func TestPasswordResetAppliesThePasswordRules(t *testing.T) {
	ctx := context.Background()
	reset := emailpassword.ResetOptions{WaitForSend: true, RequestLimit: core.Limit{Max: 100, Window: time.Hour}}

	refuseAll := func(t *testing.T, app *resetApp, link core.MailMessage, refused map[string]string) {
		t.Helper()
		for name, password := range refused {
			_, err := app.plugin.ResetPassword(ctx, link.Token, password)
			assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "refused, %s: %v", name, err)
		}
	}
	// acceptAll sets each password and signs in with it. The first uses link,
	// which the refusals did not use up, and the others a link of their own.
	acceptAll := func(t *testing.T, app *resetApp, link core.MailMessage, accepted map[string]string) {
		t.Helper()
		for name, password := range accepted {
			_, err := app.plugin.ResetPassword(ctx, link.Token, password)
			require.NoError(t, err, "accepted, %s", name)
			_, err = app.signIn(password)
			require.NoError(t, err, "accepted, %s: it signs in", name)
			link = app.link(t)
		}
	}

	t.Run("the default limits", func(t *testing.T) {
		app, _ := bootReset(t, reset, nil)
		link := app.link(t)
		refuseAll(t, app, link, map[string]string{
			"one under the minimum": strings.Repeat("x", emailpassword.DefaultMinPasswordLength-1),
			"one over the maximum":  strings.Repeat("x", emailpassword.DefaultMaxPasswordLength+1),
		})
		acceptAll(t, app, link, map[string]string{
			"the minimum": strings.Repeat("x", emailpassword.DefaultMinPasswordLength),
			"the maximum": strings.Repeat("x", emailpassword.DefaultMaxPasswordLength),
		})
	})

	t.Run("limits and a validator of the application", func(t *testing.T) {
		errWeak := errors.New("found in the list of leaked passwords")
		const leaked = "my Password is long"
		var seen []string
		app, user := bootResetPlugin(t, emailpassword.Options{
			MinPasswordLength: 10,
			MaxPasswordLength: 24,
			ValidatePassword: func(password string) error {
				seen = append(seen, password)
				if strings.Contains(strings.ToLower(password), "password") {
					return errWeak
				}
				return nil
			},
			Reset: reset,
		}, core.TokenConfig{}, resetDBParams, nil, nil)
		seen = nil // Ada's sign-up went through it
		refused := map[string]string{
			"one under the minimum":                       strings.Repeat("x", 9),
			"one over the maximum":                        strings.Repeat("x", 25),
			"too few characters, but more bytes than min": strings.Repeat("é", 9),
			"too many characters":                         strings.Repeat("é", 25),
			"the validator refuses it":                    leaked,
		}
		accepted := map[string]string{
			"the minimum":                         strings.Repeat("x", 10),
			"the minimum, in two-byte characters": strings.Repeat("é", 10),
			"the maximum":                         strings.Repeat("x", 24),
			"the maximum, in two-byte characters": strings.Repeat("é", 24),
		}
		link := app.link(t)

		refuseAll(t, app, link, refused)
		assert.Equal(t, []string{leaked}, seen, "the validator was asked only about the password whose length was allowed")
		_, err := app.plugin.ResetPassword(ctx, link.Token, leaked)
		assert.ErrorIs(t, err, errWeak, "the validator's error is the cause")

		// The route answers 400 and does not repeat what the validator said.
		confirm := app.mounted["/api/auth"+emailpassword.PathResetConfirm]
		require.NotNil(t, confirm.Handler)
		w := call(t, app.ac, confirm, `{"token":"`+link.Token+`","password":"`+leaked+`"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), emailpassword.ErrorCodeInvalidPassword)
		assert.NotContains(t, w.Body.String(), errWeak.Error())

		// ValidatePassword, SetPassword and sign-up give a password the
		// answer the reset did.
		signUps := 0
		for name, password := range refused {
			err := app.plugin.ValidatePassword(password)
			assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "ValidatePassword, %s: %v", name, err)
			_, err = app.plugin.SetPassword(asAdmin(ctx), user.ID, password)
			assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "SetPassword, %s: %v", name, err)
			signUps++
			_, err = app.plugin.SignUp(ctx, behemoth.M{"email": fmt.Sprintf("new%d@example.com", signUps), "password": password})
			assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "SignUp, %s: %v", name, err)
		}
		for name, password := range accepted {
			assert.NoError(t, app.plugin.ValidatePassword(password), "ValidatePassword, %s", name)
		}

		seen = nil
		acceptAll(t, app, link, accepted)
		assert.ElementsMatch(t, slices.Collect(maps.Values(accepted)), seen,
			"the validator saw each password that was set, as it was given")
	})
}

// The password is checked before the link, so a password the rules refuse
// gets the same answer for a link that works and for a token nobody issued:
// the answer says nothing about the token. With a good password the two
// differ, and the refusals did not use the link up.
func TestPasswordResetRefusesAPasswordBeforeLookingAtTheLink(t *testing.T) {
	ctx := context.Background()
	app, _ := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
	p := app.plugin
	confirm := app.mounted["/api/auth"+emailpassword.PathResetConfirm]
	require.NotNil(t, confirm.Handler)
	link := app.link(t)

	_, withLink := p.ResetPassword(ctx, link.Token, "short")
	_, withNothing := p.ResetPassword(ctx, "not-a-token", "short")
	assert.True(t, errs.IsCode(withLink, emailpassword.ErrorCodeInvalidPassword), "%v", withLink)
	assert.True(t, errs.IsCode(withNothing, emailpassword.ErrorCodeInvalidPassword), "%v", withNothing)

	good := call(t, app.ac, confirm, `{"token":"`+link.Token+`","password":"short"}`)
	unknown := call(t, app.ac, confirm, `{"token":"not-a-token","password":"short"}`)
	assert.Equal(t, http.StatusBadRequest, good.Code, good.Body.String())
	assert.Equal(t, good.Code, unknown.Code)
	assert.Equal(t, good.Body.String(), unknown.Body.String(), "the response does not tell whether the token is good")

	_, err := p.ResetPassword(ctx, "not-a-token", resetNewPassword)
	assert.True(t, invalidResetToken(err), "with a good password the unknown token is refused as such: %v", err)
	_, err = p.ResetPassword(ctx, link.Token, resetNewPassword)
	require.NoError(t, err, "the refusals did not use the link up")
}

// failingRevokes is a Database whose update of a single token, which is how
// a link is revoked, fails while on is set.
type failingRevokes struct {
	behemoth.Database
	on *atomic.Bool
}

func (d failingRevokes) UpdateOne(ctx context.Context, m behemoth.Model, expr clause.Expression, updates behemoth.M) error {
	if _, isToken := m.(*models.Token); isToken && d.on.Load() {
		return errs.NewDatabaseError("failingRevokes", errors.New("connection reset by peer"))
	}
	return d.Database.UpdateOne(ctx, m, expr, updates)
}

// warned returns the warnings the app logged with message.
func (app *resetApp) warned(message string) (found []telemetrytest.LogEntry) {
	for _, entry := range app.rec.Logger.At(slog.LevelWarn) {
		if entry.Message == message {
			found = append(found, entry)
		}
	}
	return found
}

// A link that could not be sent is revoked, and the caller is told why the
// send failed. When the revoke fails too, the link stays valid with nobody
// holding it. The caller can't be told that and still gets the send's
// failure, so the warning in the log is the only trace of it.
func TestPasswordResetLinkThatCouldNotBeSentOrRevoked(t *testing.T) {
	ctx := context.Background()
	const warning = "the reset link that could not be sent was not revoked"
	errSMTP := errors.New("smtp down")
	var revokeFails atomic.Bool
	var failures []core.FailureReason
	app, user := bootResetPlugin(t, emailpassword.Options{Reset: emailpassword.ResetOptions{WaitForSend: true}}, core.TokenConfig{}, resetDBParams,
		func(db behemoth.Database) behemoth.Database { return failingRevokes{Database: db, on: &revokeFails} },
		func(reg core.HookRegistry) error {
			return reg.OnFailed(emailpassword.HookResetRequestFailed, func(_ *core.HookContext, reason core.FailureReason) error {
				failures = append(failures, reason)
				return nil
			}, nil)
		})
	app.outbox.err = errSMTP
	request := func() error {
		_, err := app.plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
		return err
	}
	linkWorks := func(msg core.MailMessage) error {
		_, err := app.ac.TokenManager.Verify(ctx, core.TokenKindPasswordReset, msg.Token)
		return err
	}

	// The revoke works: the link that was not sent can't be used, and nothing is logged.
	err := request()
	require.Error(t, err)
	assert.ErrorIs(t, err, errSMTP, "the caller gets the send's failure")
	assert.Contains(t, err.Error(), errSMTP.Error(), "and its text")
	assert.True(t, errs.IsCategory(linkWorks(app.outbox.last(t)), errs.CategoryToken), "the link that was not sent is revoked")
	assert.Empty(t, app.warned(warning))

	// The revoke fails too: the link is still valid, and a warning says so.
	revokeFails.Store(true)
	err = request()
	assert.ErrorIs(t, err, errSMTP, "the caller still gets the send's failure, not the revoke's")
	assert.NoError(t, linkWorks(app.outbox.last(t)), "the link could not be revoked, so it is still valid")
	warnings := app.warned(warning)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Fields[telemetry.FieldError], "connection reset by peer", "the line says why")

	// Both requests failed as a send, whatever became of the revoke.
	require.Len(t, failures, 2)
	for i, reason := range failures {
		assert.Equal(t, "send_failed", reason.Code, "request %d", i)
		assert.ErrorIs(t, reason.Cause, errSMTP, "request %d", i)
		assert.Equal(t, user.ID, reason.SubjectID, "request %d", i)
	}
}

// The notice that tells a user their password changed is a courtesy. When it
// can't be queued, here because the mailer was closed, a reset still stands,
// its caller is not told and the user's sessions are ended; a warning in the
// log is the only trace. SetPassword ends the same way.
func TestPasswordChangedNoticeThatCouldNotBeQueued(t *testing.T) {
	ctx := context.Background()
	const warning = "the password changed notice was not queued"
	app, user := bootReset(t, emailpassword.ResetOptions{WaitForSend: true}, nil)
	link := app.link(t)
	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	// A closed mailer takes no more background messages and still sends the
	// ones a caller waits for, such as the link.
	require.NoError(t, app.ac.Mailer.Close(ctx))
	require.Empty(t, app.warned(warning))

	reset, err := app.plugin.ResetPassword(ctx, link.Token, resetNewPassword)
	require.NoError(t, err, "the reset stands")
	assert.Equal(t, user.ID, reset.ID)
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	assert.Error(t, err, "the user's sessions were ended")
	_, err = app.signIn(resetNewPassword)
	require.NoError(t, err, "the new password signs in")
	assert.Len(t, app.rec.Audit.OfType(string(emailpassword.HookResetAfter)), 1)
	warnings := app.warned(warning)
	require.Len(t, warnings, 1)
	assert.NotEmpty(t, warnings[0].Fields[telemetry.FieldError], "the line says why")
	assert.Equal(t, 1, app.outbox.count(), "only the link: no notice was sent")

	_, err = app.plugin.SetPassword(asAdmin(ctx), user.ID, "and another one")
	require.NoError(t, err, "SetPassword stands too")
	assert.Len(t, app.warned(warning), 2)
	assert.Equal(t, 1, app.outbox.count())
}

// Password reset can't run without a token manager: Init says so with a
// configuration error, where the same AuthContext with its token manager is
// accepted. A plugin that was never booted refuses both flows in the same
// way.
func TestPasswordResetNeedsATokenManagerAndABootedPlugin(t *testing.T) {
	ctx := context.Background()
	app, _ := bootReset(t, emailpassword.ResetOptions{}, nil)
	opts := emailpassword.Options{Reset: emailpassword.ResetOptions{LinkURL: "https://app.example.com/auth/reset"}}

	require.NoError(t, emailpassword.New(opts).Init(app.ac), "the booted AuthContext is accepted")
	withoutTokens := *app.ac
	withoutTokens.TokenManager = nil
	err := emailpassword.New(opts).Init(&withoutTokens)
	require.Error(t, err, "an AuthContext without a token manager is refused")
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	assert.Contains(t, err.Error(), "token manager")

	idle := emailpassword.New(opts)
	_, err = idle.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: resetEmail})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	_, err = idle.ResetPassword(ctx, "a-token", resetNewPassword)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
}

// SetPassword and SetPasswordUnchecked set a password without a link. Who
// may call them is checked in TestAdminOperationsRequireAPrivilegedCaller,
// and the tests below are about what they do.

// A system caller sets a user's password. Unlike a reset it leaves the
// verified flag alone and fires none of the reset's points. Like a reset it
// ends every session of the user, with the reason, and starts none, and it
// tells the user in the background by a message without a link.
func TestSetPasswordEndsSessionsAndTellsTheUser(t *testing.T) {
	ctx := context.Background()
	var fired []core.HookPoint
	app, user := bootReset(t, emailpassword.ResetOptions{}, func(reg core.HookRegistry) error {
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestBefore, emailpassword.HookResetBefore} {
			if err := reg.OnBefore(point, func(hctx *core.HookContext, _ behemoth.M) (behemoth.M, error) {
				fired = append(fired, hctx.Point)
				return nil, nil
			}, nil); err != nil {
				return err
			}
		}
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestAfter, emailpassword.HookResetAfter} {
			if err := reg.OnAfter(point, func(hctx *core.HookContext, _ any) error {
				fired = append(fired, hctx.Point)
				return nil
			}, nil); err != nil {
				return err
			}
		}
		for _, point := range []core.HookPoint{emailpassword.HookResetRequestFailed, emailpassword.HookResetFailed} {
			if err := reg.OnFailed(point, func(hctx *core.HookContext, _ core.FailureReason) error {
				fired = append(fired, hctx.Point)
				return nil
			}, nil); err != nil {
				return err
			}
		}
		return nil
	})
	first, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	second, err := app.signIn(resetOldPassword)
	require.NoError(t, err)

	updated, err := app.plugin.SetPassword(asAdmin(ctx), user.ID, resetNewPassword)
	require.NoError(t, err)
	assert.Equal(t, user.ID, updated.ID)
	assert.False(t, updated.EmailVerified, "unlike a reset, it does not verify the address")
	stored, err := app.ac.Store.FindUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.False(t, stored.EmailVerified)

	// Both sessions were ended with the reason, and none was started.
	for i, session := range []*models.Session{first.Session, second.Session} {
		ended, err := app.ac.Store.FindSessionByID(ctx, session.ID)
		require.NoError(t, err)
		assert.NotNil(t, ended.RevokedAt, "session %d", i)
		assert.Equal(t, emailpassword.SessionRevokedPasswordReset, ended.RevokedReason, "session %d", i)
	}
	live, err := app.ac.Store.ListLiveSessions(ctx, user.ID, "", 0)
	require.NoError(t, err)
	assert.Empty(t, live, "no session was started for the user")

	_, err = app.signIn(resetOldPassword)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the old password: %v", err)
	_, err = app.signIn(resetNewPassword)
	require.NoError(t, err, "the new password signs in")

	// The only message is the notice: no link was asked for.
	require.Eventually(t, func() bool { return app.outbox.count() == 1 }, 5*time.Second, time.Millisecond)
	notice := app.outbox.last(t)
	assert.Equal(t, core.MailPasswordChanged, notice.Kind)
	assert.Equal(t, resetEmail, notice.To)
	assert.Equal(t, user.ID, notice.User.ID)
	assert.Empty(t, notice.Token)
	assert.Empty(t, notice.URL)

	assert.Empty(t, fired, "none of the reset's points fired")
	for _, point := range []core.HookPoint{emailpassword.HookResetRequestAfter, emailpassword.HookResetAfter, emailpassword.HookResetRequestFailed, emailpassword.HookResetFailed} {
		assert.Empty(t, app.rec.Audit.OfType(string(point)), "no audit event for %s", point)
	}
}

// A refused call changes nothing. Of the callers, only a system caller may
// set a password: an anonymous one, a signed-in user, even for their own
// account, and a job acting for the user are all refused. A password the
// rules refuse and a user who does not exist are refused for the unchecked
// step as well. A plugin that was not booted refuses both.
func TestSetPasswordRefusals(t *testing.T) {
	ctx := context.Background()
	app, user := bootReset(t, emailpassword.ResetOptions{}, nil)
	p := app.plugin
	session, err := app.signIn(resetOldPassword)
	require.NoError(t, err)
	const attempt = "a password that must not stick"

	for name, tc := range map[string]struct {
		caller context.Context
		code   string
	}{
		"an anonymous caller":       {ctx, errs.CodeSessionMissing},
		"the user, signed in":       {presenting(ctx, app.ac, session.RawToken), errs.CodeInsufficientScope},
		"a job acting for the user": {actingFor(ctx, user.ID), errs.CodeInsufficientScope},
		"a job acting for somebody": {actingFor(ctx, "somebody-else"), errs.CodeInsufficientScope},
	} {
		_, err := p.SetPassword(tc.caller, user.ID, attempt)
		assert.True(t, errs.IsCode(err, tc.code), "%s: %v", name, err)
	}

	_, err = p.SetPassword(asAdmin(ctx), user.ID, "short")
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "a password the rules refuse: %v", err)
	_, err = p.SetPasswordUnchecked(ctx, user.ID, "short")
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidPassword), "the unchecked step too: %v", err)
	_, err = p.SetPassword(asAdmin(ctx), "no-such-user", resetNewPassword)
	assert.True(t, errs.IsNotFound(err), "a user who does not exist: %v", err)
	_, err = p.SetPasswordUnchecked(ctx, "no-such-user", resetNewPassword)
	assert.True(t, errs.IsNotFound(err), "the unchecked step too: %v", err)

	// Nothing changed: the session is live, the old password is the one, and
	// nobody was told.
	_, err = app.ac.SessionManager.Validate(ctx, session.RawToken)
	require.NoError(t, err, "no refusal ended the user's session")
	for _, wrong := range []string{attempt, "short", resetNewPassword} {
		_, err = app.signIn(wrong)
		assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "%q was not set: %v", wrong, err)
	}
	_, err = app.signIn(resetOldPassword)
	require.NoError(t, err, "the old password still works")
	require.NoError(t, app.ac.Mailer.Close(ctx), "wait for the notices that are queued")
	assert.Zero(t, app.outbox.count(), "no refusal sent a notice")

	// A plugin that was never passed to Prepare and Boot says so.
	idle := emailpassword.New(emailpassword.Options{})
	_, err = idle.SetPassword(asAdmin(ctx), user.ID, resetNewPassword)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	_, err = idle.SetPasswordUnchecked(ctx, user.ID, resetNewPassword)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "the unchecked step too: %v", err)
}

// A user who signed up some other way, and has no password, gets a
// credential account from SetPassword, keyed by the user's id, and their
// other accounts are left alone. Setting it again changes that account in
// place. A user with no email address gets the password and no notice.
func TestSetPasswordCreatesTheCredentialAccount(t *testing.T) {
	ctx := context.Background()
	app, _ := bootReset(t, emailpassword.ResetOptions{}, nil)

	grace := &models.User{Email: "grace@example.com", EmailVerified: true}
	require.NoError(t, app.ac.Store.CreateUser(ctx, grace))
	require.NoError(t, app.ac.Store.CreateAccount(ctx, &models.Account{UserID: grace.ID, ProviderID: "github", AccountID: "gh-1"}))
	accountsOf := func(userID string) map[string]*models.Account {
		t.Helper()
		accounts, err := app.ac.Store.ListAccountsForUser(ctx, userID)
		require.NoError(t, err)
		byProvider := map[string]*models.Account{}
		for _, account := range accounts {
			byProvider[account.ProviderID] = account
		}
		require.Len(t, byProvider, len(accounts), "one account for each provider")
		return byProvider
	}
	signInGrace := func(password string) error {
		_, err := app.plugin.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: grace.Email, Password: password})
		return err
	}
	require.Len(t, accountsOf(grace.ID), 1, "her only account is the provider's")
	assert.Error(t, signInGrace(resetNewPassword), "she has no password yet")

	updated, err := app.plugin.SetPassword(asAdmin(ctx), grace.ID, resetNewPassword)
	require.NoError(t, err)
	assert.True(t, updated.EmailVerified, "the verified flag is left as it was")
	accounts := accountsOf(grace.ID)
	require.Len(t, accounts, 2)
	credential := accounts[models.ProviderCredential]
	require.NotNil(t, credential, "a credential account was created")
	assert.Equal(t, grace.ID, credential.AccountID, "keyed by the user's id")
	assert.NotEmpty(t, credential.PasswordHash)
	assert.NotEqual(t, resetNewPassword, credential.PasswordHash, "stored as a hash")
	assert.Equal(t, "gh-1", accounts["github"].AccountID, "the provider's account is left alone")
	require.NoError(t, signInGrace(resetNewPassword))

	// A second call changes the credential account, and adds none.
	_, err = app.plugin.SetPassword(asAdmin(ctx), grace.ID, "and then another one")
	require.NoError(t, err)
	again := accountsOf(grace.ID)
	require.Len(t, again, 2)
	assert.Equal(t, credential.ID, again[models.ProviderCredential].ID, "the same account")
	assert.NotEqual(t, credential.PasswordHash, again[models.ProviderCredential].PasswordHash)
	assert.Error(t, signInGrace(resetNewPassword), "the first password no longer works")
	require.NoError(t, signInGrace("and then another one"))

	// A user with no address can't be told, and gets the password all the same.
	nameless := &models.User{}
	require.NoError(t, app.ac.Store.CreateUser(ctx, nameless))
	_, err = app.plugin.SetPasswordUnchecked(ctx, nameless.ID, resetNewPassword)
	require.NoError(t, err)
	assert.Contains(t, accountsOf(nameless.ID), models.ProviderCredential)

	require.NoError(t, app.ac.Mailer.Close(ctx), "wait for the notices that are queued")
	assert.Equal(t, 2, app.outbox.count(), "one notice for each of Grace's two passwords, none for the user without an address")
	assert.Equal(t, grace.Email, app.outbox.last(t).To)
}

// SetPassword needs neither password reset nor a mail sender: with both off
// it sets the password and ends the user's sessions with the reason.
func TestSetPasswordWithoutPasswordReset(t *testing.T) {
	ctx := context.Background()
	ac, p, _ := bootedPlugin(t, nil, nil)
	user, err := p.SignUp(ctx, behemoth.M{"email": "ada@example.com", "password": "correct horse"})
	require.NoError(t, err)
	signedIn, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "correct horse"})
	require.NoError(t, err)

	_, err = p.SetPassword(asAdmin(ctx), user.ID, "another password")
	require.NoError(t, err, "no link URL and no sender are configured")

	ended, err := ac.Store.FindSessionByID(ctx, signedIn.Session.ID)
	require.NoError(t, err)
	assert.NotNil(t, ended.RevokedAt)
	assert.Equal(t, emailpassword.SessionRevokedPasswordReset, ended.RevokedReason)
	_, err = p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "correct horse"})
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "the old password: %v", err)
	_, err = p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "another password"})
	require.NoError(t, err, "the new password signs in")
}
