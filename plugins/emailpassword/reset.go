package emailpassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/utils"
)

// Password reset has two flows. RequestPasswordReset generates a single-use token
// and sends the reset link through the application's mailer (AuthContext.Mailer).
// ResetPassword validates the token, updates the user's password, invalidates their existing sessions
// and sends a confirmation notice.
// Password reset is disabled until Options.Reset.LinkURL is configured.

// The flow is similar to the magic link plugin's request and verify flows.
// The key difference is the outcome: magic links create a session, while
// password reset only updates the password without creating a new session.

// DefaultResetTTL defines how long a reset link remains valid when ResetOptions.TTL is zero.
const DefaultResetTTL = time.Hour

// The points the plugin declares and fires around a password reset, when
// reset is enabled.
const (

	// HookResetRequestBefore runs before a reset request, allowing handlers
	// to modify the "email" and "metadata" inputs or stop the request.
	HookResetRequestBefore core.HookPoint = "auth.passwordReset.beforeRequest"

	// HookResetRequestAfter runs after the reset link is passed to the mail
	// sender, providing the *ResetRequestResult.
	HookResetRequestAfter core.HookPoint = "auth.passwordReset.afterRequest"

	// HookResetRequestFailed runs when no reset link is sent due to
	// "user_not_found", "issue_failed", "send_failed", or "rejected_by_hook".
	HookResetRequestFailed core.HookPoint = "auth.passwordReset.requestFailed"

	// HookResetBefore runs before a token is consumed, providing the "password"
	// and, if available, "email". Handlers can stop the reset, but payload changes
	// are not applied.
	HookResetBefore core.HookPoint = "auth.passwordReset.before"

	// HookResetAfter runs after the password is updated and all user sessions
	// are ended, providing the *models.User.
	HookResetAfter core.HookPoint = "auth.passwordReset.after"

	// HookResetFailed runs when a password reset fails due to "invalid_token",
	// "user_not_found", "email_changed", or "rejected_by_hook".
	HookResetFailed core.HookPoint = "auth.passwordReset.failed"
)

// Names of the rate-limit rules the plugin declares when reset is enabled.
const (
	RuleResetRequestAddress = "emailpassword.reset.request.address" // reset requests per client address
	RuleResetConfirmAddress = "emailpassword.reset.confirm.address" // reset attempts per client address
	RuleResetRequestEmail   = "emailpassword.reset.request.email"   // reset requests per email, from any caller
)

// Paths of the reset routes, below the router's base path.
const (
	PathResetRequest = "/password-reset/request"
	PathResetConfirm = "/password-reset/confirm"
)

// Keys of the reset flows' hook payloads and request bodies.
const (
	resetMetadataKey = "metadata"
	resetTokenKey    = "token"
)

// ErrorCodeInvalidResetToken is returned by ResetPassword when a reset link
// is invalid, expired, already used, superseded, or belongs to a user who no
// longer exists or has changed their email.
const ErrorCodeInvalidResetToken = "invalid_reset_token"

// SessionRevokedPasswordReset is the reason given when a password reset
// revokes a user's sessions.
const SessionRevokedPasswordReset = "password_reset"

// ErrNoAccount is returned by RequestPasswordReset when no account matches
// the given email. The route returns the same response for every email to
// prevent revealing which addresses have registered accounts.
var ErrNoAccount = errors.New("emailpassword: no account with this email")

// ResetOptions configures password reset. Reset is disabled when LinkURL is
// empty; no reset routes are registered, and no mail sender is required.
type ResetOptions struct {

	// LinkURL is the application page users are sent to when resetting
	// their password, such as "https://app.example.com/auth/reset".
	// Setting it enables password reset. Must be an absolute HTTP or HTTPS
	// URL. The plugin appends the token as a "token" query parameter.
	// The page collects the new password and submits it to the confirm route.
	LinkURL string

	// TTL is how long a reset link remains valid. Zero uses DefaultResetTTL.
	// TokenConfig.TTLOverrides for core.TokenKindPasswordReset takes precedence.
	TTL time.Duration

	// RequestLimit limits reset requests per email address, regardless of
	// caller. This prevents repeated emails to the same address. Zero allows
	// 5 requests per 15 minutes.
	RequestLimit core.Limit

	// WaitForSend controls whether the request waits for the mail sender.
	//
	// false (default): The message is queued using Mailer.SendAsync, and the
	// request returns immediately. Send failures are logged by the mailer,
	// and the user must try again. This keeps mail delivery time out of the
	// response, preventing account enumeration through timing differences.
	//
	// true: The request waits for Mailer.Send to finish. If sending fails,
	// the reset link is revoked and auth.passwordReset.requestFailed fires.
	WaitForSend bool

	// LeaveEmailUnverified prevents password resets from marking the user's
	// email as verified. By default, a successful reset verifies an
	// unverified email because following the link demonstrates control
	// of that address. Set this when EmailVerified must only reflect
	// verification through the application's own process.
	LeaveEmailUnverified bool
}

// enabled reports whether password reset is enabled.
func (o ResetOptions) enabled() bool { return o.LinkURL != "" }

// ResetRequest holds the input for RequestPasswordReset.
//
// It implements [behemoth.Serializable], allowing WithLifecycle to convert
// it to and from the auth.passwordReset.beforeRequest hook payload.
type ResetRequest struct {
	// Email is the address of the account whose password is being reset.
	Email string

	// Metadata is passed unchanged to MailMessage.Metadata, such as a
	// locale or template name. It is optional and is neither read nor
	// stored by the plugin.
	Metadata behemoth.M
}

// ToMap converts the request into a hook payload.
func (r *ResetRequest) ToMap() (map[string]any, error) {
	m := map[string]any{credentialEmailKey: r.Email}
	if r.Metadata != nil {
		m[resetMetadataKey] = r.Metadata
	}
	return m, nil
}

// FromMap populates the request from the input body or a modified hook
// payload. A missing email remains empty and is rejected by the flow.
// Values of the wrong type return a validation error.
func (r *ResetRequest) FromMap(data map[string]any) error {
	const op = "emailpassword.RequestPasswordReset"
	var email string
	switch v := data[credentialEmailKey].(type) {
	case nil:
	case string:
		email = v
	default:
		return errs.NewInvalidInputError(op, credentialEmailKey, "email must be a string", nil)
	}
	var metadata behemoth.M
	switch v := data[resetMetadataKey].(type) {
	case nil:
	case behemoth.M: // a payload a handler built, or a caller's request
		metadata = v
	case map[string]any: // a decoded request body
		metadata = v
	default:
		return errs.NewInvalidInputError(op, resetMetadataKey, "metadata must be an object", nil)
	}
	r.Email, r.Metadata = email, metadata
	return nil
}

// ResetRequestResult is returned by RequestPasswordReset and passed to
// auth.passwordReset.afterRequest handlers. It contains no reset token. type ResetRequestResult struct
type ResetRequestResult struct {
	UserID    string
	Email     string
	TokenID   string
	ExpiresAt time.Time
}

// AuditSubject implements [core.AuditSubject], identifying the user whose
// password is being reset.
func (r *ResetRequestResult) AuditSubject() (subjectType, subjectID string) {
	if r == nil {
		return "", ""
	}
	return models.UserTable, r.UserID
}

// resetInput is the input of the wrapped reset flow. Its payload on
// auth.passwordReset.before is "password" and, when the token names one,
// "email". The token is not in the payload, and nothing is read back from
// it: a handler can stop the reset, but it can't point it at another link
// or swap the password.
type resetInput struct {
	token    string
	email    string
	password string

	// What ResetPassword's read of the token gave: the token when it is
	// usable, and otherwise the reason it is not. The flow checks the user
	// against verified before it consumes the token.
	verified  *models.Token
	verifyErr error
}

func (in *resetInput) ToMap() (map[string]any, error) {
	m := map[string]any{credentialPasswordKey: in.password}
	if in.email != "" {
		m[credentialEmailKey] = in.email
	}
	return m, nil
}

func (in *resetInput) FromMap(map[string]any) error { return nil }

// declareReset registers the password reset token kind, lifecycle hooks (auth.passwordReset.*),
// and rate-limit rules. Called by Declare when password reset is enabled.
func (p *Plugin) declareReset(ic *core.PluginInitContext) error {

	// Store tokens in the database so new links can revoke previous ones
	// and deleting a user also removes their outstanding links.
	if err := ic.Tokens.Declare(core.TokenKindDef{
		Kind: core.TokenKindPasswordReset, SingleUse: true, DefaultTTL: p.opts.Reset.TTL, Backend: core.TokenBackendDB,
	}); err != nil {
		return err
	}

	for _, def := range []core.HookPointDef{
		{Point: HookResetRequestBefore, Phase: core.BeforeHookPhase},
		{Point: HookResetRequestAfter, Phase: core.AfterHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookResetRequestFailed, Phase: core.FailedHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookResetBefore, Phase: core.BeforeHookPhase},
		{Point: HookResetAfter, Phase: core.AfterHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookResetFailed, Phase: core.FailedHookPhase, Audit: &core.AuditSpec{}},
	} {
		if err := ic.Hooks.Declare(def); err != nil {
			return err
		}
	}

	// Limit requests per client address, following the core sign-in rule.
	// This slows individual clients without allowing them to block others.
	// Also limit requests per email to prevent mailbox flooding. Unknown
	// addresses count too, keeping responses consistent and preventing
	// account enumeration. All limits run on the flows' before hooks,
	// regardless of which transport invokes them.
	perAddress := core.Limit{Max: 10, Window: time.Minute}
	for _, rule := range []core.HookRateLimitRule{
		{Name: RuleResetRequestAddress, Point: HookResetRequestBefore, KeyFunc: core.KeyByClientIP(), Limit: perAddress},
		{Name: RuleResetConfirmAddress, Point: HookResetBefore, KeyFunc: core.KeyByClientIP(), Limit: perAddress},
		{Name: RuleResetRequestEmail, Point: HookResetRequestBefore, KeyFunc: core.KeyByValues(hooks.HookValueEmail), Limit: p.opts.Reset.RequestLimit},
	} {
		if err := ic.RateLimits.DeclareHookRateLimitRule(rule); err != nil {
			return err
		}
	}
	return nil
}

// initReset checks the reset options and wraps the two flows with their
// points. Init calls it when reset is enabled.
func (p *Plugin) initReset(ac *core.AuthContext) error {
	const op = "emailpassword.Init"
	if ac.Mailer == nil || !ac.Mailer.Configured() {
		return errs.NewConfigurationError(op, "password reset is enabled and no mail sender is configured; set BootConfig.Mail.Sender: the plugin does not send email itself", nil)
	}
	if ac.TokenManager == nil {
		return errs.NewConfigurationError(op, "password reset is enabled and AuthContext has no token manager", nil)
	}
	if p.opts.Reset.TTL < 0 {
		return errs.NewConfigurationError(op, fmt.Sprintf("Options.Reset.TTL is negative: %s", p.opts.Reset.TTL), nil)
	}
	linkURL, err := url.Parse(p.opts.Reset.LinkURL)
	if err != nil || (linkURL.Scheme != "http" && linkURL.Scheme != "https") || linkURL.Host == "" {
		return errs.NewConfigurationError(op,
			fmt.Sprintf("Options.Reset.LinkURL %q is not an absolute http or https URL", p.opts.Reset.LinkURL), err)
	}
	p.resetURL = linkURL
	p.resetRequest = core.WithLifecycle(ac.Dispatcher, HookResetRequestBefore, HookResetRequestAfter, HookResetRequestFailed, p.resetRequestBody)
	p.reset = core.WithLifecycle(ac.Dispatcher, HookResetBefore, HookResetAfter, HookResetFailed, p.resetBody)
	return nil
}

// resetOperation identifies a password reset operation and rejects it
// when password reset is disabled.
func (p *Plugin) resetOperation(ctx context.Context, op string) (*core.HookContext, error) {
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return nil, err
	}
	if !p.opts.Reset.enabled() {
		return nil, errs.NewConfigurationError(op, "password reset is not enabled; set Options.Reset.LinkURL", nil)
	}
	return hctx, nil
}

// resetAfterLookupError represents a failure after finding the user, such
// as token issuance or email delivery failure. Since it only occurs for
// existing accounts, the route handles it like ErrNoAccount to avoid
// revealing whether an email is registered. Direct callers receive this
// error and the underlying cause.
type resetAfterLookupError struct{ err error }

func (e *resetAfterLookupError) Error() string { return e.err.Error() }
func (e *resetAfterLookupError) Unwrap() error { return e.err }

// RequestPasswordReset sends a password reset link to the address in req.Email.
// Issuing a new link invalidates any earlier reset links for the user. The
// password and active sessions remain unchanged until the link is used.
// Fires auth.passwordReset.beforeRequest, .afterRequest, and .requestFailed.
//
// Users without a password, such as those who signed up through a magic link
// or external provider, can also request a link. Using it sets their first
// password.
//
// Returns ErrNoAccount if no user has the given email, without sending a
// message. An invalid email format returns a validation error.
//
// This is the programmatic equivalent of POST /password-reset/request.
// Unlike the route, it exposes ErrNoAccount and email delivery failures.
// The plugin must be initialized through Boot with password reset enabled.
func (p *Plugin) RequestPasswordReset(ctx context.Context, req ResetRequest) (*ResetRequestResult, error) {
	hctx, err := p.resetOperation(ctx, "emailpassword.RequestPasswordReset")
	if err != nil {
		return nil, err
	}
	publishEmail(hctx, req.Email)
	return p.resetRequest(hctx, req)
}

func (p *Plugin) resetRequestBody(hctx *core.HookContext, req ResetRequest) (*ResetRequestResult, error) {
	const op = "emailpassword.RequestPasswordReset"
	ac := hctx.Auth

	// Validate the email format before looking up the user, so the result
	// does not reveal whether an account exists. Only check its shape:
	// Options.ValidateEmail applies to new accounts, while existing accounts
	// created under older rules must still be able to reset their passwords.
	email := store.NormalizeEmail(req.Email)
	if !utils.IsValidEmail(email) {
		return nil, errs.NewInvalidInputError(op, credentialEmailKey, "invalid email", nil)
	}

	user, err := ac.Store.FindUserByEmail(hctx.Ctx, email)
	if err != nil {
		if !errs.IsNotFound(err) {
			return nil, err // infra error (DB down): not "no such user"
		}
		if failErr := ac.Dispatcher.Fail(hctx, HookResetRequestFailed, core.FailureReason{
			Code: "user_not_found", Metadata: behemoth.M{hooks.HookValueEmail: email},
		}); failErr != nil {
			return nil, failErr
		}
		return nil, ErrNoAccount
	}

	// From here on a failure is one of a known account, and its audit event
	// says which.
	failed := func(code string, cause error) error {
		if failErr := ac.Dispatcher.Fail(hctx, HookResetRequestFailed, core.FailureReason{
			Code: code, Cause: cause, SubjectType: models.UserTable, SubjectID: user.ID,
		}); failErr != nil {
			return failErr
		}
		return &resetAfterLookupError{err: cause}
	}

	// One live link per user. The subject is the user's id, not the
	// email, so Store.DeleteUser removes the links with the user. The email
	// the link was sent to is kept next to it and checked at the reset.
	if err := ac.TokenManager.RevokeAllForSubject(hctx.Ctx, core.TokenKindPasswordReset, user.ID); err != nil {
		return nil, failed("issue_failed", err)
	}
	token, rawToken, err := ac.TokenManager.Issue(hctx.Ctx, core.TokenKindPasswordReset, user.ID, behemoth.M{credentialEmailKey: email})
	if err != nil {
		return nil, failed("issue_failed", err)
	}

	link := *p.resetURL
	query := link.Query()
	query.Set(resetTokenKey, rawToken)
	link.RawQuery = query.Encode()

	msg := core.MailMessage{
		Kind:      core.MailPasswordReset,
		To:        email,
		URL:       link.String(),
		Token:     rawToken,
		ExpiresAt: token.ExpiresAt,
		User:      user,
		Metadata:  req.Metadata,
	}
	if p.opts.Reset.WaitForSend {
		err = ac.Mailer.Send(hctx.Ctx, msg)
	} else {
		err = ac.Mailer.SendAsync(hctx.Ctx, msg) // an error here means it was not queued
	}
	if err != nil {
		// A link not sent successfully should not stay valid.
		if revokeErr := ac.TokenManager.Revoke(hctx.Ctx, token.ID); revokeErr != nil {
			p.log.Warn(hctx.Ctx, "the reset link that could not be sent was not revoked", telemetry.ErrorFields(revokeErr))
		}
		return nil, failed("send_failed", fmt.Errorf("emailpassword: send reset link: %w", err))
	}

	return &ResetRequestResult{UserID: user.ID, Email: email, TokenID: token.ID, ExpiresAt: token.ExpiresAt}, nil
}

// errInvalidResetToken returns the same unauthorized error for every
// rejected reset link, preventing callers from distinguishing expired,
// used, or nonexistent tokens. Returns HTTP 401.
func errInvalidResetToken() error {
	return errs.NewUnauthorized("emailpassword.ResetPassword", ErrorCodeInvalidResetToken, "invalid or expired link")
}

// isTokenRejection reports whether the token was rejected because it is
// missing, expired, used, or revoked, rather than due to a system error.
func isTokenRejection(err error) bool {
	return errs.IsCategory(err, errs.CategoryToken) || errs.IsNotFound(err)
}

// ResetPassword sets a new password for the account associated with a reset
// link and returns the updated user. rawToken is the link's "token" query
// parameter.
//
// The new password is validated against the same rules used at sign-up
// (Options). Validation happens before the token is consumed, so a rejected
// password leaves the link usable. The token is consumed only after the user
// and password hash are ready, immediately before the database write. This
// ensures the link is only spent without completing the reset if the write
// itself fails.
//
// On success:
//   - The user's credential account receives the new password hash. If the
//     user had no password, a credential account is created.
//   - All existing sessions are revoked with SessionRevokedPasswordReset.
//     No new session is created, requiring the user to sign in again and
//     preserving any second-factor requirements.
//   - The email is marked as verified if it was not already, unless
//     ResetOptions.LeaveEmailUnverified is enabled.
//   - A core.MailPasswordChanged notification is sent in the background.
//
// Unknown, expired, used, or superseded links all return the same error
// with ErrorCodeInvalidResetToken. The same applies if the associated user
// no longer exists or has changed their email since the link was issued.
//
// Fires auth.passwordReset.before, .after, and .failed.
// This is the flow behind POST /password-reset/confirm.
func (p *Plugin) ResetPassword(ctx context.Context, rawToken, newPassword string) (*models.User, error) {
	hctx, err := p.resetOperation(ctx, "emailpassword.ResetPassword")
	if err != nil {
		return nil, err
	}

	// Verify the token without consuming it. The before hook needs its email
	// for the payload and rate-limit rules, and the flow uses it to check
	// the associated user. Rejected tokens still go through the flow so it
	// can handle the failure and fire auth.passwordReset.failed.
	in := resetInput{token: rawToken, password: newPassword}
	token, err := p.authContext.TokenManager.Verify(ctx, core.TokenKindPasswordReset, rawToken)
	switch {
	case err == nil:
		in.verified = token
		in.email, _ = token.MetadataJSON[credentialEmailKey].(string)
	case !isTokenRejection(err):
		return nil, err
	default:
		in.verifyErr = err
	}
	publishEmail(hctx, in.email)
	return p.reset(hctx, in)
}

func (p *Plugin) resetBody(hctx *core.HookContext, in resetInput) (*models.User, error) {
	const op = "emailpassword.ResetPassword"
	ac := hctx.Auth
	dispatcher := ac.Dispatcher

	// Validate the password before checking the token. This ensures a
	// password typo does not consume the reset link, and the result does
	// not reveal anything about the token.
	if err := p.validatePassword(in.password); err != nil {
		return nil, errInvalidPassword(op, err)
	}

	// Complete every check that can reject the reset before consuming
	// the token. Only the database write remains afterward.
	token := in.verified
	if token == nil {
		if failErr := dispatcher.Fail(hctx, HookResetFailed, core.FailureReason{Code: "invalid_token", Cause: in.verifyErr}); failErr != nil {
			return nil, failErr
		}
		return nil, errInvalidResetToken()
	}

	userID := fmt.Sprint(token.Subject)
	rejected := func(code string, cause error) core.FailureReason {
		return core.FailureReason{Code: code, Cause: cause, SubjectType: models.UserTable, SubjectID: userID}
	}

	user, err := ac.Store.FindUserByID(hctx.Ctx, userID)
	if err != nil {
		if !errs.IsNotFound(err) {
			return nil, err
		}
		if failErr := dispatcher.Fail(hctx, HookResetFailed, rejected("user_not_found", nil)); failErr != nil {
			return nil, failErr
		}
		return nil, errInvalidResetToken()
	}

	// The link proves control of the address it was sent to. Reject it
	// if the user's email has changed since the link was issued.
	sentTo, _ := token.MetadataJSON[credentialEmailKey].(string)
	if sentTo == "" || sentTo != store.NormalizeEmail(user.Email) {
		if failErr := dispatcher.Fail(hctx, HookResetFailed, rejected("email_changed", nil)); failErr != nil {
			return nil, failErr
		}
		return nil, errInvalidResetToken()
	}

	// Hash the password only after verifying the token, preventing
	// unnecessary hashing for invalid links. Hashing also happens before
	// consumption, so a failure leaves the link usable.
	passwordHash, err := hashPassword(hctx, in.password)
	if err != nil {
		return nil, err // infra error - no Fail()
	}

	// Consume the token to ensure it can only be used once. If multiple
	// resets race with the same link, only one can succeed.
	if _, err := ac.TokenManager.Consume(hctx.Ctx, core.TokenKindPasswordReset, in.token); err != nil {
		if !isTokenRejection(err) {
			return nil, err // infra error: not a bad link
		}
		if failErr := dispatcher.Fail(hctx, HookResetFailed, rejected("invalid_token", err)); failErr != nil {
			return nil, failErr
		}
		return nil, errInvalidResetToken()
	}

	// Update the password and email verification status in one transaction.
	// Token consumption happens outside the transaction, so a failed write
	// leaves the link spent and the user must request another.
	markVerified := !user.EmailVerified && !p.opts.Reset.LeaveEmailUnverified
	err = ac.Store.Transaction(hctx.Ctx, func(ctx context.Context, tx *store.Store) error {
		if err := storePasswordHash(ctx, tx, user.ID, passwordHash); err != nil || !markVerified {
			return err
		}
		updated, err := tx.UpdateUser(ctx, user.ID, behemoth.M{models.UserEmailVerified: true})
		if err != nil {
			return err
		}
		user = updated
		return nil
	})
	if err != nil {
		// A data hook's typed veto is a refusal of the reset. Anything else
		// is a failure of the system.
		return nil, core.FailIfRejected(dispatcher, hctx, HookResetFailed, err,
			core.FailureReason{SubjectType: models.UserTable, SubjectID: userID})
	}

	if err := p.afterPasswordChange(hctx.Ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// storePasswordHash saves hash as the user's password, updating their
// existing credential account or creating one if they signed up through
// a magic link or external provider.
func storePasswordHash(ctx context.Context, tx *store.Store, userID, hash string) error {
	credential, err := tx.FindAccount(ctx, models.ProviderCredential, userID)
	switch {
	case err == nil:
		_, err = tx.UpdateAccount(ctx, credential.ID, behemoth.M{models.AccountPasswordHash: hash})
	case errs.IsNotFound(err):
		err = tx.CreateAccount(ctx, &models.Account{
			UserID:       userID,
			ProviderID:   models.ProviderCredential,
			AccountID:    userID,
			PasswordHash: hash,
		})
	}
	return err
}

// afterPasswordChange handles the steps that follow a password change
// outside sign-up, once the new password has been saved.
//
// All existing sessions are revoked so anyone using the old password or
// an existing session must sign in again. If revocation fails, the password
// is still changed and the caller receives an error.
//
// The user is then notified in the background in case the change was
// unauthorized. Notification failures do not undo the password change or
// affect the response. Failed queue attempts are logged. No notice is sent
// when the mailer is unavailable or the user has no email address.
func (p *Plugin) afterPasswordChange(ctx context.Context, user *models.User) error {
	ac := p.authContext
	if err := ac.SessionManager.RevokeAllForUser(ctx, user.ID, SessionRevokedPasswordReset, ""); err != nil {
		return fmt.Errorf("emailpassword: the password was set, but ending the user's sessions failed: %w", err)
	}
	if ac.Mailer == nil || !ac.Mailer.Configured() || user.Email == "" {
		return nil
	}
	notice := core.MailMessage{Kind: core.MailPasswordChanged, To: store.NormalizeEmail(user.Email), User: user}
	if err := ac.Mailer.SendAsync(ctx, notice); err != nil {
		p.log.Warn(ctx, "the password changed notice was not queued", telemetry.ErrorFields(err))
	}
	return nil
}

// ValidatePassword checks a new password against the plugin's configured
// rules: Options.MinPasswordLength, MaxPasswordLength, and ValidatePassword.
// It returns the same validation error used during sign-up, with code
// ErrorCodeInvalidPassword.
//
// Plugins that set passwords through SetPassword can call this first to
// validate the password before consuming resources that cannot be recovered
// if a later step fails, such as a one-time code.
func (p *Plugin) ValidatePassword(password string) error {
	if err := p.validatePassword(password); err != nil {
		return errInvalidPassword("emailpassword.ValidatePassword", err)
	}
	return nil
}

// SetPassword sets a user's password and returns the updated user.
//
// This is an administrative operation requiring PermissionSetPassword
// through Access.RequireAdmin. Currently, only system callers have this
// permission, so application code must identify itself using
// identity.System(name). Anonymous callers receive a session error with
// the reason, while signed-in users receive a forbidden error with
// errs.CodeInsufficientScope.
//
// This performs the same password update as ResetPassword, but without
// requiring a reset link. The password is validated against the plugin's
// rules, and a credential account is created if the user does not have one.
// All existing sessions are revoked with SessionRevokedPasswordReset, and
// no new session is created. A core.MailPasswordChanged notice is sent when
// a mail sender is configured.
//
// Unlike ResetPassword, this does not change EmailVerified or fire any
// lifecycle hooks; the calling flow is responsible for its own hooks.
//
// Works independently of email password reset. The plugin must be
// initialized through Boot.
func (p *Plugin) SetPassword(ctx context.Context, userID, password string) (*models.User, error) {
	const op = "emailpassword.SetPassword"
	if p.authContext == nil {
		return nil, errs.NewConfigurationError(op, "the plugin is not initialized; pass it to Prepare and Boot first", nil)
	}
	if err := p.authContext.Access.RequireAdmin(ctx, PermissionSetPassword); err != nil {
		return nil, err
	}
	return p.setPassword(ctx, op, userID, password)
}

// SetPasswordUnchecked sets a user's password without checking the caller's
// permissions. It is intended for plugins that have already verified their
// own authorization, such as confirming a code sent to the user's phone
// before allowing an anonymous caller to set a password.
//
// The original caller in ctx is preserved, so audit events and rate limits
// continue to reflect the actual client.
//
// Application code should use SetPassword instead. Exposing this method
// through a route that accepts an arbitrary user ID would let anyone change
// another user's password.
func (p *Plugin) SetPasswordUnchecked(ctx context.Context, userID, password string) (*models.User, error) {
	return p.setPassword(ctx, "emailpassword.SetPasswordUnchecked", userID, password)
}

// setPassword is SetPassword after the check.
func (p *Plugin) setPassword(ctx context.Context, op, userID, password string) (*models.User, error) {
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return nil, err
	}
	if err := p.validatePassword(password); err != nil {
		return nil, errInvalidPassword(op, err)
	}
	ac := p.authContext
	user, err := ac.Store.FindUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	passwordHash, err := hashPassword(hctx, password)
	if err != nil {
		return nil, err
	}
	if err := ac.Store.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		return storePasswordHash(ctx, tx, user.ID, passwordHash)
	}); err != nil {
		return nil, err
	}
	if err := p.afterPasswordChange(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// handleResetRequest returns the same response for every well-formed
// request, regardless of whether the email belongs to an account. Unknown
// emails, token issuance failures, and send failures all return HTTP 200.
// Validation and rate-limit errors are reported because they do not reveal
// whether an account exists. Other errors indicate a system failure and
// are handled by the router.
func (p *Plugin) handleResetRequest(rctx *httpapi.RequestContext) error {
	var body behemoth.M
	if err := json.NewDecoder(rctx.Request.Body).Decode(&body); err != nil {
		return errs.NewValidationError("emailpassword.RequestPasswordReset", "request", err)
	}
	var req ResetRequest
	if err := req.FromMap(body); err != nil {
		return err
	}

	_, err := p.RequestPasswordReset(rctx.Ctx, req)
	if hidden, ok := errors.AsType[*resetAfterLookupError](err); ok {
		// Nothing tells the client, so this line is the only report.
		p.log.Error(rctx.Ctx, "password reset link was not sent", telemetry.ErrorFields(hidden.err))
		err = nil
	}
	if err != nil && !errors.Is(err, ErrNoAccount) {
		return err
	}
	return rctx.Response.JSON(http.StatusOK, behemoth.M{"status": "ok"})
}

// handleResetConfirm returns errors from ResetPassword unchanged, allowing
// the router to map them to the appropriate status codes. Invalid or
// unusable links return 401, while password validation errors return 400.
// No session token is issued because resetting a password does not sign
// the user in.
func (p *Plugin) handleResetConfirm(rctx *httpapi.RequestContext) error {
	const op = "emailpassword.ResetPassword"
	var body behemoth.M
	if err := json.NewDecoder(rctx.Request.Body).Decode(&body); err != nil {
		return errs.NewValidationError(op, "request", err)
	}
	rawToken, _ := body[resetTokenKey].(string)
	if rawToken == "" {
		return errs.NewInvalidInputError(op, resetTokenKey, "token is required", nil)
	}
	password, ok := body[credentialPasswordKey].(string)
	if !ok {
		return errs.NewInvalidInputError(op, credentialPasswordKey, "password is required", nil)
	}

	if _, err := p.ResetPassword(rctx.Ctx, rawToken, password); err != nil {
		return err
	}
	return rctx.Response.JSON(http.StatusOK, behemoth.M{"status": "password_reset"})
}
