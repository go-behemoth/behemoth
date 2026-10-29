package plugins_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/plugins/emailverification"
	"github.com/go-behemoth/behemoth/plugins/magiclink"
	"github.com/go-behemoth/behemoth/plugins/phonenumber"
	"github.com/go-behemoth/behemoth/plugins/twofactor"
	"github.com/go-behemoth/behemoth/plugins/twofactor/totp"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file make the database fail at each call of a flow in
// turn. A failure of the database is the server's, so the flow reports it
// with an error that answers 5xx, or carries on when the write was one it
// can do without. It never reports it as the caller's mistake, such as a
// wrong code or an invalid link: that would count against the user's
// attempts, and hide the outage.

// faults fails one call, counting from when it is armed: a call of a
// faultyDB, or a hook handler's run.
type faults struct {
	err    error // what the failing call returns
	mu     sync.Mutex
	armed  bool
	calls  int
	failAt int
}

// errInjected is the database's failure, as an adapter reports a lost
// connection.
var errInjected = errs.NewDatabaseError("faultyDB", errors.New("connection reset by peer"))

// errRefused is a hook handler's failure.
var errRefused = errs.NewForbidden("test handler", "refused_by_handler", "refused by a handler")

// failCall arms f to fail the n-th call from now.
func (f *faults) failCall(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.calls, f.failAt = true, 0, n
}

// stop disarms f, and returns the calls made since it was armed.
func (f *faults) stop() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = false
	return f.calls
}

func (f *faults) next() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed {
		return nil
	}
	f.calls++
	if f.calls == f.failAt {
		return f.err
	}
	return nil
}

// faultyDB is a Database whose calls, those of its transactions included,
// can fail through faults.
type faultyDB struct {
	behemoth.Database
	f *faults
}

func (d faultyDB) Create(ctx context.Context, m behemoth.Model) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.Create(ctx, m)
}

func (d faultyDB) FindOne(ctx context.Context, m behemoth.Model, expr clause.Expression) (behemoth.Model, error) {
	if err := d.f.next(); err != nil {
		return nil, err
	}
	return d.Database.FindOne(ctx, m, expr)
}

func (d faultyDB) FindMany(ctx context.Context, m behemoth.Model, expr clause.Expression, opts *behemoth.QueryOptions) ([]behemoth.Model, error) {
	if err := d.f.next(); err != nil {
		return nil, err
	}
	return d.Database.FindMany(ctx, m, expr, opts)
}

func (d faultyDB) Update(ctx context.Context, m behemoth.Model) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.Update(ctx, m)
}

func (d faultyDB) UpdateOne(ctx context.Context, m behemoth.Model, expr clause.Expression, updates behemoth.M) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.UpdateOne(ctx, m, expr, updates)
}

func (d faultyDB) UpdateMany(ctx context.Context, m behemoth.Model, expr clause.Expression, updates behemoth.M) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.UpdateMany(ctx, m, expr, updates)
}

func (d faultyDB) Delete(ctx context.Context, m behemoth.Model) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.Delete(ctx, m)
}

func (d faultyDB) DeleteOne(ctx context.Context, m behemoth.Model, expr clause.Expression) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.DeleteOne(ctx, m, expr)
}

func (d faultyDB) DeleteMany(ctx context.Context, m behemoth.Model, expr clause.Expression) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.DeleteMany(ctx, m, expr)
}

func (d faultyDB) DeleteAll(ctx context.Context, m behemoth.Model) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.DeleteAll(ctx, m)
}

func (d faultyDB) Count(ctx context.Context, m behemoth.Model, expr clause.Expression) (int64, error) {
	if err := d.f.next(); err != nil {
		return 0, err
	}
	return d.Database.Count(ctx, m, expr)
}

func (d faultyDB) Transaction(ctx context.Context, fn behemoth.TransactionFunc) error {
	if err := d.f.next(); err != nil {
		return err
	}
	return d.Database.Transaction(ctx, func(ctx context.Context, tx behemoth.Database) (any, error) {
		return fn(ctx, faultyDB{Database: tx, f: d.f})
	})
}

// faultApp has every plugin with a flow of its own, over a faultyDB, and a
// handler on each hook point that fails through handlerFaults.
type faultApp struct {
	ac            *core.AuthContext
	passwords     *emailpassword.Plugin
	verification  *emailverification.Plugin
	links         *magiclink.Plugin
	phones        *phonenumber.Plugin
	factors       *twofactor.Plugin
	routes        mountedRoutes
	mail          *outbox
	sms           *texts
	faults        *faults // the database's
	handlerFaults *faults
	users         int
}

func bootFaultApp(t *testing.T) *faultApp {
	t.Helper()
	// The flows run thousands of transactions, and nothing has to outlive
	// the test: without syncing each commit to the disk, the tests take a
	// second instead of half a minute.
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "faults.db")+"?_foreign_keys=on&_busy_timeout=10000&_txlock=immediate&_synchronous=OFF&_journal_mode=MEMORY")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	app := &faultApp{
		routes: mountedRoutes{}, mail: &outbox{}, sms: &texts{},
		faults: &faults{err: errInjected}, handlerFaults: &faults{err: errRefused},
	}
	app.passwords = emailpassword.New(emailpassword.Options{
		Reset: emailpassword.ResetOptions{LinkURL: "https://app.example.com/auth/reset", WaitForSend: true},
	})
	app.verification = emailverification.New(emailverification.Options{
		LinkURL:       "https://app.example.com/auth/verify-email",
		ChangeLinkURL: "https://app.example.com/auth/change-email",
		RevertLinkURL: "https://app.example.com/auth/undo-email-change",
	})
	app.links = magiclink.New(magiclink.Options{LinkURL: "https://app.example.com/auth/magic", WaitForSend: true})
	app.phones = phonenumber.New(app.passwords, phonenumber.Options{PasswordReset: true, WaitForSend: true})
	app.factors = twofactor.New(twofactor.Options{
		Issuer: "Example", Methods: []twofactor.Method{twofactor.MethodTOTP, twofactor.MethodEmail}, WaitForSend: true,
	})
	prepared, err := engine.Prepare([]core.Plugin{app.passwords, app.verification, app.links, app.phones, app.factors}, engine.PrepareConfig{})
	require.NoError(t, err)
	testutils.MigrateApp(t, db, prepared)

	app.ac, err = engine.Boot(context.Background(), prepared, faultyDB{Database: sqliteAdapter.NewSQLiteAdapter(db, prepared.Resolver), f: app.faults}, engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("5a", 32)}, Current: 1},
			// The lowest cost Argon2id takes: the flows hash passwords many times.
			Password: &crypto.Params{Memory: 8, Iterations: 1, Parallelism: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Mail:    core.MailConfig{Sender: app.mail},
		SMS:     core.SMSConfig{Sender: app.sms},
		HTTP:    app.routes,
		Hooks:   func(reg core.HookRegistry) error { return app.onEveryPoint(reg, prepared.Hooks.All()) },
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		app.ac.Mailer.Close(context.Background())
		app.ac.SMS.Close(context.Background())
	})
	return app
}

// onEveryPoint registers on each point a handler that changes nothing, and
// fails when handlerFaults says so.
func (app *faultApp) onEveryPoint(reg core.HookRegistry, points []core.HookPointDef) error {
	for _, def := range points {
		var err error
		switch def.Phase {
		case core.BeforeHookPhase:
			err = reg.OnBefore(def.Point, func(*core.HookContext, behemoth.M) (behemoth.M, error) {
				return nil, app.handlerFaults.next()
			}, nil)
		case core.AfterHookPhase:
			err = reg.OnAfter(def.Point, func(*core.HookContext, any) error { return app.handlerFaults.next() }, nil)
		case core.FailedHookPhase:
			err = reg.OnFailed(def.Point, func(*core.HookContext, core.FailureReason) error { return app.handlerFaults.next() }, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// email returns an address no user of app has had yet.
func (app *faultApp) email() string {
	app.users++
	return fmt.Sprintf("user%d@example.com", app.users)
}

// number returns the phone number of the user email named last.
func (app *faultApp) number() string {
	return fmt.Sprintf("+1415555%04d", app.users)
}

// signUp signs up a new user with a password, and waits for the link the
// verification plugin sends it, which it issues in the background.
func (app *faultApp) signUp(t *testing.T) *models.User {
	t.Helper()
	email := app.email()
	user, err := app.passwords.SignUp(context.Background(), behemoth.M{"email": email, "password": password})
	require.NoError(t, err)
	app.mail.to(t, core.MailEmailVerification, email)
	return user
}

// signIn signs a new user up and in, and returns the user and the context of
// a request that presents the session.
func (app *faultApp) signIn(t *testing.T) (*models.User, context.Context) {
	t.Helper()
	user := app.signUp(t)
	return user, app.passwordSignIn(t, user.Email, models.SessionActive)
}

func (app *faultApp) passwordSignIn(t *testing.T, email string, state models.SessionState) context.Context {
	t.Helper()
	result, err := app.passwords.SignIn(context.Background(), emailpassword.EmailAndPasswordCredentials{Email: email, Password: password})
	require.NoError(t, err)
	require.Equal(t, state, result.Session.State)
	return presenting(context.Background(), app.ac, result.RawToken)
}

// withTOTP signs a new user up and in, turns TOTP on, and returns the
// user's secret and recovery codes, and a pending sign-in that waits for
// the second factor.
func (app *faultApp) withTOTP(t *testing.T) (secret []byte, recovery []string, pending context.Context) {
	t.Helper()
	user, ctx := app.signIn(t)
	enrollment, err := app.factors.Enroll(ctx, twofactor.EnrollRequest{Method: twofactor.MethodTOTP})
	require.NoError(t, err)
	secret, err = totp.DecodeSecret(enrollment.Secret)
	require.NoError(t, err)
	confirmed, err := app.factors.Confirm(ctx, twofactor.ConfirmRequest{Method: twofactor.MethodTOTP, Code: totp.Code(secret, time.Now())})
	require.NoError(t, err)
	return secret, confirmed.RecoveryCodes, app.passwordSignIn(t, user.Email, models.SessionPending)
}

// withEmailCode signs a new user up and in, turns codes by email on, and
// returns a pending sign-in that waits for the second factor.
func (app *faultApp) withEmailCode(t *testing.T) (*models.User, context.Context) {
	t.Helper()
	user, ctx := app.signIn(t)
	_, err := app.factors.Enroll(ctx, twofactor.EnrollRequest{Method: twofactor.MethodEmail})
	require.NoError(t, err)
	code := app.mail.to(t, core.MailTwoFactorSetup, user.Email).Code
	_, err = app.factors.Confirm(ctx, twofactor.ConfirmRequest{Method: twofactor.MethodEmail, Code: code})
	require.NoError(t, err)
	return user, app.passwordSignIn(t, user.Email, models.SessionPending)
}

// to waits for a message of kind to address, and returns the newest.
func (o *outbox) to(t *testing.T, kind core.MailKind, address string) core.MailMessage {
	t.Helper()
	var found core.MailMessage
	require.Eventually(t, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		for i := len(o.links) - 1; i >= 0; i-- {
			if o.links[i].Kind == kind && o.links[i].To == address {
				found = o.links[i]
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond, "no %s message to %s", kind, address)
	return found
}

// to returns the newest code sent to number.
func (s *texts) to(t *testing.T, number string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sent) - 1; i >= 0; i-- {
		if s.sent[i].To == number {
			return s.sent[i].Code
		}
	}
	require.Failf(t, "no text", "no code was sent to %s", number)
	return ""
}

// underEachFailure runs a flow once for each database call it makes, with
// that call failing. prepare brings app to where the flow starts, for a user
// of its own each time, and returns the flow.
func underEachFailure(t *testing.T, app *faultApp, prepare func(t *testing.T) func() error) {
	t.Helper()
	const maxCalls = 200
	mapper := &httpapi.DefaultErrorMapper{}
	for n := 1; n <= maxCalls; n++ {
		run := prepare(t)
		app.faults.failCall(n)
		err := run()
		calls := app.faults.stop()
		if calls < n {
			require.NoError(t, err, "the flow, with no failure")
			require.Greater(t, n, 1, "the flow makes no database call")
			return
		}
		if err != nil {
			status, _ := mapper.Map(err)
			assert.GreaterOrEqual(t, status, http.StatusInternalServerError, "call %d of %d failed, and the caller heard: %v", n, calls, err)
		}
	}
	t.Fatalf("the flow makes more than %d database calls", maxCalls)
}

func TestFlowsReportADatabaseFailureAsTheServers(t *testing.T) {
	for name, prepare := range faultFlows() {
		t.Run(name, func(t *testing.T) {
			app := bootFaultApp(t)
			underEachFailure(t, app, func(t *testing.T) func() error { return prepare(t, app) })
		})
	}
}

// A hook handler that fails stops the flow, and the caller hears the
// handler's error and not one of the flow's own: a sign-in that a handler
// refuses must not read as a wrong password. A flow may also carry on past
// a failing handler of an after point.
func TestFlowsPassOnAFailingHookHandler(t *testing.T) {
	const maxRuns = 200
	for name, prepare := range faultFlows() {
		t.Run(name, func(t *testing.T) {
			app := bootFaultApp(t)
			for n := 1; n <= maxRuns; n++ {
				run := prepare(t, app)
				app.handlerFaults.failCall(n)
				err := run()
				runs := app.handlerFaults.stop()
				if runs < n {
					require.NoError(t, err, "the flow, with no failure")
					return
				}
				if err != nil {
					assert.ErrorIs(t, err, errRefused, "handler %d of %d failed, and the caller heard: %v", n, runs, err)
				}
			}
			t.Fatalf("the flow runs more than %d handlers", maxRuns)
		})
	}
}

// faultFlows returns, by name, the flows of the plugins. Each prepares app
// for a user of its own, and returns the flow to run.
func faultFlows() map[string]func(t *testing.T, app *faultApp) func() error {
	ctx := context.Background()
	return map[string]func(t *testing.T, app *faultApp) func() error{
		"password sign-up": func(t *testing.T, app *faultApp) func() error {
			email := app.email()
			return func() error {
				_, err := app.passwords.SignUp(ctx, behemoth.M{"email": email, "password": password})
				return err
			}
		},
		"password sign-in": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			return func() error {
				_, err := app.passwords.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{Email: user.Email, Password: password})
				return err
			}
		},
		"sign-out": func(t *testing.T, app *faultApp) func() error {
			_, session := app.signIn(t)
			return func() error { return app.passwords.SignOut(session) }
		},
		"password reset request": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			return func() error {
				_, err := app.passwords.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: user.Email})
				return err
			}
		},
		"password reset": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			_, err := app.passwords.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: user.Email})
			require.NoError(t, err)
			token := app.mail.to(t, core.MailPasswordReset, user.Email).Token
			return func() error {
				_, err := app.passwords.ResetPassword(ctx, token, "another horse")
				return err
			}
		},
		"password set by an admin": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			return func() error {
				_, err := app.passwords.SetPassword(asAdmin(ctx), user.ID, "another horse")
				return err
			}
		},
		"verification link request": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			return func() error {
				_, err := app.verification.SendVerification(ctx, emailverification.SendRequest{UserID: user.ID})
				return err
			}
		},
		"verification": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			token := app.mail.to(t, core.MailEmailVerification, user.Email).Token
			return func() error {
				_, err := app.verification.Verify(ctx, token)
				return err
			}
		},
		"email change request": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			to := app.email()
			return func() error {
				_, err := app.verification.RequestChange(actingFor(ctx, user.ID), emailverification.ChangeRequest{NewEmail: to})
				return err
			}
		},
		"email change": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			to := app.email()
			_, err := app.verification.RequestChange(actingFor(ctx, user.ID), emailverification.ChangeRequest{NewEmail: to})
			require.NoError(t, err)
			token := app.mail.to(t, core.MailEmailChange, to).Token
			return func() error {
				_, err := app.verification.ConfirmChange(ctx, token)
				return err
			}
		},
		"email change revert": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			to := app.email()
			_, err := app.verification.RequestChange(actingFor(ctx, user.ID), emailverification.ChangeRequest{NewEmail: to})
			require.NoError(t, err)
			_, err = app.verification.ConfirmChange(ctx, app.mail.to(t, core.MailEmailChange, to).Token)
			require.NoError(t, err)
			token := app.mail.to(t, core.MailEmailChangeNotice, user.Email).Token
			return func() error {
				_, err := app.verification.RevertChange(ctx, token)
				return err
			}
		},
		"magic link request": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			return func() error {
				_, err := app.links.RequestLink(ctx, magiclink.LinkRequest{Email: user.Email})
				return err
			}
		},
		"magic link sign-in": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			_, err := app.links.RequestLink(ctx, magiclink.LinkRequest{Email: user.Email})
			require.NoError(t, err)
			token := app.mail.to(t, core.MailMagicLink, user.Email).Token
			return func() error {
				_, err := app.links.Verify(ctx, token)
				return err
			}
		},
		"phone code request": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			number := app.number()
			return func() error {
				_, err := app.phones.SendCode(actingFor(ctx, user.ID), phonenumber.SendRequest{PhoneNumber: number})
				return err
			}
		},
		"phone number": func(t *testing.T, app *faultApp) func() error {
			user := app.signUp(t)
			number := app.number()
			_, err := app.phones.SendCode(actingFor(ctx, user.ID), phonenumber.SendRequest{PhoneNumber: number})
			require.NoError(t, err)
			code := app.sms.to(t, number)
			return func() error {
				_, err := app.phones.AddNumber(asAdmin(ctx), user.ID, number, code)
				return err
			}
		},
		"phone sign-in by code": func(t *testing.T, app *faultApp) func() error {
			number := app.withNumber(t)
			_, err := app.phones.SendCode(ctx, phonenumber.SendRequest{PhoneNumber: number})
			require.NoError(t, err)
			code := app.sms.to(t, number)
			return func() error {
				_, err := app.phones.SignInWithCode(ctx, number, code)
				return err
			}
		},
		"phone sign-in by password": func(t *testing.T, app *faultApp) func() error {
			number := app.withNumber(t)
			return func() error {
				_, err := app.phones.SignIn(ctx, phonenumber.Credentials{PhoneNumber: number, Password: password})
				return err
			}
		},
		"phone password reset request": func(t *testing.T, app *faultApp) func() error {
			number := app.withNumber(t)
			return func() error {
				_, err := app.phones.RequestPasswordReset(ctx, phonenumber.ResetRequest{PhoneNumber: number})
				return err
			}
		},
		"phone password reset": func(t *testing.T, app *faultApp) func() error {
			number := app.withNumber(t)
			_, err := app.phones.RequestPasswordReset(ctx, phonenumber.ResetRequest{PhoneNumber: number})
			require.NoError(t, err)
			code := app.sms.to(t, number)
			return func() error {
				_, err := app.phones.ResetPassword(ctx, number, code, "another horse")
				return err
			}
		},
		"two-factor enrollment": func(t *testing.T, app *faultApp) func() error {
			_, session := app.signIn(t)
			return func() error {
				_, err := app.factors.Enroll(session, twofactor.EnrollRequest{Method: twofactor.MethodTOTP})
				return err
			}
		},
		"two-factor enrollment by email": func(t *testing.T, app *faultApp) func() error {
			_, session := app.signIn(t)
			return func() error {
				_, err := app.factors.Enroll(session, twofactor.EnrollRequest{Method: twofactor.MethodEmail})
				return err
			}
		},
		"two-factor confirmation": func(t *testing.T, app *faultApp) func() error {
			_, session := app.signIn(t)
			enrollment, err := app.factors.Enroll(session, twofactor.EnrollRequest{Method: twofactor.MethodTOTP})
			require.NoError(t, err)
			secret, err := totp.DecodeSecret(enrollment.Secret)
			require.NoError(t, err)
			return func() error {
				_, err := app.factors.Confirm(session, twofactor.ConfirmRequest{Method: twofactor.MethodTOTP, Code: totp.Code(secret, time.Now())})
				return err
			}
		},
		"two-factor confirmation by email": func(t *testing.T, app *faultApp) func() error {
			user, session := app.signIn(t)
			_, err := app.factors.Enroll(session, twofactor.EnrollRequest{Method: twofactor.MethodEmail})
			require.NoError(t, err)
			code := app.mail.to(t, core.MailTwoFactorSetup, user.Email).Code
			return func() error {
				_, err := app.factors.Confirm(session, twofactor.ConfirmRequest{Method: twofactor.MethodEmail, Code: code})
				return err
			}
		},
		"two-factor status": func(t *testing.T, app *faultApp) func() error {
			_, session := app.signIn(t)
			return func() error {
				_, err := app.factors.Status(session)
				return err
			}
		},
		"two-factor disable": func(t *testing.T, app *faultApp) func() error {
			user, _ := app.withEmailCode(t)
			session := app.passwordSignIn(t, user.Email, models.SessionPending)
			code := app.secondFactorByEmail(t, user, session)
			return func() error {
				_, err := app.factors.Disable(code, twofactor.DisableRequest{Method: twofactor.MethodEmail})
				return err
			}
		},
		"recovery codes": func(t *testing.T, app *faultApp) func() error {
			user, _ := app.withEmailCode(t)
			session := app.passwordSignIn(t, user.Email, models.SessionPending)
			signedIn := app.secondFactorByEmail(t, user, session)
			return func() error {
				_, err := app.factors.RegenerateRecoveryCodes(signedIn)
				return err
			}
		},
		"two-factor reset": func(t *testing.T, app *faultApp) func() error {
			user, _ := app.withEmailCode(t)
			return func() error {
				_, err := app.factors.Reset(asAdmin(ctx), twofactor.ResetRequest{UserID: user.ID})
				return err
			}
		},
		"second factor challenge": func(t *testing.T, app *faultApp) func() error {
			_, pending := app.withEmailCode(t)
			return func() error {
				_, err := app.factors.Challenge(pending)
				return err
			}
		},
		"second factor code request": func(t *testing.T, app *faultApp) func() error {
			_, pending := app.withEmailCode(t)
			return func() error {
				_, err := app.factors.SendCode(pending, twofactor.SendCodeRequest{Method: twofactor.MethodEmail})
				return err
			}
		},
		"second factor by email": func(t *testing.T, app *faultApp) func() error {
			user, pending := app.withEmailCode(t)
			_, err := app.factors.SendCode(pending, twofactor.SendCodeRequest{Method: twofactor.MethodEmail})
			require.NoError(t, err)
			code := app.mail.to(t, core.MailTwoFactorCode, user.Email).Code
			return func() error {
				_, err := app.factors.Verify(pending, twofactor.VerifyRequest{Method: twofactor.MethodEmail, Code: code})
				return err
			}
		},
		"second factor by TOTP": func(t *testing.T, app *faultApp) func() error {
			secret, _, pending := app.withTOTP(t)
			code := nextTOTP(secret)
			return func() error {
				_, err := app.factors.Verify(pending, twofactor.VerifyRequest{Method: twofactor.MethodTOTP, Code: code})
				return err
			}
		},
		"second factor by recovery code": func(t *testing.T, app *faultApp) func() error {
			_, recovery, pending := app.withTOTP(t)
			return func() error {
				_, err := app.factors.Verify(pending, twofactor.VerifyRequest{Method: twofactor.MethodRecoveryCode, Code: recovery[0]})
				return err
			}
		},
	}
}

// withNumber signs a new user up, gives it a phone number by the code flow,
// and returns the number.
func (app *faultApp) withNumber(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	user := app.signUp(t)
	number := app.number()
	_, err := app.phones.SendCode(actingFor(ctx, user.ID), phonenumber.SendRequest{PhoneNumber: number})
	require.NoError(t, err)
	_, err = app.phones.AddNumber(asAdmin(ctx), user.ID, number, app.sms.to(t, number))
	require.NoError(t, err)
	return number
}

// secondFactorByEmail finishes the pending sign-in of user with a code by
// email, and returns the context of a request that presents the session.
func (app *faultApp) secondFactorByEmail(t *testing.T, user *models.User, pending context.Context) context.Context {
	t.Helper()
	before := app.mail.count()
	_, err := app.factors.SendCode(pending, twofactor.SendCodeRequest{Method: twofactor.MethodEmail})
	require.NoError(t, err)
	require.Greater(t, app.mail.count(), before)
	code := app.mail.to(t, core.MailTwoFactorCode, user.Email).Code
	result, err := app.factors.Verify(pending, twofactor.VerifyRequest{Method: twofactor.MethodEmail, Code: code})
	require.NoError(t, err)
	return presenting(context.Background(), app.ac, result.RawToken)
}
