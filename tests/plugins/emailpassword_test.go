package plugins_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/auth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	dbschema "github.com/go-behemoth/behemoth/schema"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/httpapi"
	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE users (
	id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, firstname TEXT, lastname TEXT,
	email_verified BOOLEAN NOT NULL DEFAULT 0, image_url TEXT,
	created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
CREATE TABLE accounts (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL, provider_id TEXT NOT NULL, account_id TEXT NOT NULL,
	password_hash TEXT, access_token TEXT, refresh_token TEXT, id_token TEXT,
	access_token_expires_at TIMESTAMP, refresh_token_expires_at TIMESTAMP, scope TEXT,
	created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, UNIQUE (provider_id, account_id));
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL, lookup_hash TEXT NOT NULL UNIQUE, token_hash TEXT NOT NULL,
	key_version INTEGER NOT NULL, state TEXT NOT NULL, expires_at TIMESTAMP NOT NULL, last_active_at TIMESTAMP,
	fresh_at TIMESTAMP, factors TEXT, ip_address TEXT, user_agent TEXT, impersonator_id TEXT, revoked_at TIMESTAMP,
	revoked_reason TEXT, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
CREATE TABLE audit_log (
	id TEXT PRIMARY KEY, event_type TEXT NOT NULL, outcome TEXT NOT NULL, actor_type TEXT NOT NULL,
	actor_id TEXT, delegate_type TEXT, delegate_id TEXT, subject_type TEXT, subject_id TEXT, session_id TEXT, request_id TEXT,
	ip_address TEXT, user_agent TEXT, metadata TEXT, created_at TIMESTAMP NOT NULL);
CREATE TABLE rate_limits (limit_key TEXT PRIMARY KEY, count BIGINT NOT NULL, expires_at TIMESTAMP NOT NULL);`

type passDispatcher struct{}

func (passDispatcher) RunBefore(_ *core.HookContext, _ core.HookPoint, p behemoth.M) (behemoth.M, error) {
	return p, nil
}
func (passDispatcher) RunAfter(*core.HookContext, core.HookPoint, any) error   { return nil }
func (passDispatcher) RunAfterTx(*core.HookContext, core.HookPoint, any) error { return nil }
func (passDispatcher) Fail(*core.HookContext, core.HookPoint, core.FailureReason) error {
	return nil
}

// authContext wires what Boot would, by hand, with a pass-through
// dispatcher. TestEmailPasswordThroughPrepareAndBoot covers the booted path.
func authContext(t *testing.T) *core.AuthContext {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	c, err := crypto.New(context.Background(), crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
	}, nil)
	require.NoError(t, err)

	st := store.New(sqliteAdapter.NewSQLiteAdapter(db, nil))
	sm := auth.NewSessionManager(st, nil, c,
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		passDispatcher{}, nil, nil)
	// The permission the plugin's Declare would have declared under Prepare.
	permissions := auth.NewDefaultPermissionCatalog()
	require.NoError(t, permissions.Declare(core.PermissionDef{
		Name: emailpassword.PermissionSignOut, Kind: core.PermissionSelf, Description: "sign out", Owner: emailpassword.PluginName,
	}))
	return &core.AuthContext{
		Store:          st,
		Crypto:         c,
		Dispatcher:     passDispatcher{},
		Public:         dbschema.NewPublicView([]dbschema.Table{models.UserTableSchema()}),
		SessionManager: sm,
		Access:         auth.NewAccess(permissions, sm.IsFresh),
		Authenticator:  auth.NewAuthenticator(sm, nil),
	}
}

// headerCarrier is the token carrier of the header delivery, which the
// tests that call a plugin's routes without a router hand to each request,
// as the router would.
var headerCarrier = httpapi.NewTokenCarrier(httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader})

// mountedRoutes is a FrameworkDriver that keeps the routes Boot mounts, by
// path. They arrive wrapped by the router, so calling one runs the router's
// error mapping and logging, which a plugin's own route does not.
type mountedRoutes map[string]httpapi.Route

func (m mountedRoutes) Mount(routes ...httpapi.Route) error {
	for _, r := range routes {
		m[r.Path] = r
	}
	return nil
}

// call invokes a plugin route the way the router does.
func call(t *testing.T, ac *core.AuthContext, route httpapi.Route, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callWith(t, context.Background(), ac, route, body)
}

// callWith is call for a request whose context is ctx, with the caller ctx
// carries.
func callWith(t *testing.T, ctx context.Context, ac *core.AuthContext, route httpapi.Route, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, route.Path, strings.NewReader(body))
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}, Auth: ac, TokenCarrier: headerCarrier}
	rctx.Ctx = httpapi.ContextWithRequest(req.Context(), rctx)
	err := route.Handler(rctx)
	w := httptest.NewRecorder()
	if err != nil {
		// The router's error mapping, with its default mapper.
		status, body := (&httpapi.DefaultErrorMapper{}).Map(err)
		w.Code = status
		require.NoError(t, json.NewEncoder(w.Body).Encode(body))
		return w
	}
	rctx.Response.Flush(w)
	return w
}

func TestEmailPasswordSignUpAndSignInThroughTheStore(t *testing.T) {
	ctx := context.Background()
	ac := authContext(t)
	p := emailpassword.New(emailpassword.Options{})
	require.NoError(t, p.Init(ac))
	routes := map[string]httpapi.Route{}
	for _, r := range p.Routes() {
		routes[r.Path] = r
	}

	w := call(t, ac, routes["/sign-up/email"],
		`{"email":"  Ada@Example.COM ","password":"correct horse","firstname":"Ada","email_verified":true,"role":"admin"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "PasswordHash", "the response never carries the password hash")
	assert.NotContains(t, w.Body.String(), "argon2", "the response never carries the password hash")

	stored, err := ac.Store.FindUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	assert.Equal(t, "ada@example.com", stored.Email, "stored normalized")
	assert.Equal(t, "Ada", stored.Firstname, "profile fields from the payload")
	assert.NotEmpty(t, stored.ID)
	credential, err := ac.Store.FindAccount(ctx, models.ProviderCredential, stored.ID)
	require.NoError(t, err, "signing up creates the user's credential account")
	assert.Equal(t, stored.ID, credential.UserID)
	assert.Contains(t, credential.PasswordHash, "argon2", "the hash is stored on the account")
	assert.False(t, stored.EmailVerified, "a client can't verify its own email")
	assert.Empty(t, stored.Extras(), "a client can't write arbitrary columns (mass assignment)")

	w = call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"another one"}`)
	assert.Equal(t, http.StatusConflict, w.Code, "an existing email can't sign up again")
	assert.JSONEq(t, `{"error":"an account with this email already exists","code":"email_taken"}`, w.Body.String())

	// Sign-up's own rejections are typed: a client reads the code.
	for body, code := range map[string]string{
		`{`:                            "request_validation_error",
		`{"password":"correct horse"}`: emailpassword.ErrorCodeInvalidEmail,
		`{"email":42,"password":"correct horse"}`:             emailpassword.ErrorCodeInvalidEmail,
		`{"email":"not an email","password":"correct horse"}`: emailpassword.ErrorCodeInvalidEmail,
		`{"email":"grace@example.com"}`:                       emailpassword.ErrorCodeInvalidPassword,
		`{"email":"grace@example.com","password":"short"}`:    emailpassword.ErrorCodeInvalidPassword,
	} {
		w = call(t, ac, routes["/sign-up/email"], body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "sign-up body %s", body)
		var answer struct{ Code string }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &answer), "sign-up body %s", body)
		assert.Equal(t, code, answer.Code, "sign-up body %s", body)
	}
	_, err = ac.Store.FindUserByEmail(ctx, "grace@example.com")
	assert.True(t, errs.IsNotFound(err), "none of them created a user: %v", err)

	w = call(t, ac, routes["/sign-in/email"], `{"email":"ADA@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "argon2")
	sessions, err := ac.Store.ListSessionsForUser(ctx, stored.ID)
	require.NoError(t, err)
	assert.Len(t, sessions, 1, "signing in created a session")

	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	assert.NotEqual(t, http.StatusOK, w.Code, "a wrong password is rejected")

	// A user who only has an OAuth account has no password to sign in with.
	oauthOnly := &models.User{Email: "grace@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, oauthOnly))
	require.NoError(t, ac.Store.CreateAccount(ctx, &models.Account{UserID: oauthOnly.ID, ProviderID: "google", AccountID: "g-1"}))
	w = call(t, ac, routes["/sign-in/email"], `{"email":"grace@example.com","password":"correct horse"}`)
	assert.NotEqual(t, http.StatusOK, w.Code, "no credential account, no password sign-in")
}

// Sign-in and sign-out return their errors to the router, which decides the
// status: 401 for a refused credential, 500 without the error's text for a
// failure of the system.
func TestEmailPasswordSignInAndSignOutErrors(t *testing.T) {
	ctx := context.Background()
	ac := authContext(t)
	p := emailpassword.New(emailpassword.Options{})
	require.NoError(t, p.Init(ac))
	routes := map[string]httpapi.Route{}
	for _, r := range p.Routes() {
		routes[r.Path] = r
	}
	require.Equal(t, http.StatusCreated,
		call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`).Code)
	oauthOnly := &models.User{Email: "grace@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, oauthOnly))

	// The three refusals are one answer, so the response does not tell
	// which emails have an account.
	wrongPassword := call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	assert.Equal(t, http.StatusUnauthorized, wrongPassword.Code)
	assert.JSONEq(t, `{"error":"invalid email or password","code":"invalid_credentials"}`, wrongPassword.Body.String())
	for name, body := range map[string]string{
		"unknown email":         `{"email":"nobody@example.com","password":"correct horse"}`,
		"no credential account": `{"email":"grace@example.com","password":"correct horse"}`,
	} {
		w := call(t, ac, routes["/sign-in/email"], body)
		assert.Equal(t, wrongPassword.Code, w.Code, name)
		assert.JSONEq(t, wrongPassword.Body.String(), w.Body.String(), name)
	}

	// Called from code, the refusal is told from a failure by its type.
	_, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "wrong password"})
	assert.True(t, errs.IsCategory(err, errs.CategoryUnauthorized), "%v", err)
	assert.True(t, errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials), "%v", err)

	assert.Equal(t, http.StatusBadRequest, call(t, ac, routes["/sign-in/email"], `not json`).Code)

	// Sign-out requires a signed-in caller. Without one it is refused with
	// the reason, as RequireSession refused it, before anything is read.
	w := call(t, ac, routes["/sign-out"], ``)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), errs.CodeSessionMissing)

	// With the database gone, both answer 500 and send none of the error.
	signedIn := identity.WithCaller(ctx, identity.SignedIn(identity.SessionRef{ID: "s1", UserID: "u1", State: string(models.SessionActive)}, identity.Peer{}))
	require.NoError(t, ac.Store.DB().(*sqliteAdapter.SQLiteAdapter).DB.(*sql.DB).Close())
	for _, path := range []string{"/sign-in/email", "/sign-out"} {
		w := callWith(t, signedIn, ac, routes[path], `{"email":"ada@example.com","password":"correct horse"}`)
		assert.Equal(t, http.StatusInternalServerError, w.Code, path)
		assert.JSONEq(t, `{"error":"an internal error occurred","code":"database_error"}`, w.Body.String(), path)
	}
}

// busyHasher refuses every hash, as a hasher at its bound does once its
// wait runs out.
type busyHasher struct{ crypto.PasswordHasher }

func errServerBusy() error {
	return errs.NewUnavailable("test", errs.CodeServerBusy, "the server is busy, please try again later", 2*time.Second)
}

func (busyHasher) Hash(context.Context, string) (string, error) { return "", errServerBusy() }
func (busyHasher) Verify(context.Context, string, string) (bool, error) {
	return false, errServerBusy()
}

// A busy hasher answers every sign-in with the same 503, whether the email
// has an account or not: the hash that keeps an unknown email's answer as
// slow as a wrong password's returns the hasher's error, as Verify does.
// Sign-up answers 503 for a taken email as for a free one. No failed point
// fires, so a lockout built on them does not count the refusals.
func TestEmailPasswordAnswersBusyTheSameForEveryEmail(t *testing.T) {
	ctx := context.Background()
	tel, rec := telemetrytest.New()
	ac, routes := auditApp(t, tel, nil)
	require.Equal(t, http.StatusCreated,
		call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`).Code)
	require.NoError(t, ac.Store.CreateUser(ctx, &models.User{Email: "grace@example.com"}))
	ac.Crypto.Passwords = busyHasher{ac.Crypto.Passwords}

	known := call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	assert.Equal(t, http.StatusServiceUnavailable, known.Code)
	assert.JSONEq(t, `{"error":"the server is busy, please try again later","code":"server_busy"}`, known.Body.String())
	for name, tc := range map[string]struct{ path, body string }{
		"sign-in, unknown email":         {"/sign-in/email", `{"email":"nobody@example.com","password":"correct horse"}`},
		"sign-in, no credential account": {"/sign-in/email", `{"email":"grace@example.com","password":"correct horse"}`},
		"sign-up, taken email":           {"/sign-up/email", `{"email":"ada@example.com","password":"correct horse"}`},
		"sign-up, free email":            {"/sign-up/email", `{"email":"new@example.com","password":"correct horse"}`},
	} {
		w := call(t, ac, routes[tc.path], tc.body)
		assert.Equal(t, known.Code, w.Code, name)
		assert.JSONEq(t, known.Body.String(), w.Body.String(), name)
	}

	assert.Empty(t, rec.Audit.OfType(string(hooks.HookSignInFailed)))
	assert.Empty(t, rec.Audit.OfType(string(hooks.HookSignUpFailed)))
}

func TestEmailPasswordPluginMetadata(t *testing.T) {
	p := emailpassword.New(emailpassword.Options{})
	assert.Equal(t, emailpassword.PluginName, p.Meta().Name)
	assert.Empty(t, p.Meta().MountPath, "routes sit directly under the base path")
	assert.Empty(t, p.Meta().Dependencies)
	assert.NotEmpty(t, p.Version())
	assert.Empty(t, p.Middlewares(), "no application-wide middleware")
}

// The plugin goes through Prepare and Boot like any other, and its flows
// dispatch through the real dispatcher: every point they fire is declared.
func TestEmailPasswordThroughPrepareAndBoot(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-boot.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	p := emailpassword.New(emailpassword.Options{})
	app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
	require.NoError(t, err)
	assert.Equal(t, []string{emailpassword.PluginName}, app.Order)

	// The application observes the flows through hooks. The sign-in result
	// carries the raw session token, which the test needs to sign out.
	var fired []core.HookPoint
	var failures []string
	var rawToken string
	observe := func(point core.HookPoint) core.AfterHookFunc {
		return func(_ *core.HookContext, result any) error {
			fired = append(fired, point)
			if r, ok := result.(*emailpassword.SignInResult); ok {
				rawToken = r.RawToken
			}
			return nil
		}
	}
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:  httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		Hooks: func(reg core.HookRegistry) error {
			for _, point := range []core.HookPoint{hooks.HookSignUpAfter, hooks.HookSignInAfter, hooks.HookSignOutAfter} {
				if err := reg.OnAfter(point, observe(point), nil); err != nil {
					return err
				}
			}
			return reg.OnFailed(hooks.HookSignInFailed, func(_ *core.HookContext, reason core.FailureReason) error {
				failures = append(failures, reason.Code)
				return nil
			}, nil)
		},
	})
	require.NoError(t, err)

	routes := map[string]httpapi.Route{}
	for _, r := range p.Routes() {
		routes[r.Path] = r
	}
	require.Len(t, routes, 3)

	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	assert.NotEqual(t, http.StatusOK, w.Code)
	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotEmpty(t, rawToken)

	// Sign-out with its route middleware behind the authentication stage,
	// the way the router mounts it.
	signOut := routes["/sign-out"].Handler
	for i := len(routes["/sign-out"].Middlewares) - 1; i >= 0; i-- {
		signOut = routes["/sign-out"].Middlewares[i](signOut)
	}
	signOut = httpapi.AuthenticateRequests(ac.Authenticator, headerCarrier, nil)(signOut)
	signOutWith := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/sign-out", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}, Auth: ac, TokenCarrier: headerCarrier}
		rctx.Ctx = httpapi.ContextWithRequest(req.Context(), rctx)
		rec := httptest.NewRecorder()
		if err := signOut(rctx); err != nil {
			// The session check returns its refusal for the router to
			// answer; this is its error mapping, with the default mapper.
			status, body := (&httpapi.DefaultErrorMapper{}).Map(err)
			rec.Code = status
			require.NoError(t, json.NewEncoder(rec.Body).Encode(body))
			return rec
		}
		rctx.Response.Flush(rec)
		return rec
	}
	assert.Equal(t, http.StatusUnauthorized, signOutWith("").Code, "sign-out needs a session")
	assert.Equal(t, http.StatusOK, signOutWith(rawToken).Code)
	assert.Equal(t, http.StatusUnauthorized, signOutWith(rawToken).Code, "the session is revoked")

	assert.Equal(t, []core.HookPoint{hooks.HookSignUpAfter, hooks.HookSignInAfter, hooks.HookSignOutAfter}, fired)
	assert.Equal(t, []string{"invalid_credentials"}, failures)
}

// A data hook that rejects the new user with a typed error stops the sign-up,
// its error reaches the route's caller unchanged, and auth.signUp.failed fires.
func TestEmailPasswordSignUpReturnsADataHooksVeto(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-veto.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	p := emailpassword.New(emailpassword.Options{})
	app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
	require.NoError(t, err)

	veto := errs.NewInvalidInputError("test", "user", "this email domain is not allowed", nil)
	var failures []core.FailureReason
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Hooks: func(reg core.HookRegistry) error {
			if err := reg.OnBefore(hooks.HookUserBeforeCreate, func(_ *core.HookContext, row behemoth.M) (behemoth.M, error) {
				email, _ := row[models.UserEmail].(string)
				if strings.HasSuffix(email, "@blocked.example") {
					return nil, veto
				}
				if strings.HasSuffix(email, "@broken.example") {
					return nil, errors.New("pq: connection reset by peer")
				}
				return row, nil
			}, nil); err != nil {
				return err
			}
			return reg.OnFailed(hooks.HookSignUpFailed, func(_ *core.HookContext, reason core.FailureReason) error {
				failures = append(failures, reason)
				return nil
			}, nil)
		},
	})
	require.NoError(t, err)

	var signUp httpapi.Route
	for _, r := range p.Routes() {
		if r.Path == "/sign-up/email" {
			signUp = r
		}
	}
	req := httptest.NewRequest(http.MethodPost, signUp.Path, strings.NewReader(`{"email":"eve@blocked.example","password":"correct horse"}`))
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}, Auth: ac, TokenCarrier: headerCarrier}
	rctx.Ctx = httpapi.ContextWithRequest(req.Context(), rctx)

	err = signUp.Handler(rctx)
	assert.ErrorIs(t, err, veto, "the handler's own error, for the router to map")
	status, body := (&httpapi.DefaultErrorMapper{}).Map(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "this email domain is not allowed", body["error"])

	require.Len(t, failures, 1)
	assert.Equal(t, "rejected_by_hook", failures[0].Code)
	assert.ErrorIs(t, failures[0].Cause, veto)
	_, err = ac.Store.FindUserByEmail(ctx, "eve@blocked.example")
	assert.True(t, errs.IsNotFound(err), "nothing was written: %v", err)

	// An untyped error is a failure of the system and not a rejection: a
	// 500 without the error's text, and no failed point.
	w := call(t, ac, signUp, `{"email":"eve@broken.example","password":"correct horse"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "connection reset")
	assert.Len(t, failures, 1)

	w = call(t, ac, signUp, `{"email":"ada@example.com","password":"correct horse"}`)
	assert.Equal(t, http.StatusCreated, w.Code, "other sign-ups are unaffected")
	assert.Len(t, failures, 1)
}

// The exported flows run without an HTTP request, and Values last for one
// operation: what a handler on a flow's before point leaves is there for the
// flow's after point and, as a copy, for the operations nested in the flow
// (the user write, the session create). What those write stays their own.
func TestEmailPasswordFlowsShareValuesAndRunWithoutARequest(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-values.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	p := emailpassword.New(emailpassword.Options{})
	_, err = p.SignUp(ctx, behemoth.M{"email": "early@example.com", "password": "correct horse"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a flow called before Boot is a configuration error: %v", err)

	app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
	require.NoError(t, err)

	type seen struct {
		point  core.HookPoint
		phase  core.HookPhase
		values behemoth.M
		hasReq bool
	}
	var log []seen
	record := func(hctx *core.HookContext) {
		values := behemoth.M{}
		for k, v := range hctx.Values {
			values[k] = v
		}
		log = append(log, seen{hctx.Point, hctx.Phase, values, httpapi.RequestFrom(hctx.Ctx) != nil})
	}
	before := func(note string) core.BeforeHookFunc {
		return func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			record(hctx)
			if note != "" {
				hctx.Values[note] = true
			}
			return payload, nil
		}
	}
	after := func(hctx *core.HookContext, _ any) error { record(hctx); return nil }

	_, err = engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute, CaptureIPAndAgent: true},
		Hooks: func(reg core.HookRegistry) error {
			// An invite plugin in miniature: the code is input, the id it
			// resolves to is a note for later points.
			if err := reg.OnBefore(hooks.HookSignUpBefore, func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
				record(hctx)
				if payload["inviteCode"] == "ABC123" {
					hctx.Values["invite.id"] = "inv-1"
				}
				return payload, nil
			}, nil); err != nil {
				return err
			}
			for point, note := range map[core.HookPoint]string{
				hooks.HookUserBeforeCreate:    "write.note",
				hooks.HookSignInBefore:        "signin.note",
				hooks.HookSessionBeforeCreate: "session.note",
			} {
				if err := reg.OnBefore(point, before(note), nil); err != nil {
					return err
				}
			}
			for _, point := range []core.HookPoint{
				hooks.HookUserAfterCreate, hooks.HookUserCreated, hooks.HookSignUpAfter,
				hooks.HookSessionAfterCreate, hooks.HookSignInAfter,
			} {
				if err := reg.OnAfter(point, after, nil); err != nil {
					return err
				}
			}
			return nil
		},
	})
	require.NoError(t, err)

	user, err := p.SignUp(ctx, behemoth.M{"email": "ada@example.com", "password": "correct horse", "inviteCode": "ABC123"})
	require.NoError(t, err)
	require.NotEmpty(t, user.ID)

	// The flow publishes the email it was called with and its method, for
	// rate-limit rules.
	email := behemoth.M{hooks.HookValueEmail: "ada@example.com", hooks.HookValueMethod: "emailpassword"}
	with := func(base behemoth.M, extra behemoth.M) behemoth.M {
		m := maps.Clone(base)
		maps.Copy(m, extra)
		return m
	}
	invite := with(email, behemoth.M{"invite.id": "inv-1"})
	write := with(invite, behemoth.M{"write.note": true})
	assert.Equal(t, []seen{
		{hooks.HookSignUpBefore, core.BeforeHookPhase, email, false},
		{hooks.HookUserBeforeCreate, core.BeforeHookPhase, invite, false},
		{hooks.HookUserAfterCreate, core.AfterHookPhase, write, false},
		{hooks.HookUserCreated, core.AfterHookPhase, write, false},
		{hooks.HookSignUpAfter, core.AfterHookPhase, invite, false},
	}, log, "the write sees the flow's note; the flow does not see the write's")

	log = nil
	result, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "correct horse"})
	require.NoError(t, err, "sign-in works without an HTTP request")
	assert.NotEmpty(t, result.RawToken)
	assert.Empty(t, result.Session.IPAddress, "no request, no address")

	// The session create publishes its user the same way, in its own copy.
	signIn := with(email, behemoth.M{"signin.note": true})
	sessionBefore := with(signIn, behemoth.M{hooks.HookValueUserID: user.ID})
	session := with(sessionBefore, behemoth.M{"session.note": true})
	assert.Equal(t, []seen{
		{hooks.HookSignInBefore, core.BeforeHookPhase, email, false},
		{hooks.HookSessionBeforeCreate, core.BeforeHookPhase, sessionBefore, false},
		{hooks.HookSessionAfterCreate, core.AfterHookPhase, session, false},
		{hooks.HookSignInAfter, core.AfterHookPhase, signIn, false},
	}, log)

	// Called with a request's context, the same flow gives handlers the request.
	log = nil
	req := httptest.NewRequest(http.MethodPost, "/sign-in/email", nil)
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
	_, err = p.SignIn(httpapi.ContextWithRequest(req.Context(), rctx),
		emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "correct horse"})
	require.NoError(t, err)
	for _, s := range log {
		assert.True(t, s.hasReq, "%s: the request on the context reaches the handler", s.point)
	}
}

// Sign-in's input beyond the email and password reaches the handlers on
// auth.signIn.before as top-level payload keys, from the route's body and
// from a caller's Extra alike, and a handler's rewrite comes back into the
// credentials.
func TestEmailPasswordSignInPassesExtraInputToBeforeHandlers(t *testing.T) {
	var payloads []behemoth.M
	ac, p, routes := bootedPlugin(t, nil, func(reg core.HookRegistry) error {
		return reg.OnBefore(hooks.HookSignInBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			seen := behemoth.M{}
			maps.Copy(seen, payload)
			payloads = append(payloads, seen)
			if payload["captchaToken"] == "bad" {
				return nil, errs.NewValidationError("captcha", "captchaToken", errors.New("captcha failed"))
			}
			return nil, nil
		}, nil)
	})
	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	t.Run("from the route", func(t *testing.T) {
		payloads = nil
		w := call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse","captchaToken":"ok","device":{"id":"d1"}}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, []behemoth.M{{
			"email": "ada@example.com", "password": "correct horse",
			"captchaToken": "ok", "device": map[string]any{"id": "d1"},
		}}, payloads)
	})
	t.Run("a handler rejects on an extra field", func(t *testing.T) {
		w := call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse","captchaToken":"bad"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	})
	t.Run("from code", func(t *testing.T) {
		payloads = nil
		extra := behemoth.M{"captchaToken": "ok", "email": "someone@else.example"}
		_, err := p.SignIn(context.Background(), emailpassword.EmailAndPasswordCredentials{
			Email: "ada@example.com", Password: "correct horse", Extra: extra,
		})
		require.NoError(t, err)
		assert.Equal(t, []behemoth.M{{"email": "ada@example.com", "password": "correct horse", "captchaToken": "ok"}}, payloads,
			"the Email field wins over an Extra key of the same name")
	})
	t.Run("an email that is not a string", func(t *testing.T) {
		payloads = nil
		w := call(t, ac, routes["/sign-in/email"], `{"email":5,"password":"correct horse"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Empty(t, payloads, "refused before the before chain")
	})
	t.Run("no email", func(t *testing.T) {
		w := call(t, ac, routes["/sign-in/email"], `{"password":"correct horse"}`)
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	})
}

// The credentials' payload form: Extra's keys sit next to the two fields, and
// FromMap takes them apart again.
func TestEmailAndPasswordCredentialsPayload(t *testing.T) {
	extra := behemoth.M{"captchaToken": "ok"}
	creds := emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com", Password: "pw", Extra: extra}
	m, err := creds.ToMap()
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"email": "ada@example.com", "password": "pw", "captchaToken": "ok"}, m)

	m["added"] = true
	assert.Equal(t, behemoth.M{"captchaToken": "ok"}, extra, "the payload is a copy; a handler's write does not reach the caller's map")

	var back emailpassword.EmailAndPasswordCredentials
	require.NoError(t, back.FromMap(m))
	assert.Equal(t, emailpassword.EmailAndPasswordCredentials{
		Email: "ada@example.com", Password: "pw", Extra: behemoth.M{"captchaToken": "ok", "added": true},
	}, back)

	require.NoError(t, back.FromMap(map[string]any{"email": "ada@example.com"}))
	assert.Equal(t, emailpassword.EmailAndPasswordCredentials{Email: "ada@example.com"}, back,
		"Extra is replaced, and a missing password is empty")

	err = back.FromMap(map[string]any{"email": "ada@example.com", "password": 5})
	assert.True(t, errs.IsValidationError(err), "got %v", err)
}

// Core limits sign-in, and sign-up, to ten attempts a minute per client
// address. Through the router the eleventh is answered 429 with the time left
// in the window, before the flow looks up the user. Another address is not
// affected, and the two flows count separately. Only the first refusal of a
// window is an audit event; every refusal is counted in the metric.
func TestSignInRouteIsRateLimited(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-limit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	tel, rec := telemetrytest.New()
	mounted := mountedRoutes{}
	app, err := engine.Prepare([]core.Plugin{emailpassword.New(emailpassword.Options{})}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:    httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		Telemetry: tel,
		HTTP:      mounted,
	})
	require.NoError(t, err)

	signIn := mounted["/api/auth/sign-in/email"]
	require.NotNil(t, signIn.Handler)
	attempt := func(remoteAddr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, signIn.Path, strings.NewReader(`{"email":"nobody@example.com","password":"a wrong password"}`))
		req.RemoteAddr = remoteAddr
		rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, signIn.Handler(rctx))
		w := httptest.NewRecorder()
		rctx.Response.Flush(w)
		return w
	}

	for i := 1; i <= 10; i++ {
		require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.7:50000").Code, "attempt %d reaches the route", i)
	}
	w := attempt("203.0.113.7:50001")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	wait, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err, "Retry-After: %q", w.Header().Get("Retry-After"))
	assert.Greater(t, wait, 0)
	assert.LessOrEqual(t, wait, 60, "the time left in the window")
	for i := 1; i <= 2; i++ {
		require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.7:50001").Code, "a refused address stays refused")
	}

	assert.Equal(t, http.StatusUnauthorized, attempt("198.51.100.9:50000").Code, "another address has its own count")

	// The address that used up its sign-ins can still sign up: ten times.
	signUp := mounted["/api/auth/sign-up/email"]
	require.NotNil(t, signUp.Handler)
	register := func(i int) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"email":"user%d@example.com","password":"correct horse"}`, i)
		req := httptest.NewRequest(http.MethodPost, signUp.Path, strings.NewReader(body))
		req.RemoteAddr = "203.0.113.7:50002"
		rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, signUp.Handler(rctx))
		w := httptest.NewRecorder()
		rctx.Response.Flush(w)
		return w
	}
	for i := 1; i <= 10; i++ {
		require.Equal(t, http.StatusCreated, register(i).Code, "sign-up %d reaches the route", i)
	}
	w = register(11)
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	_, err = ac.Store.FindUserByEmail(ctx, "user11@example.com")
	assert.True(t, errs.IsNotFound(err), "the refused sign-up created nothing: %v", err)

	assert.Empty(t, rec.Logger.At(slog.LevelWarn), "the counter was reachable")
	limited := rec.Audit.OfType(telemetry.AuditRateLimitExceeded)
	require.Len(t, limited, 2, "one event per rule and window, not one per refusal")
	assert.Equal(t, "203.0.113.7", limited[0].IPAddress)
	assert.Equal(t, engine.RuleSignInAddress, limited[0].Metadata["rule"])
	assert.Equal(t, engine.RuleSignUpAddress, limited[1].Metadata["rule"])
	assert.EqualValues(t, 3, rec.Metrics.Count(telemetry.MetricRateLimitChecks,
		behemoth.M{telemetry.AttrRule: engine.RuleSignInAddress, telemetry.AttrResult: "limited"}), "every refused sign-in is counted")
}

// The per-address limits are hook rules on the flows, so a flow called from
// another transport's wrapper is limited like the route: a sign-in through
// a caller with an address is refused at the eleventh attempt. Each sign-in
// method counts apart, code that named itself is not counted, and
// RateLimitConfig changes a limit by name.
func TestSignInLimitsHoldForEveryTransport(t *testing.T) {
	ctx := context.Background()
	boot := func(t *testing.T, cfg core.RateLimitConfig) (*emailpassword.Plugin, error) {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-transports.db"))
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		_, err = db.Exec(schema)
		require.NoError(t, err)
		p := emailpassword.New(emailpassword.Options{})
		app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
		require.NoError(t, err)
		_, err = engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
			Crypto: crypto.Config{
				Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
			},
			RateLimit: cfg,
		})
		return p, err
	}
	overGRPC := identity.WithCaller(ctx, identity.Anonymous(identity.Peer{Transport: "grpc", IPAddress: "203.0.113.7"}))
	byPassword := func(p *emailpassword.Plugin, ctx context.Context) error {
		_, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: "nobody@example.com", Password: "a wrong password"})
		return err
	}
	byUsername := func(p *emailpassword.Plugin, ctx context.Context) error {
		_, err := p.SignInWith(ctx, emailpassword.IdentifierCredentials{
			Key: "username", Value: "nobody", Password: "a wrong password", Method: "username",
			Find: func(context.Context, *store.Store, string) (*models.User, error) {
				return nil, errs.NewNotFound("test", models.UserTable, nil)
			},
		})
		return err
	}
	limited := func(err error) bool { return errs.IsCategory(err, errs.CategoryRateLimited) }

	p, err := boot(t, core.RateLimitConfig{})
	require.NoError(t, err)
	for i := 1; i <= 10; i++ {
		err := byPassword(p, overGRPC)
		require.Error(t, err)
		require.False(t, limited(err), "attempt %d reaches the flow: %v", i, err)
	}
	assert.True(t, limited(byPassword(p, overGRPC)), "the eleventh is refused")
	assert.False(t, limited(byUsername(p, overGRPC)), "another method has its own count")
	job := identity.WithCaller(ctx, identity.System("import"))
	for i := 1; i <= 12; i++ {
		require.False(t, limited(byPassword(p, job)), "code in the process has no address to count")
	}

	p, err = boot(t, core.RateLimitConfig{Limits: map[string]core.Limit{engine.RuleSignInAddress: {Max: 2, Window: time.Minute}}})
	require.NoError(t, err)
	assert.False(t, limited(byPassword(p, overGRPC)))
	assert.False(t, limited(byPassword(p, overGRPC)))
	assert.True(t, limited(byPassword(p, overGRPC)), "the limit the configuration set")

	p, err = boot(t, core.RateLimitConfig{Disabled: []string{engine.RuleSignInAddress}})
	require.NoError(t, err)
	for i := 1; i <= 12; i++ {
		require.False(t, limited(byPassword(p, overGRPC)), "the rule is off")
	}

	_, err = boot(t, core.RateLimitConfig{Disabled: []string{"core.signin.route"}})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a name no rule has: %v", err)
}

// limitsPlugin declares a token kind and one hook rate-limit rule per firing
// site that publishes a value to key on.
type limitsPlugin struct{}

const kindInvite models.TokenKind = "limits.invite"

func (limitsPlugin) Meta() core.PluginMeta             { return core.PluginMeta{Name: "limits"} }
func (limitsPlugin) Version() string                   { return "0.0.0" }
func (limitsPlugin) Init(*core.AuthContext) error      { return nil }
func (limitsPlugin) Routes() []httpapi.Route           { return nil }
func (limitsPlugin) Middlewares() []httpapi.Middleware { return nil }
func (limitsPlugin) Register(core.HookRegistry) error  { return nil }
func (limitsPlugin) Declare(ic *core.PluginInitContext) error {
	if err := ic.Tokens.Declare(core.TokenKindDef{Kind: kindInvite, DefaultTTL: time.Hour, Backend: core.TokenBackendDB}); err != nil {
		return err
	}
	value := core.KeyByValues
	for _, rule := range []core.HookRateLimitRule{
		{Name: "limits.signup.email", Point: hooks.HookSignUpBefore, KeyFunc: value(hooks.HookValueEmail), Limit: core.Limit{Max: 1, Window: time.Minute}},
		{Name: "limits.signin.email", Point: hooks.HookSignInBefore, KeyFunc: value(hooks.HookValueEmail), Limit: core.Limit{Max: 2, Window: time.Minute}},
		{Name: "limits.session.user", Point: hooks.HookSessionBeforeCreate, KeyFunc: value(hooks.HookValueUserID), Limit: core.Limit{Max: 3, Window: time.Minute}},
		{Name: "limits.token.subject", Point: hooks.HookTokenBeforeIssue, KeyFunc: value(hooks.HookValueTokenKind, hooks.HookValueTokenSubject), Limit: core.Limit{Max: 2, Window: time.Minute}},
	} {
		if err := ic.RateLimits.DeclareHookRateLimitRule(rule); err != nil {
			return err
		}
	}
	return nil
}

// A hook rule keys on what the firing site published in Values: the email of
// a sign-up and a sign-in, the user of a session create, the kind and subject
// of a token issue. No call here has an HTTP request, so the limits hold for
// a CLI or a job as they do for a route.
func TestHookRateLimitRulesKeyOnPublishedValues(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-hook-limit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema + `
CREATE TABLE tokens (
	id TEXT PRIMARY KEY, kind TEXT NOT NULL, subject TEXT, lookup_hash TEXT NOT NULL, token_hash TEXT NOT NULL,
	key_version INTEGER NOT NULL, metadata TEXT, expires_at TIMESTAMP, consumed_at TIMESTAMP, revoked_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL);`)
	require.NoError(t, err)

	tel, rec := telemetrytest.New()
	p := emailpassword.New(emailpassword.Options{})
	app, err := engine.Prepare([]core.Plugin{p, limitsPlugin{}}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Telemetry: tel,
		Hooks: func(reg core.HookRegistry) error {
			// A handler's rewrite of the payload does not move the attempt
			// to another count: the rule keys on the email the caller sent.
			return reg.OnBefore(hooks.HookSignInBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
				if payload["email"] == "alias@example.com" {
					payload["email"] = "ada@example.com"
				}
				return payload, nil
			}, nil)
		},
	})
	require.NoError(t, err)
	limited := func(err error) bool { return errs.IsCategory(err, errs.CategoryRateLimited) }

	// Sign-up: one per email, counted on the normalized form.
	user, err := p.SignUp(ctx, behemoth.M{"email": "ada@example.com", "password": "correct horse"})
	require.NoError(t, err)
	_, err = p.SignUp(ctx, behemoth.M{"email": "  ADA@Example.com ", "password": "correct horse"})
	assert.True(t, limited(err), "the same email in another spelling shares the count: %v", err)
	_, err = p.SignUp(ctx, behemoth.M{"email": "grace@example.com", "password": "correct horse"})
	require.NoError(t, err, "another email has its own count")

	// Sign-in: two per email, whatever the password and the outcome.
	signIn := func(email, password string) error {
		_, err := p.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: email, Password: password})
		return err
	}
	require.NoError(t, signIn("ada@example.com", "correct horse"))
	err = signIn(" Ada@Example.com", "a wrong password")
	assert.True(t, errs.IsCategory(err, errs.CategoryUnauthorized), "the second attempt reaches the flow: %v", err)
	assert.True(t, limited(signIn("ada@example.com", "correct horse")), "the third attempt on the email is refused")
	require.NoError(t, signIn("grace@example.com", "correct horse"), "another email has its own count")
	require.NoError(t, signIn("alias@example.com", "correct horse"), "counted under the email that was sent, signed in as the rewritten one")
	for i := 1; i <= 3; i++ {
		err = signIn(" ", "a wrong password")
		assert.True(t, errs.IsCategory(err, errs.CategoryUnauthorized), "attempt %d without an email is not counted by the rule per email: %v", i, err)
	}

	// Session create: three per user. Ada's two sign-ins above made two.
	create := func(userID string) error {
		_, _, err := ac.SessionManager.Create(ctx, userID, core.SessionMeta{State: models.SessionActive})
		return err
	}
	require.NoError(t, create(user.ID))
	assert.True(t, limited(create(user.ID)), "the fourth session of the user is refused")
	require.NoError(t, create("another-user"))

	// Token issue: two per kind and subject.
	issue := func(subject string) error {
		_, _, err := ac.TokenManager.Issue(ctx, kindInvite, subject, nil)
		return err
	}
	require.NoError(t, issue("team-1"))
	require.NoError(t, issue("team-1"))
	assert.True(t, limited(issue("team-1")), "the third token for the subject is refused")
	require.NoError(t, issue("team-2"))
	for i := 1; i <= 3; i++ {
		_, _, err = ac.TokenManager.Issue(ctx, kindInvite, nil, nil)
		require.NoError(t, err, "token %d without a subject is not counted by the rule per subject", i)
	}
	assert.EqualValues(t, 3, rec.Metrics.Count(telemetry.MetricRateLimitChecks,
		behemoth.M{telemetry.AttrRule: "limits.signin.email", telemetry.AttrResult: "skipped"}), "a skipped rule is counted as such")

	var rules []any
	for _, e := range rec.Audit.OfType(telemetry.AuditRateLimitExceeded) {
		rules = append(rules, e.Metadata["rule"])
	}
	assert.Equal(t, []any{"limits.signup.email", "limits.signin.email", "limits.session.user", "limits.token.subject"}, rules)
}

// The email and password rules are the plugin's own options.
func TestEmailPasswordOptions(t *testing.T) {
	signUp := func(t *testing.T, opts emailpassword.Options, body string) int {
		t.Helper()
		ac := authContext(t)
		p := emailpassword.New(opts)
		require.NoError(t, p.Init(ac))
		for _, r := range p.Routes() {
			if r.Path == "/sign-up/email" {
				return call(t, ac, r, body).Code
			}
		}
		t.Fatal("no sign-up route")
		return 0
	}
	const ok, rejected = http.StatusCreated, http.StatusBadRequest

	t.Run("defaults", func(t *testing.T) {
		assert.Equal(t, ok, signUp(t, emailpassword.Options{}, `{"email":"a@example.com","password":"12345678"}`))
		assert.Equal(t, rejected, signUp(t, emailpassword.Options{}, `{"email":"a@example.com","password":"1234567"}`), "shorter than 8")
		assert.Equal(t, rejected, signUp(t, emailpassword.Options{},
			`{"email":"a@example.com","password":"`+strings.Repeat("x", 129)+`"}`), "longer than 128")
		assert.Equal(t, rejected, signUp(t, emailpassword.Options{}, `{"email":"not-an-email","password":"12345678"}`))
		assert.Equal(t, ok, signUp(t, emailpassword.Options{}, `{"email":"a@example.com","password":"пароль12"}`),
			"length is counted in characters, not bytes")
	})
	t.Run("custom limits and checks", func(t *testing.T) {
		opts := emailpassword.Options{
			MinPasswordLength: 12,
			ValidatePassword: func(pw string) error {
				if strings.Contains(pw, "password") {
					return errors.New("too common")
				}
				return nil
			},
			ValidateEmail: func(email string) error {
				if !strings.HasSuffix(email, "@example.com") {
					return errors.New("company addresses only")
				}
				return nil
			},
		}
		assert.Equal(t, ok, signUp(t, opts, `{"email":"a@example.com","password":"correct horse"}`))
		assert.Equal(t, rejected, signUp(t, opts, `{"email":"a@example.com","password":"12345678"}`), "below the custom minimum")
		assert.Equal(t, rejected, signUp(t, opts, `{"email":"a@example.com","password":"password-password"}`), "the extra check")
		assert.Equal(t, rejected, signUp(t, opts, `{"email":"a@other.org","password":"correct horse"}`), "the custom email check")
	})
	t.Run("contradicting limits fail Init", func(t *testing.T) {
		p := emailpassword.New(emailpassword.Options{MinPasswordLength: 20, MaxPasswordLength: 10})
		err := p.Init(authContext(t))
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	})
}

// What an application sees in its logs: a boot summary, a warning for a
// configuration that is probably a mistake, the SQL statements without their
// values, and a sign-in that failed because the database did.
func TestEmailPasswordLogging(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-log.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	tel, rec := telemetrytest.New()
	mounted := mountedRoutes{}
	p := emailpassword.New(emailpassword.Options{})
	app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil).WithLogger(tel.Logger), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Telemetry: tel,
		Router:    httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}, ClientIPHeader: "X-Real-IP"}, // no TrustedProxies: the header is ignored
		HTTP:      mounted,
	})
	require.NoError(t, err)

	find := func(level slog.Level, component string) []telemetrytest.LogEntry {
		var out []telemetrytest.LogEntry
		for _, e := range rec.Logger.At(level) {
			if e.Fields[telemetry.FieldComponent] == component {
				out = append(out, e)
			}
		}
		return out
	}

	boot := find(slog.LevelInfo, "boot")
	require.Len(t, boot, 1)
	assert.Equal(t, "behemoth booted", boot[0].Message)
	assert.Equal(t, []string{emailpassword.PluginName}, boot[0].Fields["plugins"])
	assert.Equal(t, 3, boot[0].Fields["routes"])
	assert.Equal(t, true, boot[0].Fields["routes_mounted"])
	require.Len(t, find(slog.LevelWarn, "boot"), 1, "ClientIPHeader without TrustedProxies is warned about")

	routes := map[string]httpapi.Route{}
	for _, r := range p.Routes() {
		routes[r.Path] = r
	}
	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// The sign-up's statements were logged, including the insert that ran
	// inside its transaction, and none of them carries a value.
	statements := find(slog.LevelDebug, "storage.sqlite")
	require.NotEmpty(t, statements)
	var sawInsert bool
	for _, e := range statements {
		stmt := e.Fields[telemetry.FieldStatement].(string)
		sawInsert = sawInsert || strings.HasPrefix(stmt, "INSERT INTO users")
		assert.NotContains(t, fmt.Sprint(e.Fields), "ada@example.com")
		assert.NotContains(t, fmt.Sprint(e.Fields), "argon2")
	}
	assert.True(t, sawInsert, "the insert inside the sign-up transaction was not logged")

	// The rest goes through the router, which maps and logs a route's error.
	signIn := mounted["/api/auth/sign-in/email"]
	require.NotNil(t, signIn.Handler)
	viaRouter := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, signIn.Path, strings.NewReader(body))
		rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, signIn.Handler(rctx))
		w := httptest.NewRecorder()
		rctx.Response.Flush(w)
		return w
	}

	// A wrong password is a rejection: 401, and nothing is logged at Error.
	w = viaRouter(`{"email":"ada@example.com","password":"wrong password"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Empty(t, rec.Logger.At(slog.LevelError))

	// With the database gone, the sign-in fails for a reason the client is
	// not told. The router answers 500 and logs it, once.
	require.NoError(t, db.Close())
	w = viaRouter(`{"email":"ada@example.com","password":"correct horse"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "closed", "the response carries none of the error's text")
	failed := rec.Logger.At(slog.LevelError)
	require.Len(t, failed, 1)
	assert.Equal(t, "request failed", failed[0].Message)
	assert.Equal(t, "router", failed[0].Fields[telemetry.FieldComponent])
	assert.Equal(t, "database", failed[0].Fields[telemetry.FieldErrorCategory])
	assert.Equal(t, http.StatusInternalServerError, failed[0].Fields[telemetry.FieldStatus])
}

// What a route behind the session check answers, through the router. A
// request without a usable session is refused: 401 with a public message and
// a code, and nothing is logged as an error. A database that fails while the
// session is looked up is the server's failure: 500 without the error's
// text, logged once. It used to be a 401 that carried the text.
func TestSessionCheckThroughTheRouter(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-session-check.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	tel, rec := telemetrytest.New()
	mounted := mountedRoutes{}
	app, err := engine.Prepare([]core.Plugin{emailpassword.New(emailpassword.Options{})}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:    httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		Telemetry: tel,
		HTTP:      mounted,
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusCreated,
		call(t, ac, mounted["/api/auth/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`).Code)
	w := call(t, ac, mounted["/api/auth/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	token := w.Header().Get(httpapi.SessionTokenHeader)
	require.NotEmpty(t, token)

	signOut := mounted["/api/auth/sign-out"]
	viaRouter := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, signOut.Path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, signOut.Handler(rctx), "the router maps the middleware's error to a response")
		w := httptest.NewRecorder()
		rctx.Response.Flush(w)
		return w
	}

	w = viaRouter("")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"missing session token","code":"session_missing"}`, w.Body.String())
	w = viaRouter("never-issued")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "a token of no session is a request without a session, not a 404")
	assert.JSONEq(t, `{"error":"invalid session","code":"session_invalid"}`, w.Body.String())
	assert.Empty(t, rec.Logger.At(slog.LevelError), "a refusal is not a failure")

	// The database goes away. The token is still good.
	require.NoError(t, db.Close())
	w = viaRouter(token)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "not a 401: the client's session has not ended")
	assert.JSONEq(t, `{"error":"an internal error occurred","code":"database_error"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "closed", "the response carries none of the error's text")
	assert.NotContains(t, w.Body.String(), "SessionManager")
	failed := rec.Logger.At(slog.LevelError)
	require.Len(t, failed, 1)
	assert.Equal(t, "request failed", failed[0].Message)
	assert.Equal(t, "router", failed[0].Fields[telemetry.FieldComponent])
	assert.Equal(t, "database", failed[0].Fields[telemetry.FieldErrorCategory])
	assert.Equal(t, http.StatusInternalServerError, failed[0].Fields[telemetry.FieldStatus])
}

// auditApp boots the email/password plugin over a fresh database and returns
// what an audit test needs. tel == nil boots with the default telemetry.
func auditApp(t *testing.T, tel *telemetry.Telemetry, appHooks func(reg core.HookRegistry) error) (*core.AuthContext, map[string]httpapi.Route) {
	t.Helper()
	ac, _, routes := bootedPlugin(t, tel, appHooks)
	return ac, routes
}

// bootedPlugin is auditApp that also returns the plugin, for tests that call
// its flows from code.
func bootedPlugin(t *testing.T, tel *telemetry.Telemetry, appHooks func(reg core.HookRegistry) error) (*core.AuthContext, *emailpassword.Plugin, map[string]httpapi.Route) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ep-audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)

	p := emailpassword.New(emailpassword.Options{})
	app, err := engine.Prepare([]core.Plugin{p}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(context.Background(), app, sqliteAdapter.NewSQLiteAdapter(db, nil), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ef", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:    httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		Telemetry: tel,
		Hooks:     appHooks,
	})
	require.NoError(t, err)
	// Each route as the router runs it, without the router's other stages:
	// the authentication stage, then the route's middlewares.
	stage := httpapi.AuthenticateRequests(ac.Authenticator, headerCarrier, nil)
	routes := map[string]httpapi.Route{}
	for _, r := range p.Routes() {
		h := r.Handler
		for i := len(r.Middlewares) - 1; i >= 0; i-- {
			h = r.Middlewares[i](h)
		}
		r.Handler, r.Middlewares = stage(h), nil
		routes[r.Path] = r
	}
	return ac, p, routes
}

// With no telemetry configured, Boot records audit events to the audit_log
// table: who signed up and in, which attempts failed and on which account.
func TestAuditLogRecordsTheAuthFlows(t *testing.T) {
	ctx := context.Background()
	var rawToken string
	ac, routes := auditApp(t, nil, func(reg core.HookRegistry) error {
		return reg.OnAfter(hooks.HookSignInAfter, func(_ *core.HookContext, result any) error {
			rawToken = result.(*emailpassword.SignInResult).RawToken
			return nil
		}, nil)
	})

	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	call(t, ac, routes["/sign-in/email"], `{"email":"nobody@example.com","password":"whatever it is"}`)
	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	user, err := ac.Store.FindUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	one := func(eventType string) telemetry.AuditEvent {
		t.Helper()
		page, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{Types: []string{eventType}})
		require.NoError(t, err)
		require.Len(t, page.Events, 1, eventType)
		return page.Events[0]
	}

	// The user's creation, written in the insert's transaction. Sign-up has
	// no session yet, so the new user is its own actor.
	created := one(telemetry.AuditUserCreated)
	assert.Equal(t, telemetry.OutcomeSuccess, created.Outcome)
	assert.Equal(t, models.UserTable, created.SubjectType)
	assert.Equal(t, user.ID, created.SubjectID)
	assert.Equal(t, telemetry.ActorUser, created.ActorType)
	assert.Equal(t, user.ID, created.ActorID)
	assert.Equal(t, user.ID, one(string(hooks.HookSignUpAfter)).SubjectID)

	// The two failed sign-ins, newest first: an unknown address, then a
	// wrong password for a known account. Neither is attributed to a user.
	failed, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{Outcome: telemetry.OutcomeFailure})
	require.NoError(t, err)
	require.Len(t, failed.Events, 2)
	unknown, wrong := failed.Events[0], failed.Events[1]
	assert.Equal(t, string(hooks.HookSignInFailed), unknown.Type)
	assert.Equal(t, "user_not_found", unknown.Metadata["code"])
	assert.NotContains(t, unknown.Metadata, "causeCode", "a rejection without a typed cause has none")
	assert.Equal(t, "nobody@example.com", unknown.Metadata["email"], "the address that was tried is kept")
	assert.Empty(t, unknown.SubjectID)
	assert.Equal(t, "invalid_credentials", wrong.Metadata["code"])
	assert.Equal(t, user.ID, wrong.SubjectID, "a failed sign-in names the account it targeted")
	assert.Equal(t, telemetry.ActorAnonymous, wrong.ActorType)
	assert.Empty(t, wrong.ActorID)
	assert.Equal(t, "192.0.2.1", wrong.IPAddress, "the request's address")

	// The successful sign-in and the session it created.
	signedIn := one(string(hooks.HookSignInAfter))
	assert.Equal(t, user.ID, signedIn.ActorID)
	assert.Equal(t, user.ID, signedIn.SubjectID)
	session := one(string(hooks.HookSessionAfterCreate))
	assert.Equal(t, models.SessionTable, session.SubjectType)
	assert.Equal(t, session.SubjectID, session.SessionID)
	assert.Equal(t, user.ID, session.ActorID)

	// Everything about one user, as an application would ask for it.
	about, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{SubjectType: models.UserTable, SubjectID: user.ID})
	require.NoError(t, err)
	assert.Len(t, about.Events, 4, "created, signed up, one failed sign-in, signed in")

	// Sign-out: the actor is the session's user.
	signOut := routes["/sign-out"].Handler
	req := httptest.NewRequest(http.MethodPost, "/sign-out", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}, Auth: ac, TokenCarrier: headerCarrier}
	rctx.Ctx = httpapi.ContextWithRequest(req.Context(), rctx)
	require.NoError(t, signOut(rctx))
	require.Equal(t, http.StatusOK, rctx.Response.Code)
	signedOut := one(string(hooks.HookSignOutAfter))
	assert.Equal(t, user.ID, signedOut.ActorID)
	assert.Equal(t, session.SessionID, signedOut.SessionID)
	assert.Equal(t, session.SessionID, one(string(hooks.HookSessionAfterRevoke)).SubjectID)
}

// The user.created event is part of the insert's transaction: when the
// insert is rolled back, so is the event.
func TestAuditEventRollsBackWithTheWrite(t *testing.T) {
	ctx := context.Background()
	veto := errs.NewInvalidInputError("test", "user", "not allowed", nil)
	ac, routes := auditApp(t, nil, func(reg core.HookRegistry) error {
		// Runs after the user row is inserted, inside its transaction.
		return reg.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, _ any) error {
			if rctx := httpapi.RequestFrom(hctx.Ctx); rctx != nil && rctx.Request.Header.Get("X-Veto") != "" {
				return veto
			}
			return nil
		}, nil)
	})

	route := routes["/sign-up/email"]
	req := httptest.NewRequest(http.MethodPost, route.Path, strings.NewReader(`{"email":"ada@example.com","password":"correct horse"}`))
	req.Header.Set("X-Veto", "1")
	rctx := &httpapi.RequestContext{Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}, Auth: ac, TokenCarrier: headerCarrier}
	rctx.Ctx = httpapi.ContextWithRequest(req.Context(), rctx)
	require.ErrorIs(t, route.Handler(rctx), veto)

	_, err := ac.Store.FindUserByEmail(ctx, "ada@example.com")
	require.True(t, errs.IsNotFound(err), "the user was rolled back")
	page, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{Types: []string{telemetry.AuditUserCreated}})
	require.NoError(t, err)
	assert.Empty(t, page.Events, "an event describes a user that was never created")

	// The refused sign-up itself is recorded, with the address it was for.
	refused, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{Types: []string{string(hooks.HookSignUpFailed)}})
	require.NoError(t, err)
	require.Len(t, refused.Events, 1)
	assert.Equal(t, "ada@example.com", refused.Events[0].Metadata["email"])

	// Without the veto the same sign-up commits both.
	w := call(t, ac, route, `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	page, err = ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{Types: []string{telemetry.AuditUserCreated}})
	require.NoError(t, err)
	assert.Len(t, page.Events, 1)
}

// An application's own recorder replaces the table, a no-op recorder turns
// auditing off, and a MultiRecorder feeds both the table and another sink.
func TestAuditRecorderChoices(t *testing.T) {
	ctx := context.Background()
	signUp := func(ac *core.AuthContext, routes map[string]httpapi.Route) {
		t.Helper()
		w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}
	stored := func(ac *core.AuthContext) int {
		t.Helper()
		page, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{})
		require.NoError(t, err)
		return len(page.Events)
	}

	t.Run("own recorder", func(t *testing.T) {
		tel, rec := telemetrytest.New()
		ac, routes := auditApp(t, tel, nil)
		signUp(ac, routes)
		assert.Len(t, rec.Audit.OfType(telemetry.AuditUserCreated), 1, "recorded once the insert committed")
		assert.Len(t, rec.Audit.OfType(string(hooks.HookSignUpAfter)), 1)
		assert.Zero(t, stored(ac), "nothing goes to the table")
		assert.Empty(t, rec.Logger.At(slog.LevelError))
	})

	t.Run("off", func(t *testing.T) {
		ac, routes := auditApp(t, telemetry.New(nil, telemetry.NoOpAuditRecorder{}, nil), nil)
		signUp(ac, routes)
		assert.Zero(t, stored(ac))
	})

	t.Run("table and another sink", func(t *testing.T) {
		sink := &telemetrytest.AuditRecorder{}
		var ac *core.AuthContext
		var routes map[string]httpapi.Route
		// The database recorder needs the database, which auditApp opens;
		// lateRecorder forwards to it once it exists.
		late := &lateRecorder{}
		ac, routes = auditApp(t, telemetry.New(nil, telemetry.MultiRecorder(late, sink), nil), nil)
		late.AuditRecorder = store.NewAuditRecorder(store.New(ac.DB))
		signUp(ac, routes)
		assert.Len(t, sink.OfType(telemetry.AuditUserCreated), 1)
		assert.Equal(t, len(sink.Events()), stored(ac), "both received every event")
	})
}

// lateRecorder is a database recorder whose database is set after Boot.
type lateRecorder struct{ *store.AuditRecorder }

// What an application's dashboards are built on: requests are not involved
// here (no router), so this checks the store, auth, session and audit
// metrics a sign-up and two sign-ins produce.
func TestEmailPasswordMetrics(t *testing.T) {
	tel, rec := telemetrytest.New()
	ac, routes := auditApp(t, tel, nil)
	m := rec.Metrics

	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.EqualValues(t, 1, m.Count(telemetry.MetricSignUp, behemoth.M{telemetry.AttrOutcome: "success"}))
	assert.EqualValues(t, 1, m.Count(telemetry.MetricSignIn, behemoth.M{telemetry.AttrOutcome: "success"}))
	assert.EqualValues(t, 1, m.Count(telemetry.MetricSignIn, behemoth.M{telemetry.AttrOutcome: "failure", telemetry.AttrReason: "invalid_credentials"}))
	assert.EqualValues(t, 1, m.Count(telemetry.MetricSessionCreated, nil))

	// The store's operations are timed by table and operation, those inside
	// the sign-up's transaction included.
	assert.Len(t, m.Observations(telemetry.MetricStoreDuration, behemoth.M{telemetry.AttrOp: "create", telemetry.AttrEntity: models.UserTable}), 1)
	assert.Len(t, m.Observations(telemetry.MetricStoreDuration, behemoth.M{telemetry.AttrOp: "create", telemetry.AttrEntity: models.AccountTable}), 1)
	assert.NotEmpty(t, m.Observations(telemetry.MetricStoreDuration, behemoth.M{telemetry.AttrOp: "transaction"}))
	// Sign-up looked the address up first and found nothing: an error of
	// category not_found, which is not a failure of the database.
	assert.EqualValues(t, 1, m.Count(telemetry.MetricStoreErrors, behemoth.M{
		telemetry.AttrOp: "find_one", telemetry.AttrEntity: models.UserTable, telemetry.AttrErrorCategory: "not_found"}))
	assert.Zero(t, m.Count(telemetry.MetricStoreErrors, behemoth.M{telemetry.AttrErrorCategory: "database"}))

	// No attribute holds an identifier or an address.
	for _, sample := range append(m.Counters(), m.Histograms()...) {
		assert.NotContains(t, fmt.Sprint(sample.Attrs), "ada@example.com", sample.Name)
	}

	// A session lookup by token is counted with its cache state.
	_, err := ac.SessionManager.Validate(context.Background(), "not-a-token")
	require.Error(t, err)
	assert.EqualValues(t, 1, m.Count(telemetry.MetricSessionValidated, behemoth.M{telemetry.AttrOutcome: "failure", telemetry.AttrCache: "miss"}))

	// An audit event that can't be stored is counted by type.
	rec.Audit.Err = errors.New("sink down")
	call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"wrong password"}`)
	assert.EqualValues(t, 1, m.Count(telemetry.MetricAuditRecordFailures, behemoth.M{telemetry.AttrType: string(hooks.HookSignInFailed)}))
}

// The spans of a sign-up and a sign-in form one tree per flow call: store
// operations under their transaction, handlers under their chain, and every
// span ended once.
func TestEmailPasswordTracing(t *testing.T) {
	tel, rec := telemetrytest.New()
	ac, routes := auditApp(t, tel, func(reg core.HookRegistry) error {
		// A handler that reads through its context: the read is traced
		// under the handler's span.
		return reg.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, _ any) error {
			_, err := hctx.Tx.FindUserByEmail(hctx.Ctx, "ada@example.com")
			return err
		}, nil)
	})
	tr := rec.Tracer

	w := call(t, ac, routes["/sign-up/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	under := func(span *telemetrytest.Span, ancestor string) bool {
		for p := span.Parent; p != nil; p = p.Parent {
			if p.Name == ancestor {
				return true
			}
		}
		return false
	}
	named := func(name string, attrs behemoth.M) []*telemetrytest.Span {
		var out []*telemetrytest.Span
	spans:
		for _, s := range tr.Named(name) {
			for k, v := range attrs {
				if s.Attrs()[k] != v {
					continue spans
				}
			}
			out = append(out, s)
		}
		return out
	}

	require.Len(t, tr.Named(telemetry.SpanPasswordHash), 1)

	// The user and the account are inserted in one transaction.
	tx := telemetry.SpanStorePrefix + "transaction"
	userInsert := named(telemetry.SpanStorePrefix+"create", behemoth.M{telemetry.AttrEntity: models.UserTable})
	accountInsert := named(telemetry.SpanStorePrefix+"create", behemoth.M{telemetry.AttrEntity: models.AccountTable})
	require.Len(t, userInsert, 1)
	require.Len(t, accountInsert, 1)
	assert.True(t, under(userInsert[0], tx), "the user insert is not under the transaction")
	assert.True(t, under(accountInsert[0], tx), "the account insert is not under the transaction")

	// The application's handler on data.user.afterCreate: a handler span
	// under the point's chain span, inside the transaction, with the
	// handler's own read under it.
	handlers := named(telemetry.SpanHookHandler, behemoth.M{telemetry.AttrPoint: string(hooks.HookUserAfterCreate), telemetry.AttrPlugin: "app"})
	require.Len(t, handlers, 1)
	require.NotNil(t, handlers[0].Parent)
	assert.Equal(t, telemetry.SpanHookChain, handlers[0].Parent.Name)
	assert.True(t, under(handlers[0], tx))
	var readUnderHandler bool
	for _, s := range named(telemetry.SpanStorePrefix+"find_one", behemoth.M{telemetry.AttrEntity: models.UserTable}) {
		readUnderHandler = readUnderHandler || s.Parent == handlers[0]
	}
	assert.True(t, readUnderHandler, "the handler's read is not a child of its span")

	// Points without handlers get no chain span.
	assert.Empty(t, named(telemetry.SpanHookChain, behemoth.M{telemetry.AttrPoint: string(hooks.HookSignUpAfter)}))

	// A refused sign-in: the lookup that finds nothing is not a failed span.
	call(t, ac, routes["/sign-in/email"], `{"email":"nobody@example.com","password":"whatever it is"}`)
	for _, s := range tr.Spans() {
		assert.NoError(t, s.Err(), "span %q failed", s.Name)
	}

	w = call(t, ac, routes["/sign-in/email"], `{"email":"ada@example.com","password":"correct horse"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, tr.Named(telemetry.SpanPasswordVerify), 1)
	creates := tr.Named(telemetry.SpanSessionCreate)
	require.Len(t, creates, 1)
	sessionInsert := named(telemetry.SpanStorePrefix+"create", behemoth.M{telemetry.AttrEntity: models.SessionTable})
	require.Len(t, sessionInsert, 1)
	assert.Equal(t, creates[0], sessionInsert[0].Parent, "the session insert is not a child of the create span")

	for _, s := range tr.Spans() {
		assert.Equal(t, 1, s.Ended(), "span %q", s.Name)
	}
}
