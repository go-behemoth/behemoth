package emailpassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/utils"
)

// PluginName is the name the plugin registers under (PluginMeta.Name). It
// owns the plugin's routes, and other plugins use it to depend on this one
// or to order their hook handlers around it.
const PluginName = "emailpassword"

// pluginVersion is what Version reports.
const pluginVersion = "0.1.0"

// Plugin is the email and password authentication plugin. It adds the
// sign-up, sign-in and sign-out routes. Users, credential accounts and
// sessions are core tables, written through the store and the session
// manager, and the auth.signUp.*, auth.signIn.* and auth.signOut.* points it
// fires are declared by core.
//
// With Options.Reset.LinkURL set it also adds password reset (reset.go): two
// more routes, the password_reset token kind, the auth.passwordReset.*
// points and their rate-limit rules.
type Plugin struct {
	opts        Options
	authContext *core.AuthContext
	log         telemetry.Logger
	signUp      func(hctx *core.HookContext, in behemoth.M) (*models.User, error)
	signIn      func(hctx *core.HookContext, in IdentifierCredentials) (*SignInResult, error)

	// Set by Init when reset is enabled, nil otherwise.
	resetURL     *url.URL
	resetRequest func(hctx *core.HookContext, in ResetRequest) (*ResetRequestResult, error)
	reset        func(hctx *core.HookContext, in resetInput) (*models.User, error)
}

// Meta implements [core.Plugin]. The plugin depends on no other plugin and
// has no mount path, so its routes sit directly under the router's base path
// (for example /api/auth/sign-in/email).
func (p *Plugin) Meta() core.PluginMeta {
	return core.PluginMeta{Name: PluginName}
}

// Version implements [core.Plugin].
func (p *Plugin) Version() string {
	return pluginVersion
}

// The permissions the plugin declares.
const (
	// PermissionSignOut is the permission SignOut requires: ending the
	// caller's own session.
	PermissionSignOut core.Permission = "session:sign_out"
	// PermissionSetPassword is the admin permission SetPassword requires:
	// setting the password of an account named in the input.
	PermissionSetPassword core.Permission = "user:set_password"
)

// Declare implements [core.Plugin]. Without password reset the plugin
// declares two permissions, PermissionSignOut and PermissionSetPassword, and
// nothing else:
//   - hook points: it fires core's auth.signUp.*, auth.signIn.* and
//     auth.signOut.* points, which core declares before any plugin's Declare
//     runs (CoreDeclareHookPoints);
//   - tables: the password hash lives on the core accounts table;
//   - token kinds: none. Email verification is a plugin of its own
//     (plugins/emailverification), which sends a link to a user this plugin
//     signs up;
//   - rate limits: core declares the limits per client address on sign-in
//     and sign-up.
//
// With password reset enabled it declares the reset's token kind, points and
// rate-limit rules (declareReset). Password change for a signed-in user is
// not built.
func (p *Plugin) Declare(ic *core.PluginInitContext) error {
	for _, def := range []core.PermissionDef{
		{Name: PermissionSignOut, Kind: core.PermissionSelf, Description: "Sign out of this session"},
		{Name: PermissionSetPassword, Kind: core.PermissionAdmin, Description: "Set the password of any account"},
	} {
		if err := ic.Permissions.Declare(def); err != nil {
			return err
		}
	}
	if p.opts.Reset.enabled() {
		return p.declareReset(ic)
	}
	return nil
}

// Register implements [core.Plugin]. The plugin attaches no handlers to
// other points; its contribution is its routes. A second-factor plugin, by
// contrast, would register on auth.signIn.credentialsVerified here.
func (p *Plugin) Register(reg core.HookRegistry) error {
	return nil
}

// Middlewares implements [httpapi.Plugin]. The plugin has no middleware that
// should run on every route of the application.
func (p *Plugin) Middlewares() []httpapi.Middleware {
	return nil
}

// Default password length limits, used when Options leaves them zero.
const (
	DefaultMinPasswordLength = 8
	DefaultMaxPasswordLength = 128
)

// Options configures the plugin. The zero value is usable.
//
// The email and password rules live here and not on core.AuthContext: this
// plugin is the only place a password enters the system, at sign-up and at a
// password reset. A plugin that also sets passwords (an admin screen) should
// depend on this one and go through it, so the rules have one owner.
type Options struct {
	// MinPasswordLength and MaxPasswordLength bound a new password's length
	// at sign-up and at a password reset, counted in characters. Zero means
	// the default (8 and 128). The upper bound also caps the work the
	// password hasher does for one request. Sign-in does not apply them: a
	// password accepted under an older policy still signs in.
	MinPasswordLength int
	MaxPasswordLength int

	// ValidatePassword is an optional extra check on a new password, run
	// after the length check, for example a breached-password lookup.
	// nil = none.
	ValidatePassword func(password string) error

	// ValidateEmail checks the normalized (trimmed, lowercased) email at
	// sign-up. nil = utils.IsValidEmail, a check of the address's shape.
	ValidateEmail func(email string) error

	// Reset configures password reset. It is off until Reset.LinkURL is
	// set, and then needs a mail sender in BootConfig.Mail.
	Reset ResetOptions
}

// New returns the plugin configured with opts. Limits left at zero take
// their defaults. Limits that contradict each other are reported by Init, as
// a configuration error from Boot.
func New(opts Options) *Plugin {
	if opts.MinPasswordLength == 0 {
		opts.MinPasswordLength = DefaultMinPasswordLength
	}
	if opts.MaxPasswordLength == 0 {
		opts.MaxPasswordLength = DefaultMaxPasswordLength
	}
	if opts.ValidateEmail == nil {
		opts.ValidateEmail = func(email string) error {
			if !utils.IsValidEmail(email) {
				return errors.New("invalid email")
			}
			return nil
		}
	}
	if opts.Reset.TTL == 0 {
		opts.Reset.TTL = DefaultResetTTL
	}
	if opts.Reset.RequestLimit == (core.Limit{}) {
		opts.Reset.RequestLimit = core.Limit{Max: 5, Window: 15 * time.Minute}
	}
	return &Plugin{opts: opts}
}

// Init implements [core.Plugin]. It checks the options, keeps the
// AuthContext and wraps the sign-up and sign-in flows with their hook points,
// and the reset flows when reset is enabled (initReset). Passwords are hashed
// with the AuthContext's Crypto.Passwords.
func (p *Plugin) Init(ac *core.AuthContext) error {
	if p.opts.MinPasswordLength < 1 || p.opts.MaxPasswordLength < p.opts.MinPasswordLength {
		return errs.NewConfigurationError("emailpassword.Init",
			fmt.Sprintf("password length limits are invalid: min %d, max %d", p.opts.MinPasswordLength, p.opts.MaxPasswordLength), nil)
	}
	if ac.Crypto.Passwords == nil {
		return errs.NewConfigurationError("emailpassword.Init", "AuthContext.Crypto has no password hasher", nil)
	}
	p.authContext = ac
	p.log = telemetry.NoOpLogger{}
	if ac.Telemetry != nil {
		p.log = telemetry.Named(ac.Telemetry.Logger, PluginName)
	}
	p.signUp = core.WithLifecycle(ac.Dispatcher, hooks.HookSignUpBefore, hooks.HookSignUpAfter, hooks.HookSignUpFailed, p.signUpBody)
	p.signIn = core.WithLifecycle(ac.Dispatcher, hooks.HookSignInBefore, hooks.HookSignInAfter, hooks.HookSignInFailed, p.signInBody)
	if p.opts.Reset.enabled() {
		return p.initReset(ac)
	}
	return nil
}

// validatePassword applies the plugin's rules to a new password, at sign-up
// and at a password reset.
func (p *Plugin) validatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < p.opts.MinPasswordLength || n > p.opts.MaxPasswordLength {
		return fmt.Errorf("password must be %d to %d characters long", p.opts.MinPasswordLength, p.opts.MaxPasswordLength)
	}
	if p.opts.ValidatePassword != nil {
		return p.opts.ValidatePassword(password)
	}
	return nil
}

// Routes implements [httpapi.Plugin]. The two reset routes are added only when
// reset is enabled.
func (p *Plugin) Routes() []httpapi.Route {
	routes := []httpapi.Route{
		{Method: http.MethodPost, Path: "/sign-up/email", Handler: p.handleSignUp},
		{Method: http.MethodPost, Path: "/sign-in/email", Handler: p.handleSignIn},
		// No session middleware: SignOut requires the caller itself.
		{Method: http.MethodPost, Path: "/sign-out", Handler: p.handleSignOut},
	}
	if p.opts.Reset.enabled() {
		routes = append(routes,
			httpapi.Route{Method: http.MethodPost, Path: PathResetRequest, Handler: p.handleResetRequest},
			httpapi.Route{Method: http.MethodPost, Path: PathResetConfirm, Handler: p.handleResetConfirm},
		)
	}
	return routes
}

// EmailAndPasswordCredentials is sign-in's input. Email and Password are all
// the flow itself reads.
//
// It implements [behemoth.Serializable], which is how WithLifecycle turns it
// into the auth.signIn.before payload and back: handlers see "email",
// "password" and the keys of Extra side by side, the way they see a sign-up's
// fields.
type EmailAndPasswordCredentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`

	// Extra holds input the flow does not read but a handler on
	// auth.signIn.before may: a captcha token, a device id. The route fills
	// it with the request fields other than email and password. Nothing in
	// it is stored. After the before chain it holds what the handlers left.
	Extra behemoth.M `json:"-"`
}

// Payload keys of the credentials' two fields.
const (
	credentialEmailKey    = "email"
	credentialPasswordKey = "password"
)

// ToMap returns the credentials as a hook payload: a copy of Extra with
// "email" and "password" set from the fields, so a key of Extra with one of
// those names does not replace them.
func (c *EmailAndPasswordCredentials) ToMap() (map[string]any, error) {
	m := make(map[string]any, len(c.Extra)+2)
	maps.Copy(m, c.Extra)
	m[credentialEmailKey], m[credentialPasswordKey] = c.Email, c.Password
	return m, nil
}

// FromMap sets the credentials from a request body or a rewritten payload.
// "email" and "password" fill the fields and every other key goes to Extra,
// which is replaced. A missing or null email or password is left empty, for
// sign-in to refuse; one that is not a string is a validation error.
func (c *EmailAndPasswordCredentials) FromMap(data map[string]any) error {
	email, password, extra, err := credentialFields(credentialEmailKey, data)
	if err != nil {
		return err
	}
	c.Email, c.Password, c.Extra = email, password, extra
	return nil
}

// IdentifierCredentials is the input of SignInWith: a sign-in by password
// for a user who is named by something other than an email. The username
// plugin signs in with it, with Key "username".
//
// It implements [behemoth.Serializable]. The auth.signIn.before payload is
// flat, like an email sign-in's: Value under Key, "password", and the keys
// of Extra. Method and Find are not part of it.
type IdentifierCredentials struct {
	// Key names the identifier: the payload key Value sits under, and the
	// HookContext.Values key it is published under for rate-limit rules.
	// Required, and not "password".
	Key string
	// Value is the identifier the caller sent, in the form a rule should
	// count it by: SignInWith publishes it as it is.
	Value    string
	Password string
	// Extra holds input only handlers on auth.signIn.before read, as in
	// EmailAndPasswordCredentials.
	Extra behemoth.M
	// Method is the SignInResult's Method: the name of the plugin that
	// calls SignInWith. Empty means this plugin's.
	Method string
	// Find looks the user up by the identifier, after the before chain, so
	// value is what the handlers left. It returns a NotFound error when no
	// user matches; any other error is a failure of the system. Required.
	Find func(ctx context.Context, s *store.Store, value string) (*models.User, error)

	// normalize puts Value in the form a failed sign-in's audit event
	// records. nil = as it is.
	normalize func(string) string
}

// ToMap returns the credentials as a hook payload: a copy of Extra with Key
// and "password" set from the fields, so a key of Extra with one of those
// names does not replace them.
func (c *IdentifierCredentials) ToMap() (map[string]any, error) {
	m := make(map[string]any, len(c.Extra)+2)
	maps.Copy(m, c.Extra)
	m[c.Key], m[credentialPasswordKey] = c.Value, c.Password
	return m, nil
}

// FromMap sets Value, Password and Extra from a rewritten payload, by the
// rules of EmailAndPasswordCredentials.FromMap. Key, Method and Find are
// kept: a handler can't change how the user is looked up.
func (c *IdentifierCredentials) FromMap(data map[string]any) error {
	value, password, extra, err := credentialFields(c.Key, data)
	if err != nil {
		return err
	}
	c.Value, c.Password, c.Extra = value, password, extra
	return nil
}

// credentialFields reads a sign-in's fields from a request body or a
// payload: the identifier under key, "password", and every other key as the
// extra input. A missing or null identifier or password is empty; one that
// is not a string is a validation error.
func credentialFields(key string, data map[string]any) (value, password string, extra behemoth.M, err error) {
	field := func(key string) (string, error) {
		switch v := data[key].(type) {
		case nil:
			return "", nil
		case string:
			return v, nil
		default:
			return "", errs.NewValidationError("emailpassword.SignIn", key, fmt.Errorf("%s must be a string", key))
		}
	}
	if value, err = field(key); err != nil {
		return "", "", nil, err
	}
	if password, err = field(credentialPasswordKey); err != nil {
		return "", "", nil, err
	}
	for k, v := range data {
		if k == key || k == credentialPasswordKey {
			continue
		}
		if extra == nil {
			extra = behemoth.M{}
		}
		extra[k] = v
	}
	return value, password, extra, nil
}

// Codes of the errors sign-up returns for its own rejections. A client
// tells them apart by the code, not by the message.
const (
	// ErrorCodeInvalidEmail is the code of the validation error (400) for
	// an email that is missing, not a string, or refused by
	// Options.ValidateEmail.
	ErrorCodeInvalidEmail = "email_invalid_input"
	// ErrorCodeInvalidPassword is the code of the validation error (400)
	// for a password the plugin's rules refuse, at sign-up and at a
	// password reset.
	ErrorCodeInvalidPassword = "password_invalid_input"
	// ErrorCodeEmailTaken is the code of the conflict error (409) for a
	// sign-up with an email that already has an account.
	ErrorCodeEmailTaken = "email_taken"
)

// errInvalidEmail and errInvalidPassword are sign-up's validation errors.
// cause is the error of the check that refused, kept for logs: its text is
// not sent to the client, since Options.ValidateEmail and ValidatePassword
// are the application's and may say more than a stranger should read.
func errInvalidEmail(cause error) error {
	return errs.NewInvalidInputError("emailpassword.SignUp", credentialEmailKey, "invalid email", cause)
}

func errInvalidPassword(op string, cause error) error {
	return errs.NewInvalidInputError(op, credentialPasswordKey, "invalid password", cause)
}

// errEmailTaken is sign-up's typed conflict (409) for an email that already
// has an account.
func errEmailTaken() error {
	const op, message = "emailpassword.SignUp", "an account with this email already exists"
	return &errs.DomainError{
		Category: errs.CategoryConflict, Op: op, Code: ErrorCodeEmailTaken,
		PublicMessage: message, InternalMessage: op + ": " + message,
	}
}

func (p *Plugin) signUpBody(hctx *core.HookContext, userData behemoth.M) (*models.User, error) {
	ac := hctx.Auth
	dispatcher := ac.Dispatcher

	emailStr, ok := userData[credentialEmailKey].(string)
	if !ok {
		return nil, errInvalidEmail(nil)
	}
	email := strings.ToLower(strings.TrimSpace(emailStr))

	if err := p.opts.ValidateEmail(email); err != nil {
		return nil, errInvalidEmail(err)
	}

	password, _ := userData[credentialPasswordKey].(string)
	if err := p.validatePassword(password); err != nil {
		return nil, errInvalidPassword("emailpassword.SignUp", err)
	}

	emailTaken := func() error {
		if err := dispatcher.Fail(hctx, hooks.HookSignUpFailed, core.FailureReason{Code: "user_exists", Metadata: behemoth.M{hooks.HookValueEmail: email}}); err != nil {
			return err
		}
		return errEmailTaken()
	}
	_, err := ac.Store.FindUserByEmail(hctx.Ctx, email)
	if err == nil {
		if err := decoyHash(hctx, password); err != nil {
			return nil, err
		}
		return nil, emailTaken()
	}
	if !errs.IsNotFound(err) {
		return nil, err // infra error (DB down): not "no such user"
	}

	passwordHash, err := hashPassword(hctx, password)
	if err != nil {
		return nil, err // infra error - no Fail()
	}

	// Only these profile fields are taken from the request. Never the whole
	// payload: every column a model knows — contributed ones included — would
	// become client-writable (mass assignment: "role": "admin", a verified
	// flag, ...). The store assigns the id and timestamps, and fires
	// data.user.beforeCreate / afterCreate (Tier 1) — hooks are where other
	// plugins add their columns. The password hash goes to the user's
	// "credential" account, created with the user or not at all. The data
	// hooks run inside this transaction, so data.user.afterCreate can see a
	// user that the account insert then rolls back; auth.signUp.after fires
	// only once the transaction has committed.
	profile := func(key string) string { v, _ := userData[key].(string); return strings.TrimSpace(v) }
	user := &models.User{
		Firstname: profile(models.UserFirstname),
		Lastname:  profile(models.UserLastname),
		ImageUrl:  profile(models.UserImageURL),
	}
	user.Email = email
	user.EmailVerified = false
	err = ac.Store.Transaction(hctx.Ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, user); err != nil {
			return err
		}
		return tx.CreateAccount(ctx, &models.Account{
			UserID:       user.ID,
			ProviderID:   models.ProviderCredential,
			AccountID:    user.ID,
			PasswordHash: passwordHash,
		})
	})
	if err != nil {
		// Another sign-up took the email after the lookup above: the
		// database refused the insert. It is answered as the lookup would
		// have answered.
		if slices.Equal(errs.DuplicateKeyColumns(err), []string{models.UserEmail}) {
			return nil, emailTaken()
		}
		// Otherwise the error is returned as it is. A typed one keeps its
		// category and public message: a data hook's veto on
		// data.user.beforeCreate or afterCreate, or a classified store
		// error, such as the conflict of another unique column a plugin
		// added (username_taken). An untyped one is a failure of the
		// system: the router answers 500 without its text and logs it.
		return nil, core.FailIfRejected(dispatcher, hctx, hooks.HookSignUpFailed, err,
			core.FailureReason{Metadata: behemoth.M{hooks.HookValueEmail: email}})
	}

	return user, nil
}

// hashPassword hashes password with the AuthContext's hasher, under the
// operation's context. The hasher traces the hash and bounds how many run at
// once, so it can refuse with a busy error (errs.CodeServerBusy).
func hashPassword(hctx *core.HookContext, password string) (string, error) {
	return hctx.Auth.Crypto.Passwords.Hash(hctx.Ctx, password)
}

// decoyHash hashes a password that is thrown away, so that an answer takes
// as long as one that hashes: a sign-in refused before any password was
// checked, and a sign-up for a taken email. Its error is returned to the
// caller, as Verify's and a real hash's are: a busy hasher then answers 503
// for an unknown email as for a known one, and the answer doesn't tell which
// emails have an account. See ADR-0010.
func decoyHash(hctx *core.HookContext, password string) error {
	_, err := hashPassword(hctx, password)
	return err
}

// ErrorCodeInvalidCredentials is the code of the error sign-in returns for an
// unknown email, a user without a password and a wrong password.
const ErrorCodeInvalidCredentials = "invalid_credentials"

// errInvalidCredentials is sign-in's one answer to every credential it
// refuses. The three cases share a code and a message so the response does
// not tell which emails have an account. It is typed (unauthorized, 401), so
// a caller can tell it from a failure of the system, which is any other
// error SignIn returns apart from a hook handler's typed rejection.
//
// identifier is the credentials' Key, for the message: "invalid email or
// password", "invalid username or password". The code is the same for all.
func errInvalidCredentials(identifier string) error {
	return errs.NewUnauthorized("emailpassword.SignIn", ErrorCodeInvalidCredentials, "invalid "+identifier+" or password")
}

// signInBody is the one sign-in by password. How the user is found is the
// credentials' (creds.Find): by email for SignIn, by another plugin's
// identifier for SignInWith. Everything after the lookup is the same.
func (p *Plugin) signInBody(hctx *core.HookContext, creds IdentifierCredentials) (*SignInResult, error) {
	ac := hctx.Auth
	hasher := ac.Crypto.Passwords
	dispatcher := ac.Dispatcher
	errInvalidCredentials := func() error { return errInvalidCredentials(creds.Key) }
	user, err := creds.Find(hctx.Ctx, ac.Store, creds.Value)
	if err != nil {
		if errs.IsNotFound(err) {
			if err := decoyHash(hctx, creds.Password); err != nil {
				return nil, err
			}
			attempted := creds.Value
			if creds.normalize != nil {
				attempted = creds.normalize(attempted)
			}
			if err := dispatcher.Fail(hctx, hooks.HookSignInFailed, core.FailureReason{
				Code: "user_not_found", Metadata: behemoth.M{creds.Key: attempted},
			}); err != nil {
				return nil, err
			}
			return nil, errInvalidCredentials()
		}
		return nil, err // infra error (DB down) — must NOT count toward lockout
	}

	// Every rejection from here on is an attempt on a known account, and
	// its audit event says which.
	rejected := func(code string, cause error) core.FailureReason {
		return core.FailureReason{Code: code, Cause: cause, SubjectType: models.UserTable, SubjectID: user.ID}
	}

	// The password lives on the user's "credential" account. A user without
	// one (signed up through an OAuth provider only) has no password to
	// match, and is answered exactly like a wrong password.
	credential, err := ac.Store.FindAccount(hctx.Ctx, models.ProviderCredential, user.ID)
	if err != nil {
		if errs.IsNotFound(err) {
			if err := decoyHash(hctx, creds.Password); err != nil {
				return nil, err
			}
			if err := dispatcher.Fail(hctx, hooks.HookSignInFailed, rejected("no_credential_account", nil)); err != nil {
				return nil, err
			}
			return nil, errInvalidCredentials()
		}
		return nil, err // infra error — must NOT count toward lockout
	}

	isValid, err := hasher.Verify(hctx.Ctx, credential.PasswordHash, creds.Password)
	if err != nil {
		return nil, err // a busy hasher, a malformed stored hash or a key error — not a wrong password, so not counted as one
	}

	if !isValid {
		if err := dispatcher.Fail(hctx, hooks.HookSignInFailed, rejected("invalid_credentials", nil)); err != nil {
			return nil, err
		}
		return nil, errInvalidCredentials()
	}

	// The seam a 2FA plugin hooks. requireStepUp travels back via the mutated
	// payload - this is the mechanism by which SignIn decides Pending vs Active
	// without knowing anything about TOTP, backup codes, etc. Any plugin may
	// refuse the sign-in here, so a refusal is reported as one by a hook and
	// not as a second factor's. See ADR-0001. The payload names the factor this
	// sign-in checked, which a second factor must not repeat. See ADR-0009.
	checkpoint, err := dispatcher.RunBefore(hctx, hooks.HookSignInCredentialsVerified,
		behemoth.M{hooks.HookValueUserID: user.ID, hooks.HookValueFactor: core.FactorPassword})
	if err != nil {
		return nil, core.FailIfRejected(dispatcher, hctx, hooks.HookSignInFailed, err,
			core.FailureReason{SubjectType: models.UserTable, SubjectID: user.ID})
	}

	state := models.SessionActive
	if requireStepUp, _ := checkpoint[hooks.HookValueRequireStepUp].(bool); requireStepUp {
		state = models.SessionPending
	}

	// The session manager reads the IP address and user agent from the
	// request on hctx.Ctx, so sign-in also works without one (a job, another
	// plugin calling in).
	session, rawToken, err := ac.SessionManager.Create(hctx.Ctx, user.ID, core.SessionMeta{State: state, Factor: core.FactorPassword})
	if err != nil {
		return nil, err
	}

	method := creds.Method
	if method == "" {
		method = PluginName
	}
	return &SignInResult{User: user, Session: session, RawToken: rawToken, Method: method}, nil
}

// SignInResult is core.SignInResult, the result every sign-in plugin
// returns and hands to auth.signIn.after. The name is kept here for callers
// of SignIn.
type SignInResult = core.SignInResult

// SignUp creates a user with an email and a password, and the user's
// credential account. input holds the sign-up fields under their column
// names ("email", "password", and optionally "firstname", "lastname",
// "image_url"); other keys are not stored, but handlers on
// auth.signUp.before see them. It fires auth.signUp.before, .after and
// .failed.
//
// It is the flow behind POST /sign-up/email, for callers that are not that
// route: a CLI, a job, another plugin. Pass the context of the request being
// handled when there is one; hook handlers get the request from it. The
// plugin must have been initialized by Boot.
func (p *Plugin) SignUp(ctx context.Context, input behemoth.M) (*models.User, error) {
	hctx, err := p.operation(ctx, "emailpassword.SignUp")
	if err != nil {
		return nil, err
	}
	email, _ := input[credentialEmailKey].(string)
	publishEmail(hctx, email)
	hctx.Values[hooks.HookValueMethod] = PluginName // core's per-address limit counts each method apart
	return p.signUp(hctx, input)
}

// SignIn verifies creds and creates a session for the user. The session is
// pending instead of active when a handler on auth.signIn.credentialsVerified
// asked for a second factor. It fires auth.signIn.before, .after and
// .failed. Handlers on auth.signIn.before see creds.Extra next to the email
// and password.
//
// It is the flow behind POST /sign-in/email. Like SignUp it runs without an
// HTTP request; the session then records no IP address or user agent unless
// the caller's context carries a request. The raw session token is in the
// result and is the caller's to deliver.
func (p *Plugin) SignIn(ctx context.Context, creds EmailAndPasswordCredentials) (*SignInResult, error) {
	hctx, err := p.operation(ctx, "emailpassword.SignIn")
	if err != nil {
		return nil, err
	}
	publishEmail(hctx, creds.Email)
	hctx.Values[hooks.HookValueMethod] = PluginName // core's per-address limit counts each method apart
	return p.signIn(hctx, IdentifierCredentials{
		Key: credentialEmailKey, Value: creds.Email, Password: creds.Password, Extra: creds.Extra,
		Find: func(ctx context.Context, s *store.Store, email string) (*models.User, error) {
			return s.FindUserByEmail(ctx, email) // the store normalizes the email
		},
		normalize: store.NormalizeEmail,
	})
}

// SignInWith is SignIn for a user named by something other than an email:
// creds.Find looks the user up, and the password check, the second-factor
// checkpoint and the session are this plugin's, as in SignIn. It fires the
// same auth.signIn.* points, and answers a user Find does not know, a user
// without a password and a wrong password with the same typed error.
//
// It is how a plugin adds an identifier without checking passwords itself:
// the username plugin's sign-in is a call of it. That plugin names this one
// in PluginMeta.Dependencies and is given it at construction.
//
// creds.Value is published in the operation's Values under creds.Key, where
// a rate-limit rule on auth.signIn.before keys on it, and creds.Method under
// hooks.HookValueMethod, where core's per-address rule does. No email is
// published: it is not known before the lookup, so a rule per email does not
// count these sign-ins.
func (p *Plugin) SignInWith(ctx context.Context, creds IdentifierCredentials) (*SignInResult, error) {
	const op = "emailpassword.SignInWith"
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return nil, err
	}
	if creds.Key == "" || creds.Key == credentialPasswordKey {
		return nil, errs.NewConfigurationError(op, fmt.Sprintf("IdentifierCredentials.Key %q can't name an identifier", creds.Key), nil)
	}
	if creds.Find == nil {
		return nil, errs.NewConfigurationError(op, "IdentifierCredentials.Find is not set", nil)
	}
	creds.normalize = nil
	delete(hctx.Values, hooks.HookValueEmail) // an enclosing operation's, not this sign-in's
	if creds.Value == "" {
		delete(hctx.Values, creds.Key)
	} else {
		hctx.Values[creds.Key] = creds.Value
	}
	method := creds.Method
	if method == "" {
		method = PluginName
	}
	hctx.Values[hooks.HookValueMethod] = method // core's per-address limit counts each method apart
	return p.signIn(hctx, creds)
}

// publishEmail puts the email a sign-up or sign-in was called with in the
// operation's Values, under hooks.HookValueEmail and in its stored form
// (store.NormalizeEmail). A rate-limit rule on the flow's before point keys
// on it there: HookRateLimitRule.KeyFunc gets the HookContext and not the
// payload. It is the email the caller sent, so a before handler that
// rewrites the payload's email does not move the attempt to another count.
// A call without an email leaves no entry, and removes the one inherited
// from an enclosing operation, so a rule per email does not apply to it.
func publishEmail(hctx *core.HookContext, email string) {
	if email = store.NormalizeEmail(email); email == "" {
		delete(hctx.Values, hooks.HookValueEmail)
		return
	}
	hctx.Values[hooks.HookValueEmail] = email
}

// operation builds the HookContext one flow call runs with. Its Values start
// as a copy of the operation ctx already belongs to, if any, and Request is
// the request ctx carries, or nil.
func (p *Plugin) operation(ctx context.Context, op string) (*core.HookContext, error) {
	if p.authContext == nil {
		return nil, errs.NewConfigurationError(op, "the plugin is not initialized; pass it to Prepare and Boot first", nil)
	}
	return &core.HookContext{
		Ctx:    ctx,
		Auth:   p.authContext,
		Values: core.NestedHookValues(ctx),
	}, nil
}

// SignOut ends the session the caller on ctx signed in with. It fires
// auth.signOut.before and .after around the revoke, as one operation: both
// points share Values, and the session manager's revoke points start from a
// copy of them.
//
// The caller needs PermissionSignOut (Access.RequireUser) and a session. A
// caller that is not signed in gets the reason as a session error, as from
// RequireSession, and a system caller acting for a user, which has no
// session, gets errs.CodeSessionMissing. Ending another client's session is
// not a self-service flow: code that has to does it with
// SessionManager.Revoke.
//
// It is the flow behind POST /sign-out, and writes nothing to a response. A
// route that signs its caller out takes the token back afterwards
// (TokenCarrier.Clear), as the plugin's sign-out route does.
func (p *Plugin) SignOut(ctx context.Context) error {
	const op = "emailpassword.SignOut"
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return err
	}
	if _, err := p.authContext.Access.RequireUser(ctx, PermissionSignOut); err != nil {
		return err
	}
	session, ok := identity.CallerFrom(ctx).Session()
	if !ok {
		return errs.NewSessionError(op, errs.CodeSessionMissing, nil)
	}
	sessionID := session.ID
	hctx = core.AsOperation(hctx)
	hctx.Values[hooks.HookValueSessionID] = sessionID // for the operation's later points and its audit event
	ac := hctx.Auth
	// The returned payload is not read: a handler can stop the sign-out, but
	// it can't point it at another session.
	if _, err := ac.Dispatcher.RunBefore(hctx, hooks.HookSignOutBefore, behemoth.M{"sessionID": sessionID}); err != nil {
		return err
	}
	if err := ac.SessionManager.Revoke(hctx.Ctx, sessionID, "user_logout"); err != nil {
		return err
	}
	return ac.Dispatcher.RunAfter(hctx, hooks.HookSignOutAfter, nil)
}

// handleSignUp returns the flow's error unchanged, like handleSignIn, and
// the router maps and logs it (RouterConfig.ErrorMapper). The flow's own
// rejections are typed: a validation error (400) for the email or the
// password, a conflict (409) for an email that is taken. A hook handler's
// typed rejection keeps its own status. Anything else is a failure of the
// system: the router answers 500 without the error's text and logs it.
func (p *Plugin) handleSignUp(rctx *httpapi.RequestContext) error {
	var body behemoth.M
	if err := json.NewDecoder(rctx.Request.Body).Decode(&body); err != nil {
		return errs.NewValidationError("emailpassword.SignUp", "request", err)
	}

	user, err := p.SignUp(rctx.Ctx, body)
	if err != nil {
		return err
	}
	// The user's public columns, under the key a sign-in answers with. A
	// sign-up creates no session, so there is no token to deliver.
	view, err := rctx.Auth.Public.Of(user)
	if err != nil {
		return err
	}
	return rctx.Response.JSON(http.StatusCreated, behemoth.M{httpapi.SignInUserKey: view})
}

// handleSignIn returns the flow's error unchanged, and the router maps and
// logs it (RouterConfig.ErrorMapper). A refused credential is a typed
// unauthorized error (401), and a hook handler's typed rejection keeps its
// own status. Anything else is a failure of the system: the router answers
// 500 without the error's text and logs it at Error.
func (p *Plugin) handleSignIn(rctx *httpapi.RequestContext) error {
	// The body is decoded as a map so that fields other than the email and
	// password reach the before handlers, in creds.Extra.
	var body behemoth.M
	if err := json.NewDecoder(rctx.Request.Body).Decode(&body); err != nil {
		return errs.NewValidationError("SignIn", "request", err)
	}
	var creds EmailAndPasswordCredentials
	if err := creds.FromMap(body); err != nil {
		return err
	}

	result, err := p.SignIn(rctx.Ctx, creds)
	if err != nil {
		return err
	}

	// The body every sign-in route answers with, and the session token the
	// way RouterConfig.Carrier says.
	return httpapi.WriteSignIn(rctx, result, nil)
}

// handleSignOut returns SignOut's error unchanged, like handleSignIn. A typed
// error keeps its status (a hook handler's rejection, a session that no
// longer exists). A failed revoke is a 500 from the router, which does not
// send the error's text. After a sign-out that went through, it takes the
// session token back from the client.
func (p *Plugin) handleSignOut(rctx *httpapi.RequestContext) error {
	if err := p.SignOut(rctx.Ctx); err != nil {
		return err
	}
	// The session is the caller's own and it has ended, so the client gives
	// its token up: with a cookie delivery the response removes the cookie.
	// A refused sign-out returned above and leaves the token in place, since
	// its session is still live.
	rctx.TokenCarrier.Clear(rctx)
	return rctx.Response.JSON(http.StatusOK, behemoth.M{"status": "signed_out"})
}

var _ httpapi.Plugin = (*Plugin)(nil)
