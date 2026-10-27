package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/utils"
)

// HookValueProvider is the key under which Complete publishes the id of the
// provider a sign-in names, in HookContext.Values. Handlers on the
// auth.signIn.* and auth.signUp.* points read it to learn which provider a
// sign-in with the Method "oauth" came through.
const HookValueProvider = "oauth_provider"

// Codes of the errors Complete returns for its own rejections. A client
// tells them apart by the code, not by the message.
const (
	// ErrorCodeInvalidCallback is the code of the unauthorized error (401)
	// for a callback Complete refuses: a state that is unknown, expired or
	// already used, a binding value that does not match, a provider other
	// than the one the sign-in was started with, a link completed in
	// another session than the one that started it, and a code the
	// provider refused. The user starts the sign-in or the link again.
	ErrorCodeInvalidCallback = "invalid_oauth_callback"
	// ErrorCodeAccountNotLinked is the code of the conflict error (409) for
	// a sign-in whose email belongs to a user who has no account at the
	// provider. The user signs in the way they did before, and links the
	// provider from there.
	ErrorCodeAccountNotLinked = "account_not_linked"
	// ErrorCodeEmailNotProvided is the code of the forbidden error (403)
	// for a first sign-in for which the provider returned no usable email.
	ErrorCodeEmailNotProvided = "email_not_provided"
	// ErrorCodeProviderAlreadyLinked is the code of the conflict error
	// (409) for a link to a provider the user has an account at already. A
	// user has at most one account per provider: unlink it first.
	ErrorCodeProviderAlreadyLinked = "provider_already_linked"
	// ErrorCodeProviderAccountTaken is the code of the conflict error (409)
	// for a link of a provider account that another user has.
	ErrorCodeProviderAccountTaken = "provider_account_taken"
	// ErrorCodeProviderNotLinked is the code of the conflict error (409)
	// for an unlink of a provider the user has no account at.
	ErrorCodeProviderNotLinked = "provider_not_linked"
	// ErrorCodeLastSignInMethod is the code of the conflict error (409) for
	// an unlink that would leave the user no way to sign in: no password,
	// and no account at another provider of the plugin.
	ErrorCodeLastSignInMethod = "last_sign_in_method"
	// ErrorCodeProviderTokenUnavailable is the code of the conflict error
	// (409) of AccessToken for an account without a usable token: none was
	// stored, or the access token expired and can't be refreshed. The user
	// signs in with the provider again, which stores new tokens.
	ErrorCodeProviderTokenUnavailable = "provider_token_unavailable"
)

// Keys of the hook payloads of a completed sign-in: auth.signIn.before gets
// "method" and "provider", and auth.signUp.before gets those and the new
// user's columns under their names.
const keyMethod = "method"

// maxUserColumnLength is the length of the users table's email, firstname
// and lastname columns.
const maxUserColumnLength = 255

// CompleteRequest is the input of Complete: what the provider sent to the
// callback page, and what the client kept from Begin.
type CompleteRequest struct {
	// Provider is the id of the provider whose redirect URI the code
	// arrived at: the last segment of the callback page's path.
	Provider string
	// Code is the authorization code, the "code" query parameter.
	Code string
	// State is the "state" query parameter.
	State string
	// Binding is BeginResult.Binding, from where the client kept it. A
	// value that came with the callback's URL does not belong here.
	Binding string
	// Issuer is the "iss" query parameter, when the provider sent one. It
	// is handed to the provider, which checks it (RFC 9207).
	Issuer string
}

// CompleteResult is what Complete returns: the sign-in or the link, and
// where Begin or BeginLink was asked to go afterwards.
type CompleteResult struct {
	// SignInResult is the sign-in, for a state Begin started. It is nil for
	// a link.
	*core.SignInResult
	// Link is the provider account that was linked, for a state BeginLink
	// started. It is nil for a sign-in.
	Link *LinkResult
	// RedirectURL is BeginRequest.RedirectURL, checked against the trusted
	// origins when the sign-in was started. Empty when none was given.
	RedirectURL string
}

// completeInput is the input of the wrapped flow. Its payload on
// auth.signIn.before is "method" and "provider". The code, the state and the
// binding value are not in the payload, and nothing is read back from it: a
// handler can stop the sign-in, but it can't point it at another one.
type completeInput struct {
	req CompleteRequest
	// transaction is the token of the state as it was read before the flow,
	// or nil when the state did not check out.
	transaction *models.Token
}

func (c *completeInput) ToMap() (map[string]any, error) {
	return map[string]any{keyMethod: PluginName, keyProvider: c.req.Provider}, nil
}

func (c *completeInput) FromMap(map[string]any) error { return nil }

// signUpInput is the input of the wrapped sign-up of a new user. Its payload
// on auth.signUp.before is "method", "provider" and the user's columns. As
// with completeInput, nothing is read back: the identity is the provider's.
// The tokens are not in the payload.
type signUpInput struct {
	provider string
	subject  string
	user     models.User
	tokens   Tokens
}

func (s *signUpInput) ToMap() (map[string]any, error) {
	return map[string]any{
		keyMethod:                PluginName,
		keyProvider:              s.provider,
		models.UserEmail:         s.user.Email,
		models.UserEmailVerified: s.user.EmailVerified,
		models.UserFirstname:     s.user.Firstname,
		models.UserLastname:      s.user.Lastname,
		models.UserImageURL:      s.user.ImageUrl,
	}, nil
}

func (s *signUpInput) FromMap(map[string]any) error { return nil }

// errInvalidCallback is Complete's one answer to everything that is wrong
// with what a callback presents, so the response does not tell a used state
// from a wrong binding value. It is typed (unauthorized, 401).
func errInvalidCallback() error {
	return errs.NewUnauthorized("oauth.Complete", ErrorCodeInvalidCallback, "the sign-in could not be completed")
}

func errAccountNotLinked() error {
	return conflict("oauth.Complete", ErrorCodeAccountNotLinked, "an account with this email already exists")
}

func errProviderAlreadyLinked(op string) error {
	return conflict(op, ErrorCodeProviderAlreadyLinked, "an account at this provider is linked already")
}

func errProviderAccountTaken() error {
	return conflict("oauth.Complete", ErrorCodeProviderAccountTaken, "this provider account is linked to another user")
}

func errProviderNotLinked(op string) error {
	return conflict(op, ErrorCodeProviderNotLinked, "no account at this provider is linked")
}

// conflict is a typed conflict error (409) with code.
func conflict(op, code, message string) error {
	return &errs.DomainError{
		Category: errs.CategoryConflict, Op: op, Code: code,
		PublicMessage: message, InternalMessage: op + ": " + message,
	}
}

func errEmailNotProvided() error {
	const op, message = "oauth.Complete", "the provider did not share an email address"
	return &errs.DomainError{
		Category: errs.CategoryForbidden, Op: op, Code: ErrorCodeEmailNotProvided,
		PublicMessage: message, InternalMessage: op + ": " + message,
	}
}

// isTokenRejection reports whether err says the token is not usable (not
// found, expired, consumed, revoked), as opposed to a failure of the system.
func isTokenRejection(err error) bool {
	return errs.IsCategory(err, errs.CategoryToken) || errs.IsNotFound(err)
}

// Complete finishes a sign-in that Begin started, or a link that BeginLink
// started. It checks the state and the binding value, redeems the code at
// the provider, and signs in the user the provider names, or links the
// provider account to the user who started the link. A state works once: it
// is consumed before the code is redeemed, also when a later step refuses
// the sign-in or the link.
//
// For a sign-in, the user is the one who has an account at the provider
// with the subject the provider returns. A subject that is new creates a
// user and the account, with the provider's email, name and picture; the
// email counts as verified only when the provider says it verified it.
// Complete does not find a user by email: when the email of a new subject
// belongs to a user, it refuses with ErrorCodeAccountNotLinked, unless
// Options.LinkByVerifiedEmail lets it link the subject to that user. It
// refuses a new subject without a usable email with
// ErrorCodeEmailNotProvided.
//
// The sign-in fires core's auth.signIn.before, .credentialsVerified, .after
// and .failed, with Method "oauth", auth.signUp.before, .after and .failed
// for a user it creates, and auth.oauth.beforeLink, .afterLink and
// .linkFailed for a link by a verified email. The session is pending
// instead of active when a handler on auth.signIn.credentialsVerified asked
// for a second factor.
//
// A link has to be completed by the user who started it, in the session
// that started it: the caller needs PermissionManage and that session,
// which need not be fresh any more. A caller that is not signed in gets the
// session error of Access.RequireUser, and another session
// ErrorCodeInvalidCallback. A provider account that another user has is a
// conflict with ErrorCodeProviderAccountTaken, and a provider the user has
// an account at already one with ErrorCodeProviderAlreadyLinked. A link
// creates no session, and fires auth.oauth.beforeLink, .afterLink and
// .linkFailed instead of the sign-in points.
//
// A state that is unknown, expired or used, a binding value that does not
// match, another provider than the sign-in was started with, and a code the
// provider refused get one error, with the code ErrorCodeInvalidCallback. A
// state that is unknown or expired can't say whether it was a link's, and
// its refusal fires auth.signIn.failed. A request without a provider, a
// code or a state is a validation error.
//
// Pass the context of the request being handled when there is one. The raw
// session token of a sign-in is in the result and is the caller's to
// deliver. The plugin must have been initialized by Boot.
func (p *Plugin) Complete(ctx context.Context, req CompleteRequest) (*CompleteResult, error) {
	const op = "oauth.Complete"
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return nil, err
	}
	for _, field := range []struct{ key, value string }{
		{keyProvider, req.Provider}, {"code", req.Code}, {"state", req.State},
	} {
		if field.value == "" {
			return nil, errs.NewInvalidInputError(op, field.key, field.key+" is required", nil)
		}
	}

	// Read the state's token without consuming it, for what the checks
	// before the consume need. A state that does not check out still goes
	// through the flow, which refuses it and fires auth.signIn.failed.
	in := completeInput{req: req}
	var redirect string
	token, err := p.authContext.TokenManager.Verify(ctx, core.TokenKindOAuthState, req.State)
	switch {
	case err == nil:
		in.transaction = token
		redirect, _ = token.MetadataJSON[metaRedirectURL].(string)
	case !isTokenRejection(err):
		return nil, err
	}
	hctx.Values[HookValueProvider] = req.Provider

	if in.transaction != nil && intentOf(in.transaction) == IntentLink {
		link, err := p.finishLink(hctx, in)
		if err != nil {
			return nil, err
		}
		return &CompleteResult{Link: link, RedirectURL: redirect}, nil
	}

	// Core's per-address limits count each method apart. The sign-up of a
	// new user inside the flow inherits it.
	hctx.Values[hooks.HookValueMethod] = PluginName
	result, err := p.complete(hctx, in)
	if err != nil {
		return nil, err
	}
	return &CompleteResult{SignInResult: result, RedirectURL: redirect}, nil
}

// intentOf returns the intent a state's token was issued with. A token
// without one is a sign-in's: Begin wrote none before links existed.
func intentOf(token *models.Token) Intent {
	if intent, _ := token.MetadataJSON[metaIntent].(string); intent == string(IntentLink) {
		return IntentLink
	}
	return IntentSignIn
}

// checkedCallback is what the checks before the consume found out about a
// callback: the provider its state names, and the hash of its binding
// value.
type checkedCallback struct {
	provider    Provider
	providerID  string
	bindingHash string
}

// check runs the checks of a callback that need no write. It returns the
// code of the failed point for the first check that fails, or "".
//
// They come before the consume. A request that fails one leaves the state
// usable, so somebody who saw a state in a URL can't end the sign-in it
// belongs to.
func (p *Plugin) check(in completeInput) (checkedCallback, string) {
	if in.transaction == nil {
		return checkedCallback{}, "invalid_oauth_state"
	}
	meta := in.transaction.MetadataJSON
	providerID, _ := meta[metaProvider].(string)
	provider, configured := p.providers[providerID]
	if !configured {
		return checkedCallback{}, "invalid_oauth_state"
	}
	// The code arrived at the redirect URI of in.req.Provider. A sign-in
	// started with another provider would send it to the wrong one.
	if in.req.Provider != providerID {
		return checkedCallback{}, "oauth_provider_mismatch"
	}
	bindingHash, _ := meta[metaBindingHash].(string)
	if in.req.Binding == "" || bindingHash == "" ||
		subtle.ConstantTimeCompare([]byte(utils.LookupHashOf(in.req.Binding)), []byte(bindingHash)) != 1 {
		return checkedCallback{}, "oauth_binding_mismatch"
	}
	return checkedCallback{provider: provider, providerID: providerID, bindingHash: bindingHash}, ""
}

// redeem consumes the state of a checked callback and redeems its code at
// the provider. refuse fires the flow's failed point with a code, and
// returns the error the client gets.
func (p *Plugin) redeem(hctx *core.HookContext, in completeInput, cb checkedCallback, refuse func(code string, cause error) error) (*ExchangeResult, error) {
	ac := hctx.Auth
	// The at-most-once step: of two requests with the same state, one loses
	// here, before either reaches the provider.
	if _, err := ac.TokenManager.Consume(hctx.Ctx, core.TokenKindOAuthState, in.req.State); err != nil {
		if !isTokenRejection(err) {
			return nil, err // infra error: not a bad state
		}
		return nil, refuse("invalid_oauth_state", err)
	}

	meta := in.transaction.MetadataJSON
	sealedVerifier, _ := meta[metaVerifier].(string)
	verifier, err := ac.Store.Open(sealedVerifier, verifierBinding(cb.bindingHash))
	if err != nil {
		return nil, err
	}
	nonce, _ := meta[metaNonce].(string)
	exchanged, err := cb.provider.Exchange(hctx.Ctx, ExchangeRequest{
		Code:         in.req.Code,
		RedirectURI:  RedirectURI(p.opts.CallbackURL, cb.providerID),
		CodeVerifier: verifier,
		Nonce:        nonce,
		Issuer:       in.req.Issuer,
	})
	if err != nil {
		if errors.Is(err, ErrExchangeRefused) {
			return nil, refuse("oauth_exchange_refused", err)
		}
		return nil, fmt.Errorf("oauth: exchange with provider %q: %w", cb.providerID, err)
	}
	if exchanged == nil || exchanged.Identity.Subject == "" {
		// Without a subject there is nothing to key the account on, and an
		// email is not one. This is the provider's mistake, not the user's.
		return nil, fmt.Errorf("oauth: provider %q returned an identity without a subject", cb.providerID)
	}
	return exchanged, nil
}

func (p *Plugin) completeBody(hctx *core.HookContext, in completeInput) (*core.SignInResult, error) {
	ac := hctx.Auth
	dispatcher := ac.Dispatcher
	// refuse fires the failed point with what was wrong, and returns the one
	// error a client gets for all of it.
	refuse := func(code string, cause error) error {
		if failErr := dispatcher.Fail(hctx, hooks.HookSignInFailed, core.FailureReason{
			Code: code, Cause: cause, Metadata: behemoth.M{keyProvider: in.req.Provider},
		}); failErr != nil {
			return failErr
		}
		return errInvalidCallback()
	}

	cb, code := p.check(in)
	if code != "" {
		return nil, refuse(code, nil)
	}
	exchanged, err := p.redeem(hctx, in, cb, refuse)
	if err != nil {
		return nil, err
	}

	user, err := p.resolveUser(hctx, cb.providerID, exchanged)
	if err != nil {
		return nil, err
	}

	// The seam a 2FA plugin hooks, as in the email/password sign-in. The
	// payload names the factor this sign-in checked. See ADR-0009.
	checkpoint, err := dispatcher.RunBefore(hctx, hooks.HookSignInCredentialsVerified,
		behemoth.M{hooks.HookValueUserID: user.ID, hooks.HookValueFactor: core.FactorOAuth})
	if err != nil {
		return nil, core.FailIfRejected(dispatcher, hctx, hooks.HookSignInFailed, err,
			core.FailureReason{SubjectType: models.UserTable, SubjectID: user.ID})
	}
	state := models.SessionActive
	if requireStepUp, _ := checkpoint[hooks.HookValueRequireStepUp].(bool); requireStepUp {
		state = models.SessionPending
	}

	session, rawToken, err := ac.SessionManager.Create(hctx.Ctx, user.ID, core.SessionMeta{State: state, Factor: core.FactorOAuth})
	if err != nil {
		return nil, err
	}
	return &core.SignInResult{User: user, Session: session, RawToken: rawToken, Method: PluginName}, nil
}

// resolveUser returns the user an exchange signs in: the user of the account
// with the identity's subject, the user with the identity's email when
// linkByVerifiedEmail links the two, or a new user. The account gets the
// exchange's tokens when the plugin stores the provider's. It fires
// auth.signIn.failed for its own refusals.
func (p *Plugin) resolveUser(hctx *core.HookContext, providerID string, exchanged *ExchangeResult) (*models.User, error) {
	ac := hctx.Auth
	dispatcher := ac.Dispatcher
	identity := exchanged.Identity
	refuse := func(reason core.FailureReason, err error) error {
		reason.Metadata = behemoth.M{keyProvider: providerID}
		if failErr := dispatcher.Fail(hctx, hooks.HookSignInFailed, reason); failErr != nil {
			return failErr
		}
		return err
	}

	// A sign-up that loses a race is resolved once more: by then the winner
	// has written the account or the user.
	for attempt := 0; ; attempt++ {
		account, err := ac.Store.FindAccount(hctx.Ctx, providerID, identity.Subject)
		switch {
		case err == nil:
			user, err := ac.Store.FindUserByID(hctx.Ctx, account.UserID)
			if err == nil {
				if err := p.keepTokens(hctx.Ctx, account, exchanged.Tokens); err != nil {
					return nil, err
				}
				return user, nil
			}
			if !errs.IsNotFound(err) {
				return nil, err
			}
			// The account outlived its user, where no foreign key removed it.
			return nil, refuse(core.FailureReason{
				Code: "user_not_found", SubjectType: models.UserTable, SubjectID: account.UserID,
			}, errInvalidCallback())
		case !errs.IsNotFound(err):
			return nil, err
		}

		// A subject that is new. From here on the email matters.
		email := store.NormalizeEmail(identity.Email)
		if len(email) > maxUserColumnLength || !utils.IsValidEmail(email) {
			return nil, refuse(core.FailureReason{Code: "email_not_provided"}, errEmailNotProvided())
		}
		hctx.Values[hooks.HookValueEmail] = email

		existing, err := ac.Store.FindUserByEmail(hctx.Ctx, email)
		switch {
		case err == nil:
			linked, err := p.linkByVerifiedEmail(hctx, providerID, exchanged, existing)
			switch {
			case err == nil && linked:
				return existing, nil
			case err == nil:
				// The email alone does not show that this is the same
				// person.
				return nil, refuse(core.FailureReason{
					Code: "account_not_linked", SubjectType: models.UserTable, SubjectID: existing.ID,
				}, errAccountNotLinked())
			case attempt == 0 && errs.IsDuplicateKey(err):
				// The provider account, taken by the winner of a race.
				continue
			}
			return nil, core.FailIfRejected(dispatcher, hctx, hooks.HookSignInFailed, err, core.FailureReason{
				SubjectType: models.UserTable, SubjectID: existing.ID, Metadata: behemoth.M{keyProvider: providerID},
			})
		case !errs.IsNotFound(err):
			return nil, err
		}

		// The sign-up is an operation of its own, nested under the sign-in:
		// its points get a copy of the sign-in's Values.
		user, err := p.signUp(&core.HookContext{Ctx: hctx.Ctx, Auth: ac}, signUpInput{
			provider: providerID,
			subject:  identity.Subject,
			user:     newUser(identity, email),
			tokens:   exchanged.Tokens,
		})
		switch {
		case err == nil:
			return user, nil
		case attempt == 0 && errs.IsDuplicateKey(err):
			// The email or the account, taken by the winner. A taken
			// email is a conflict that holds the duplicate key.
			continue
		}
		return nil, core.FailIfRejected(dispatcher, hctx, hooks.HookSignInFailed, err,
			core.FailureReason{Metadata: behemoth.M{keyProvider: providerID, hooks.HookValueEmail: email}})
	}
}

// signUpBody creates the user of a new subject and the user's account at the
// provider, in one transaction. The account holds the provider's tokens when
// the plugin stores them. auth.signUp.after fires once it has committed.
func (p *Plugin) signUpBody(hctx *core.HookContext, in signUpInput) (*models.User, error) {
	ac := hctx.Auth
	user := in.user
	err := ac.Store.Transaction(hctx.Ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, &user); err != nil {
			return err
		}
		return tx.CreateAccount(ctx, p.newAccount(user.ID, in.provider, in.subject, in.tokens))
	})
	if err != nil {
		// A data hook's veto is a refused sign-up. Any other error, a lost
		// race for the email among them, is returned as it is.
		return nil, core.FailIfRejected(ac.Dispatcher, hctx, hooks.HookSignUpFailed, err,
			core.FailureReason{Metadata: behemoth.M{keyProvider: in.provider, hooks.HookValueEmail: user.Email}})
	}
	return &user, nil
}

// newUser returns the user a new subject signs up as. The profile is what
// the provider said, cut to what the columns hold. EmailVerified is the
// provider's own statement about the email, and nothing else sets it.
func newUser(identity Identity, email string) models.User {
	first, last := strings.TrimSpace(identity.GivenName), strings.TrimSpace(identity.FamilyName)
	if first == "" && last == "" {
		// A provider that only knows a full name.
		first = strings.TrimSpace(identity.Name)
	}
	return models.User{
		Email:         email,
		EmailVerified: identity.EmailVerified,
		Firstname:     clip(first, maxUserColumnLength),
		Lastname:      clip(last, maxUserColumnLength),
		ImageUrl:      pictureURL(identity.Picture),
	}
}

// clip cuts s to at most limit characters, without splitting one.
func clip(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

// pictureURL returns raw when it is an absolute http or https URL, and ""
// otherwise: the value ends up in an application's page.
func pictureURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}
