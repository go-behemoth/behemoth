package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/store"
)

// keyBy is the key of the auth.oauth.beforeLink payload that says how the
// account comes to be linked.
const keyBy = "by"

// How a provider account came to be linked: LinkResult.By, and the "by" key
// of the auth.oauth.beforeLink payload.
const (
	// LinkedByUser is a link the signed-in user made: BeginLink, then
	// Complete.
	LinkedByUser = "user"
	// LinkedByVerifiedEmail is a link a sign-in made, because the provider
	// is in Options.LinkByVerifiedEmail and both sides verified the email.
	LinkedByVerifiedEmail = "verified_email"
)

// ProviderAccount is one of a user's accounts at a provider of the plugin:
// the provider signs the user in.
type ProviderAccount struct {
	// Provider is the provider's id.
	Provider string
	// CreatedAt is when the account was linked, or when the user signed up
	// with the provider.
	CreatedAt time.Time
}

// LinkResult is a provider account that a link attached to a user: what
// Complete returns for a link (CompleteResult.Link), and what
// auth.oauth.afterLink receives.
type LinkResult struct {
	// UserID is the user the account is linked to.
	UserID string
	ProviderAccount
	// By is LinkedByUser or LinkedByVerifiedEmail.
	By string
}

// AuditSubject implements [core.AuditSubject]: a link changes how the user
// signs in.
func (r *LinkResult) AuditSubject() (subjectType, subjectID string) {
	if r == nil || r.UserID == "" {
		return "", ""
	}
	return models.UserTable, r.UserID
}

func newLinkResult(account *models.Account, by string) *LinkResult {
	return &LinkResult{
		UserID:          account.UserID,
		ProviderAccount: ProviderAccount{Provider: account.ProviderID, CreatedAt: account.CreatedAt},
		By:              by,
	}
}

// linkCompletion is the input of the wrapped completion of a link that the
// user started. Its payload on auth.oauth.beforeLink is "userID",
// "provider" and "by", and nothing is read back: a handler can stop the
// link, but it can't point it at another user.
type linkCompletion struct {
	completeInput
	// owner is the user who started the link: the subject of its state.
	owner string
	// callerID and callerSession are the user and the session Complete was
	// called with.
	callerID, callerSession string
}

func (l *linkCompletion) ToMap() (map[string]any, error) {
	return map[string]any{hooks.HookValueUserID: l.owner, keyProvider: l.req.Provider, keyBy: LinkedByUser}, nil
}

func (l *linkCompletion) FromMap(map[string]any) error { return nil }

// autoLinkInput is the input of the wrapped link that a sign-in makes by a
// verified email. Its payload is shaped like linkCompletion's, and nothing
// is read back. The tokens are not in the payload.
type autoLinkInput struct {
	userID, provider, subject string
	tokens                    Tokens
}

func (a *autoLinkInput) ToMap() (map[string]any, error) {
	return map[string]any{hooks.HookValueUserID: a.userID, keyProvider: a.provider, keyBy: LinkedByVerifiedEmail}, nil
}

func (a *autoLinkInput) FromMap(map[string]any) error { return nil }

// finishLink completes the link a state of BeginLink started. The caller is
// checked first, before any point fires, as every self-service flow checks
// it: a caller who is not signed in can't complete a link, whoever's it is.
//
// Freshness is not asked for again. The session was fresh when the link
// started, and the user may spend longer at the provider than
// SessionConfig.FreshAge leaves.
func (p *Plugin) finishLink(hctx *core.HookContext, in completeInput) (*LinkResult, error) {
	callerID, err := p.authContext.Access.RequireUser(hctx.Ctx, PermissionManage)
	if err != nil {
		return nil, err
	}
	session, _ := identity.CallerFrom(hctx.Ctx).Session()
	owner := ""
	if in.transaction.Subject != nil {
		owner = fmt.Sprint(in.transaction.Subject)
	}
	hctx.Values[hooks.HookValueUserID] = owner
	return p.completeLink(hctx, linkCompletion{completeInput: in, owner: owner, callerID: callerID, callerSession: session.ID})
}

func (p *Plugin) completeLinkBody(hctx *core.HookContext, in linkCompletion) (*LinkResult, error) {
	// refuse fires the failed point with what was wrong, and returns the one
	// error a client gets for all of it.
	refuse := func(code string, cause error) error {
		if err := p.linkFailed(hctx, code, cause, in.owner, in.req.Provider); err != nil {
			return err
		}
		return errInvalidCallback()
	}

	cb, code := p.check(in.completeInput)
	if code != "" {
		return nil, refuse(code, nil)
	}
	// A link completes in the session that started it, and only while that
	// session is valid: revoking the session stops the links started in it,
	// and no other session, the user's own included, can finish them. Like
	// the other checks, this one comes before the consume.
	sessionID, _ := in.transaction.MetadataJSON[metaSessionID].(string)
	if in.callerSession == "" || in.callerSession != sessionID || in.callerID != in.owner {
		return nil, refuse("oauth_session_mismatch", nil)
	}
	exchanged, err := p.redeem(hctx, in.completeInput, cb, refuse)
	if err != nil {
		return nil, err
	}
	return p.attach(hctx, in.owner, cb.providerID, exchanged.Identity.Subject, exchanged.Tokens)
}

// attach links the provider account (providerID, subject) to userID, for a
// link the user started, with tokens when the plugin stores the provider's.
// It fires auth.oauth.linkFailed for its refusals: a provider account
// another user has, and a provider the user has an account at already. A
// lost race for the provider account is resolved once more, so the loser
// learns which of the two it was.
func (p *Plugin) attach(hctx *core.HookContext, userID, providerID, subject string, tokens Tokens) (*LinkResult, error) {
	ac := hctx.Auth
	for attempt := 0; ; attempt++ {
		code, err := p.linkConflict(hctx.Ctx, userID, providerID, subject)
		if err != nil {
			return nil, err
		}
		if code != "" {
			if err := p.linkFailed(hctx, code, nil, userID, providerID); err != nil {
				return nil, err
			}
			if code == ErrorCodeProviderAccountTaken {
				return nil, errProviderAccountTaken()
			}
			return nil, errProviderAlreadyLinked("oauth.Complete")
		}

		account := p.newAccount(userID, providerID, subject, tokens)
		err = ac.Store.CreateAccount(hctx.Ctx, account)
		switch {
		case err == nil:
			return newLinkResult(account, LinkedByUser), nil
		case attempt == 0 && errs.IsDuplicateKey(err):
			continue
		}
		// A data hook's veto is a refused link. Any other error is a failure
		// of the system.
		return nil, core.FailIfRejected(ac.Dispatcher, hctx, HookLinkFailed, err, core.FailureReason{
			SubjectType: models.UserTable, SubjectID: userID, Metadata: behemoth.M{keyProvider: providerID},
		})
	}
}

// linkConflict returns the code that refuses a link of the provider account
// (providerID, subject) to userID, or "" when nothing stands in its way.
func (p *Plugin) linkConflict(ctx context.Context, userID, providerID, subject string) (string, error) {
	account, err := p.authContext.Store.FindAccount(ctx, providerID, subject)
	switch {
	case err == nil && account.UserID == userID:
		return ErrorCodeProviderAlreadyLinked, nil
	case err == nil:
		return ErrorCodeProviderAccountTaken, nil
	case !errs.IsNotFound(err):
		return "", err
	}
	other, err := p.accountAt(ctx, userID, providerID)
	if err != nil {
		return "", err
	}
	if other != nil {
		return ErrorCodeProviderAlreadyLinked, nil
	}
	return "", nil
}

// linkByVerifiedEmail links the provider account of an exchange to
// existing, the user with the exchange's email, when
// Options.LinkByVerifiedEmail lets it: the provider is listed, the provider
// and the user both say the email is verified, and the user has no account
// at the provider yet. It reports whether it linked. The link is an
// operation of its own, nested under the sign-in, and fires the
// auth.oauth.*Link points.
func (p *Plugin) linkByVerifiedEmail(hctx *core.HookContext, providerID string, exchanged *ExchangeResult, existing *models.User) (bool, error) {
	who := exchanged.Identity
	// An unverified email on either side is a way into the account: the
	// provider's, for whoever can claim the address there, and the user's,
	// for whoever signed up with somebody else's address.
	if !p.linkByEmail[providerID] || !who.EmailVerified || !existing.EmailVerified {
		return false, nil
	}
	other, err := p.accountAt(hctx.Ctx, existing.ID, providerID)
	if err != nil {
		return false, err
	}
	if other != nil {
		// Another subject at the same provider. A user has one account per
		// provider, and the email does not say which of the two is theirs.
		return false, nil
	}
	values := core.NestedHookValues(hctx.Ctx)
	values[hooks.HookValueUserID] = existing.ID
	if _, err := p.autoLink(&core.HookContext{Ctx: hctx.Ctx, Auth: hctx.Auth, Values: values}, autoLinkInput{
		userID: existing.ID, provider: providerID, subject: who.Subject, tokens: exchanged.Tokens,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// autoLinkBody links the provider account of a sign-in to the user with its
// email, once linkByVerifiedEmail has checked that it may. The account holds
// the provider's tokens when the plugin stores them.
func (p *Plugin) autoLinkBody(hctx *core.HookContext, in autoLinkInput) (*LinkResult, error) {
	ac := hctx.Auth
	account := p.newAccount(in.userID, in.provider, in.subject, in.tokens)
	if err := ac.Store.CreateAccount(hctx.Ctx, account); err != nil {
		// A data hook's veto is a refused link. Any other error, a lost race
		// for the provider account among them, is returned as it is.
		return nil, core.FailIfRejected(ac.Dispatcher, hctx, HookLinkFailed, err, core.FailureReason{
			SubjectType: models.UserTable, SubjectID: in.userID, Metadata: behemoth.M{keyProvider: in.provider},
		})
	}
	return newLinkResult(account, LinkedByVerifiedEmail), nil
}

// linkFailed fires auth.oauth.linkFailed with code, for a link to userID of
// an account at providerID.
func (p *Plugin) linkFailed(hctx *core.HookContext, code string, cause error, userID, providerID string) error {
	reason := core.FailureReason{Code: code, Cause: cause, Metadata: behemoth.M{keyProvider: providerID}}
	if userID != "" {
		reason.SubjectType, reason.SubjectID = models.UserTable, userID
	}
	return hctx.Auth.Dispatcher.Fail(hctx, HookLinkFailed, reason)
}

// UnlinkRequest is the input of Unlink.
type UnlinkRequest struct {
	// Provider is the id of the provider whose account to remove.
	Provider string
}

// FromMap sets the request from a request body: "provider". A missing key
// leaves it empty, for Unlink to refuse; a value that is not a string is a
// validation error.
func (r *UnlinkRequest) FromMap(data map[string]any) error {
	switch v := data[keyProvider].(type) {
	case nil:
		r.Provider = ""
	case string:
		r.Provider = v
	default:
		return errs.NewInvalidInputError("oauth.Unlink", keyProvider, keyProvider+" must be a string", nil)
	}
	return nil
}

// UnlinkResult is what Unlink returns, and what auth.oauth.afterUnlink
// receives.
type UnlinkResult struct {
	// UserID is the user the account was removed from.
	UserID string
	// Provider is the id of the provider whose account was removed.
	Provider string
}

// AuditSubject implements [core.AuditSubject]: an unlink changes how the
// user signs in.
func (r *UnlinkResult) AuditSubject() (subjectType, subjectID string) {
	if r == nil || r.UserID == "" {
		return "", ""
	}
	return models.UserTable, r.UserID
}

// unlinkInput is the input of the wrapped Unlink. Its payload on
// auth.oauth.beforeUnlink is "userID" and "provider", and nothing is read
// back.
type unlinkInput struct {
	userID, provider string
}

func (u *unlinkInput) ToMap() (map[string]any, error) {
	return map[string]any{hooks.HookValueUserID: u.userID, keyProvider: u.provider}, nil
}

func (u *unlinkInput) FromMap(map[string]any) error { return nil }

// errLastWay rolls back the delete of an unlink that would leave the user
// no way to sign in.
var errLastWay = errors.New("oauth: the account is the user's last way to sign in")

// Unlink removes the caller's account at the provider req.Provider, so that
// the provider no longer signs the caller in. It refuses to remove the
// user's last way to sign in: a password, or an account at another provider
// of the plugin, has to remain. A way to sign in that keeps no account, such
// as a magic link or a code sent to a phone number, does not count.
//
// Once the account is removed, the tokens it held are revoked at the
// provider, when the provider can (Revoker). A revocation that fails is
// logged and leaves the unlink as it is.
//
// The caller needs PermissionManage and a fresh session
// (Access.RequireFreshUser). A provider the plugin doesn't have is a
// validation error. A provider the caller has no account at is a conflict
// with ErrorCodeProviderNotLinked, and the last way to sign in a conflict
// with ErrorCodeLastSignInMethod. It fires auth.oauth.beforeUnlink, and
// .afterUnlink or .unlinkFailed.
func (p *Plugin) Unlink(ctx context.Context, req UnlinkRequest) (*UnlinkResult, error) {
	const op = "oauth.Unlink"
	if err := p.ready(op); err != nil {
		return nil, err
	}
	userID, err := p.authContext.Access.RequireFreshUser(ctx, PermissionManage)
	if err != nil {
		return nil, err
	}
	if _, ok := p.providers[req.Provider]; !ok {
		return nil, errs.NewInvalidInputError(op, keyProvider, "unknown provider", nil)
	}
	hctx, err := p.operation(ctx, op)
	if err != nil {
		return nil, err
	}
	hctx.Values[hooks.HookValueUserID] = userID
	hctx.Values[HookValueProvider] = req.Provider
	return p.unlink(hctx, unlinkInput{userID: userID, provider: req.Provider})
}

func (p *Plugin) unlinkBody(hctx *core.HookContext, in unlinkInput) (*UnlinkResult, error) {
	const op = "oauth.Unlink"
	ac := hctx.Auth
	refuse := func(code string, refusal error) error {
		if err := ac.Dispatcher.Fail(hctx, HookUnlinkFailed, core.FailureReason{
			Code: code, SubjectType: models.UserTable, SubjectID: in.userID, Metadata: behemoth.M{keyProvider: in.provider},
		}); err != nil {
			return err
		}
		return refusal
	}
	notLinked := func() error {
		return refuse(ErrorCodeProviderNotLinked, errProviderNotLinked(op))
	}

	account, err := p.accountAt(hctx.Ctx, in.userID, in.provider)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, notLinked()
	}

	// The delete comes first and the check after it, in one transaction. On
	// SQLite the transaction holds the write lock from the delete on, so a
	// second unlink of the same user waits for the first, and then counts
	// without the account the first removed.
	//
	// Deferred: the other databases don't make two deletes of different
	// rows wait on each other. Two unlinks of one user at the same moment
	// can each count the other's account as the one that remains, and
	// both pass. Closing that needs a write both transactions conflict on,
	// such as one to the user's row.
	last := false
	err = ac.Store.Transaction(hctx.Ctx, func(ctx context.Context, tx *store.Store) error {
		last = false // a database that retries the transaction runs this again
		if err := tx.DeleteAccount(ctx, account.ID); err != nil {
			return err
		}
		remaining, err := tx.ListAccountsForUser(ctx, in.userID)
		if err != nil {
			return err
		}
		if !p.canSignInWith(remaining) {
			last = true
			return errLastWay
		}
		return nil
	})
	switch {
	case last:
		return nil, refuse(ErrorCodeLastSignInMethod, conflict(op, ErrorCodeLastSignInMethod, "this account is the last way to sign in"))
	case errs.IsNotFound(err):
		// A concurrent unlink removed it first.
		return nil, notLinked()
	case err != nil:
		return nil, err
	}
	// After the commit: a provider that is down should not keep the user
	// from unlinking.
	p.revokeTokens(hctx.Ctx, account)
	return &UnlinkResult{UserID: in.userID, Provider: in.provider}, nil
}

// canSignInWith reports whether accounts, all of a user's accounts, give the
// user a way to sign in: a password, or an account at a provider of the
// plugin. An account at a provider that is no longer configured signs
// nobody in. A way to sign in that keeps no account, such as a magic link
// or a code sent to a phone number, is not seen here.
func (p *Plugin) canSignInWith(accounts []*models.Account) bool {
	for _, a := range accounts {
		if a.ProviderID == models.ProviderCredential {
			if a.PasswordHash != "" {
				return true
			}
			continue
		}
		if p.providers[a.ProviderID] != nil {
			return true
		}
	}
	return false
}

// Accounts returns the caller's accounts at the providers of the plugin,
// oldest first. An account at a provider that is no longer configured is
// left out: it signs nobody in. A password is not a provider account, and
// is not listed.
//
// The caller needs PermissionRead (Access.RequireUser).
func (p *Plugin) Accounts(ctx context.Context) ([]ProviderAccount, error) {
	const op = "oauth.Accounts"
	if err := p.ready(op); err != nil {
		return nil, err
	}
	userID, err := p.authContext.Access.RequireUser(ctx, PermissionRead)
	if err != nil {
		return nil, err
	}
	accounts, err := p.authContext.Store.ListAccountsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderAccount, 0, len(accounts))
	for _, a := range accounts {
		if p.providers[a.ProviderID] != nil {
			out = append(out, ProviderAccount{Provider: a.ProviderID, CreatedAt: a.CreatedAt})
		}
	}
	return out, nil
}

// accountAt returns the user's account at providerID, or nil when the user
// has none. A user has at most one account per provider.
func (p *Plugin) accountAt(ctx context.Context, userID, providerID string) (*models.Account, error) {
	accounts, err := p.authContext.Store.ListAccountsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if a.ProviderID == providerID {
			return a, nil
		}
	}
	return nil, nil
}
