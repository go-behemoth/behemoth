package plugins_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/oauth"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkTrace keeps what the handlers on the plugin's link and unlink points
// were given during a test.
type linkTrace struct {
	linkBefore   []behemoth.M
	linkAfter    []*oauth.LinkResult
	linkFailed   []core.FailureReason
	unlinkBefore []behemoth.M
	unlinkAfter  []*oauth.UnlinkResult
	unlinkFailed []core.FailureReason
	// linkGate and unlinkGate, when set, decide for the handlers on the
	// before points.
	linkGate, unlinkGate core.BeforeHookFunc
}

func (tr *linkTrace) register(reg core.HookRegistry) error {
	before := func(into *[]behemoth.M, gate *core.BeforeHookFunc) core.BeforeHookFunc {
		return func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			*into = append(*into, payload)
			if *gate != nil {
				return (*gate)(hctx, payload)
			}
			return nil, nil
		}
	}
	failed := func(into *[]core.FailureReason) core.FailedHookFunc {
		return func(_ *core.HookContext, reason core.FailureReason) error {
			*into = append(*into, reason)
			return nil
		}
	}
	return errors.Join(
		reg.OnBefore(oauth.HookLinkBefore, before(&tr.linkBefore, &tr.linkGate), nil),
		reg.OnAfter(oauth.HookLinkAfter, func(_ *core.HookContext, result any) error {
			tr.linkAfter = append(tr.linkAfter, result.(*oauth.LinkResult))
			return nil
		}, nil),
		reg.OnFailed(oauth.HookLinkFailed, failed(&tr.linkFailed), nil),
		reg.OnBefore(oauth.HookUnlinkBefore, before(&tr.unlinkBefore, &tr.unlinkGate), nil),
		reg.OnAfter(oauth.HookUnlinkAfter, func(_ *core.HookContext, result any) error {
			tr.unlinkAfter = append(tr.unlinkAfter, result.(*oauth.UnlinkResult))
			return nil
		}, nil),
		reg.OnFailed(oauth.HookUnlinkFailed, failed(&tr.unlinkFailed), nil),
	)
}

// lastLinkFailure is the last refused link.
func (tr *linkTrace) lastLinkFailure(t *testing.T) core.FailureReason {
	t.Helper()
	require.NotEmpty(t, tr.linkFailed, "auth.oauth.linkFailed did not fire")
	return tr.linkFailed[len(tr.linkFailed)-1]
}

// lastUnlinkFailure is the last refused unlink.
func (tr *linkTrace) lastUnlinkFailure(t *testing.T) core.FailureReason {
	t.Helper()
	require.NotEmpty(t, tr.unlinkFailed, "auth.oauth.unlinkFailed did not fire")
	return tr.unlinkFailed[len(tr.unlinkFailed)-1]
}

// withTelemetry boots with tel, whose recorder keeps the audit events.
func withTelemetry(rec **telemetrytest.Recorder) func(*engine.BootConfig) {
	return func(cfg *engine.BootConfig) {
		tel, r := telemetrytest.New()
		cfg.Telemetry, *rec = tel, r
	}
}

// member creates a user with a verified email, and with a password when
// withPassword is set: a way to sign in that Unlink counts. The hash is not
// a real one, since no test signs in with it.
func (app *oauthApp) member(t *testing.T, email string, withPassword bool) *models.User {
	t.Helper()
	ctx := context.Background()
	user := &models.User{Email: email, EmailVerified: true}
	require.NoError(t, app.ac.Store.CreateUser(ctx, user))
	if withPassword {
		require.NoError(t, app.ac.Store.CreateAccount(ctx, &models.Account{
			UserID: user.ID, ProviderID: models.ProviderCredential, AccountID: user.ID, PasswordHash: "$argon2id$not-a-real-hash",
		}))
	}
	return user
}

// linkedTo gives user an account at provider with subject, as an earlier
// sign-in or link would have.
func (app *oauthApp) linkedTo(t *testing.T, user *models.User, provider, subject string) {
	t.Helper()
	require.NoError(t, app.ac.Store.CreateAccount(context.Background(), &models.Account{UserID: user.ID, ProviderID: provider, AccountID: subject}))
}

// sessionOf creates an active session for the user with userID, and returns
// the context a route would run with for it and the session's token.
func (app *oauthApp) sessionOf(t *testing.T, userID string) (context.Context, string) {
	t.Helper()
	_, raw, err := app.ac.SessionManager.Create(context.Background(), userID, core.SessionMeta{State: models.SessionActive, Factor: core.FactorPassword})
	require.NoError(t, err)
	return presenting(context.Background(), app.ac, raw), raw
}

// beginLink starts a link with provider for the caller of ctx, and returns
// the request that completes it.
func (app *oauthApp) beginLink(t *testing.T, ctx context.Context, provider *fakeProvider, redirect string) oauth.CompleteRequest {
	t.Helper()
	result, err := app.plugin.BeginLink(ctx, oauth.BeginRequest{Provider: provider.id, RedirectURL: redirect})
	require.NoError(t, err)
	require.Equal(t, oauth.IntentLink, result.Intent)
	return oauth.CompleteRequest{Provider: provider.id, Code: "code-from-provider", State: provider.last(t).State, Binding: result.Binding}
}

func sessionIDOf(ctx context.Context) string {
	session, _ := identity.CallerFrom(ctx).Session()
	return session.ID
}

// A signed-in user links a provider account: the state names the user and
// the session, the completion attaches the provider's subject to the user
// whatever email the provider reports, and no session is created. The
// provider signs the user in from then on.
func TestOAuthLinkAttachesAProviderAccount(t *testing.T) {
	var signIns signInTrace
	var links linkTrace
	var rec *telemetrytest.Recorder
	app, err := bootOAuth(t, oauth.Options{}, signIns.register(links.register), withTelemetry(&rec))
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	ctx, _ := app.sessionOf(t, user.ID)

	req := app.beginLink(t, ctx, app.google, "/settings")
	token, err := app.ac.TokenManager.Verify(ctx, core.TokenKindOAuthState, req.State)
	require.NoError(t, err)
	assert.Equal(t, user.ID, token.Subject, "the link names its user")
	assert.Equal(t, "link", token.MetadataJSON["intent"])
	assert.Equal(t, sessionIDOf(ctx), token.MetadataJSON["session_id"], "and the session it was started in")
	assert.Equal(t, "https://app.example.com/auth/callback/google", app.google.last(t).RedirectURI, "the redirect URI of a sign-in")

	// Another address, unverified: a link does not depend on the email.
	app.google.identity = oauth.Identity{Subject: "g-7", Email: "ada.lovelace@gmail.com"}
	result, err := app.plugin.Complete(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result.Link)
	assert.Nil(t, result.SignInResult, "a link is not a sign-in")
	assert.Equal(t, user.ID, result.Link.UserID)
	assert.Equal(t, "google", result.Link.Provider)
	assert.Equal(t, oauth.LinkedByUser, result.Link.By)
	assert.False(t, result.Link.CreatedAt.IsZero())
	assert.Equal(t, "/settings", result.RedirectURL)

	account, err := app.ac.Store.FindAccount(ctx, "google", "g-7")
	require.NoError(t, err)
	assert.Equal(t, user.ID, account.UserID)
	assert.Equal(t, 1, app.count(t, "sessions", "1 = 1"), "no session was created")
	reloaded, err := app.ac.Store.FindUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "ada@example.com", reloaded.Email, "the provider's email is not written to the user")

	// The link's points fired, and none of a sign-in's.
	require.Len(t, links.linkBefore, 1)
	assert.Equal(t, behemoth.M{"userID": user.ID, "provider": "google", "by": "user"}, links.linkBefore[0])
	require.Len(t, links.linkAfter, 1)
	assert.Equal(t, result.Link, links.linkAfter[0])
	assert.Empty(t, links.linkFailed)
	assert.Empty(t, signIns.signInBefore)
	assert.Empty(t, signIns.signInAfter)
	assert.Empty(t, signIns.signInFailed)

	// The audit log names the user as the subject and the actor.
	events := rec.Audit.OfType(string(oauth.HookLinkAfter))
	require.Len(t, events, 1)
	assert.Equal(t, models.UserTable, events[0].SubjectType)
	assert.Equal(t, user.ID, events[0].SubjectID)
	assert.Equal(t, user.ID, events[0].ActorID)
	assert.Equal(t, sessionIDOf(ctx), events[0].SessionID)

	// The state is used up.
	_, err = app.plugin.Complete(ctx, req)
	assert.True(t, invalidCallback(err), "got %v", err)

	// The provider now signs the user in.
	signedIn, err := app.plugin.Complete(context.Background(), app.begin(t, app.google, ""))
	require.NoError(t, err)
	assert.Equal(t, user.ID, signedIn.User.ID)
	assert.Empty(t, signIns.signUpBefore, "it was not a sign-up")
}

// Starting a link needs a user signed in with a fresh session. A system
// caller acting for the user has no session to finish the link in.
func TestOAuthBeginLinkNeedsAFreshSession(t *testing.T) {
	app, err := bootOAuth(t, oauth.Options{}, nil, func(cfg *engine.BootConfig) { cfg.Session.FreshAge = 250 * time.Millisecond })
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	_, pending, err := app.ac.SessionManager.Create(context.Background(), user.ID, core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)

	cases := []struct {
		name     string
		ctx      context.Context
		category errs.Category
		code     string
	}{
		{"an anonymous caller", context.Background(), errs.CategorySession, errs.CodeSessionMissing},
		{"a session waiting for a second factor", presenting(context.Background(), app.ac, pending), errs.CategorySession, errs.CodeSessionPending},
		{"a system caller acting for the user", actingFor(context.Background(), user.ID), errs.CategorySession, errs.CodeSessionMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.plugin.BeginLink(tc.ctx, oauth.BeginRequest{Provider: "google"})
			assert.True(t, hasCode(err, tc.category, tc.code), "got %v", err)
		})
	}
	_, err = app.plugin.BeginLink(asAdmin(context.Background()), oauth.BeginRequest{Provider: "google"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a system caller acts for nobody: %v", err)
	assert.Zero(t, app.tokens(t, "1 = 1"), "no link was started")

	// A link started while the session was fresh completes after it is not
	// any more: the user may take a while at the provider.
	ctx, _ := app.sessionOf(t, user.ID)
	req := app.beginLink(t, ctx, app.google, "")
	time.Sleep(300 * time.Millisecond)
	_, err = app.plugin.BeginLink(ctx, oauth.BeginRequest{Provider: "google"})
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionNotFresh), "got %v", err)
	app.google.identity = oauth.Identity{Subject: "g-7"}
	result, err := app.plugin.Complete(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, user.ID, result.Link.UserID)
}

// A link completes only in the session that started it. Another session,
// the user's own included, and a caller who is not signed in are refused
// before the state is used, so the right session can still finish it. A
// revoked session can't.
func TestOAuthLinkNeedsTheSessionThatStartedIt(t *testing.T) {
	var links linkTrace
	app, err := bootOAuth(t, oauth.Options{}, links.register)
	require.NoError(t, err)
	app.google.identity = oauth.Identity{Subject: "g-7"}
	user := app.member(t, "ada@example.com", true)
	grace := app.member(t, "grace@example.com", true)
	first, _ := app.sessionOf(t, user.ID)
	second, _ := app.sessionOf(t, user.ID)
	other, _ := app.sessionOf(t, grace.ID)

	req := app.beginLink(t, first, app.google, "")
	for name, ctx := range map[string]context.Context{
		"another session of the user": second,
		"another user's session":      other,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := app.plugin.Complete(ctx, req)
			assert.True(t, invalidCallback(err), "got %v", err)
			reason := links.lastLinkFailure(t)
			assert.Equal(t, "oauth_session_mismatch", reason.Code)
			assert.Equal(t, user.ID, reason.SubjectID, "the failure names the user the link was for")
			assert.Equal(t, behemoth.M{"provider": "google"}, reason.Metadata)
		})
	}
	before := len(links.linkFailed)
	_, err = app.plugin.Complete(context.Background(), req)
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionMissing), "got %v", err)
	assert.Len(t, links.linkFailed, before, "a caller who is not signed in is refused before the link's points")
	assert.Zero(t, app.google.exchanges(), "none of them reached the provider")

	result, err := app.plugin.Complete(first, req)
	require.NoError(t, err, "the state still works in the session that started it")
	assert.Equal(t, user.ID, result.Link.UserID)

	// Revoking the session stops a link started in it.
	ctx, raw := app.sessionOf(t, grace.ID)
	req = app.beginLink(t, ctx, app.google, "")
	require.NoError(t, app.ac.SessionManager.Revoke(context.Background(), sessionIDOf(ctx), "test"))
	_, err = app.plugin.Complete(presenting(context.Background(), app.ac, raw), req)
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionRevoked), "got %v", err)
	assert.Equal(t, 1, app.count(t, "accounts", "provider_id = 'google'"))
}

// A user has one account per provider, and a provider account belongs to
// one user. A link that breaks either rule is refused: at the start when it
// already can be told, and at the completion.
func TestOAuthLinkConflicts(t *testing.T) {
	var links linkTrace
	app, err := bootOAuth(t, oauth.Options{}, links.register)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	grace := app.member(t, "grace@example.com", true)
	app.linkedTo(t, grace, "google", "g-grace")
	ctx, _ := app.sessionOf(t, user.ID)

	t.Run("a provider account of another user", func(t *testing.T) {
		req := app.beginLink(t, ctx, app.google, "")
		app.google.identity = oauth.Identity{Subject: "g-grace"}
		_, err := app.plugin.Complete(ctx, req)
		assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderAccountTaken), "got %v", err)
		reason := links.lastLinkFailure(t)
		assert.Equal(t, "provider_account_taken", reason.Code)
		assert.Equal(t, user.ID, reason.SubjectID)
		account, err := app.ac.Store.FindAccount(ctx, "google", "g-grace")
		require.NoError(t, err)
		assert.Equal(t, grace.ID, account.UserID, "the account stays with its user")
	})

	// Two links started before either finished: the second finds the
	// provider linked, whichever subject it brings.
	first := app.beginLink(t, ctx, app.google, "")
	second := app.beginLink(t, ctx, app.google, "")
	third := app.beginLink(t, ctx, app.google, "")
	app.google.identity = oauth.Identity{Subject: "g-ada"}
	_, err = app.plugin.Complete(ctx, first)
	require.NoError(t, err)
	_, err = app.plugin.Complete(ctx, second)
	assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderAlreadyLinked), "the same subject: %v", err)
	app.google.identity = oauth.Identity{Subject: "g-ada-work"}
	_, err = app.plugin.Complete(ctx, third)
	assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderAlreadyLinked), "another subject: %v", err)
	assert.Equal(t, "provider_already_linked", links.lastLinkFailure(t).Code)
	assert.Equal(t, 1, app.count(t, "accounts", "provider_id = 'google' AND user_id = '"+user.ID+"'"))

	// Once linked, a new link to the provider is refused before the user is
	// sent there.
	before := app.tokens(t, "1 = 1")
	_, err = app.plugin.BeginLink(ctx, oauth.BeginRequest{Provider: "google"})
	assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderAlreadyLinked), "got %v", err)
	assert.Equal(t, before, app.tokens(t, "1 = 1"))
}

// Store.DeleteUser removes a link the user started, with the user.
func TestOAuthLinkGoesWithItsUser(t *testing.T) {
	app, err := bootOAuth(t, oauth.Options{}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	ctx, _ := app.sessionOf(t, user.ID)
	req := app.beginLink(t, ctx, app.google, "")

	require.NoError(t, app.ac.Store.DeleteUser(context.Background(), user.ID))
	_, err = app.ac.TokenManager.Verify(context.Background(), core.TokenKindOAuthState, req.State)
	assert.Error(t, err, "the started link is gone")
}

// With the provider in LinkByVerifiedEmail, a first sign-in whose verified
// email belongs to a user with a verified email links to that user and
// signs them in. Every other case is refused as before.
func TestOAuthLinkByVerifiedEmail(t *testing.T) {
	ctx := context.Background()
	var signIns signInTrace
	var links linkTrace
	var rec *telemetrytest.Recorder
	github := &fakeProvider{id: "github"}
	google := &fakeProvider{id: "google"}
	app, err := bootOAuth(t, oauth.Options{
		CustomProviders: []oauth.Provider{google, github}, LinkByVerifiedEmail: []string{"google"},
	}, signIns.register(links.register), withTelemetry(&rec))
	require.NoError(t, err)
	ada := app.member(t, "ada@example.com", true)

	google.identity = oauth.Identity{Subject: "g-1", Email: "Ada@Example.com", EmailVerified: true, GivenName: "Augusta"}
	result, err := app.plugin.Complete(ctx, app.begin(t, google, ""))
	require.NoError(t, err)
	assert.Equal(t, ada.ID, result.User.ID, "the user with the email is signed in")
	assert.Equal(t, models.SessionActive, result.Session.State)
	assert.Nil(t, result.Link, "a sign-in that links is still a sign-in")
	account, err := app.ac.Store.FindAccount(ctx, "google", "g-1")
	require.NoError(t, err)
	assert.Equal(t, ada.ID, account.UserID)
	reloaded, err := app.ac.Store.FindUserByID(ctx, ada.ID)
	require.NoError(t, err)
	assert.Empty(t, reloaded.Firstname, "the provider's profile is not written to the user")

	require.Len(t, links.linkBefore, 1)
	assert.Equal(t, behemoth.M{"userID": ada.ID, "provider": "google", "by": "verified_email"}, links.linkBefore[0])
	require.Len(t, links.linkAfter, 1)
	assert.Equal(t, oauth.LinkedByVerifiedEmail, links.linkAfter[0].By)
	assert.Len(t, signIns.signInAfter, 1)
	assert.Empty(t, signIns.signUpBefore)
	events := rec.Audit.OfType(string(oauth.HookLinkAfter))
	require.Len(t, events, 1)
	assert.Equal(t, ada.ID, events[0].SubjectID)

	// The account works like any other from then on.
	again, err := app.plugin.Complete(ctx, app.begin(t, google, ""))
	require.NoError(t, err)
	assert.Equal(t, ada.ID, again.User.ID)
	assert.Len(t, links.linkAfter, 1, "the second sign-in found the account")

	unverified := app.member(t, "grace@example.com", true)
	_, err = app.ac.Store.UpdateUser(ctx, unverified.ID, behemoth.M{models.UserEmailVerified: false})
	require.NoError(t, err)
	katherine := app.member(t, "katherine@example.com", true)
	app.linkedTo(t, katherine, "google", "g-katherine")
	app.member(t, "dorothy@example.com", true)
	cases := []struct {
		name     string
		provider *fakeProvider
		identity oauth.Identity
		user     string
	}{
		{"the provider did not verify the email", google, oauth.Identity{Subject: "g-2", Email: "ada@example.com"}, ada.ID},
		{"the user's email is not verified", google, oauth.Identity{Subject: "g-3", Email: "grace@example.com", EmailVerified: true}, unverified.ID},
		{"the user has another account at the provider", google, oauth.Identity{Subject: "g-4", Email: "katherine@example.com", EmailVerified: true}, katherine.ID},
		{"the provider is not listed", github, oauth.Identity{Subject: "gh-1", Email: "dorothy@example.com", EmailVerified: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.provider.identity = tc.identity
			_, err := app.plugin.Complete(ctx, app.begin(t, tc.provider, ""))
			assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeAccountNotLinked), "got %v", err)
			assert.Equal(t, "account_not_linked", signIns.lastFailure(t))
			if tc.user != "" {
				assert.Equal(t, tc.user, signIns.signInFailed[len(signIns.signInFailed)-1].SubjectID)
			}
		})
	}
	assert.Len(t, links.linkBefore, 1, "none of them tried to link")
	assert.Equal(t, 2, app.count(t, "accounts", "provider_id IN ('google', 'github')"), "ada's link and katherine's own account")
}

// A handler on auth.oauth.beforeLink can keep a sign-in from linking. The
// link and the sign-in both count as refused, and nothing is written.
func TestOAuthLinkByVerifiedEmailCanBeRefused(t *testing.T) {
	ctx := context.Background()
	var signIns signInTrace
	links := linkTrace{linkGate: func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
		if payload["by"] == oauth.LinkedByVerifiedEmail {
			return nil, errs.NewForbidden("test", "link_by_email_off", "link from your settings")
		}
		return nil, nil
	}}
	app, err := bootOAuth(t, oauth.Options{LinkByVerifiedEmail: []string{"google"}}, signIns.register(links.register))
	require.NoError(t, err)
	app.member(t, "ada@example.com", true)

	app.google.identity = oauth.Identity{Subject: "g-1", Email: "ada@example.com", EmailVerified: true}
	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	assert.True(t, hasCode(err, errs.CategoryForbidden, "link_by_email_off"), "the handler's error is returned: %v", err)
	assert.Equal(t, core.FailureRejectedByHook, links.lastLinkFailure(t).Code)
	assert.Equal(t, core.FailureRejectedByHook, signIns.lastFailure(t))
	assert.Zero(t, app.count(t, "accounts", "provider_id = 'google'"))
	assert.Zero(t, app.count(t, "sessions", "1 = 1"))
}

// Unlink removes the caller's account at a provider, unless it is the last
// way the user can sign in: a password, or an account at another
// configured provider, has to remain.
func TestOAuthUnlink(t *testing.T) {
	ctx := context.Background()
	var links linkTrace
	var rec *telemetrytest.Recorder
	github := &fakeProvider{id: "github"}
	app, err := bootOAuth(t, oauth.Options{CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}, github}}, links.register, withTelemetry(&rec))
	require.NoError(t, err)

	t.Run("with a password", func(t *testing.T) {
		user := app.member(t, "ada@example.com", true)
		app.linkedTo(t, user, "google", "g-ada")
		caller, _ := app.sessionOf(t, user.ID)

		result, err := app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
		require.NoError(t, err)
		assert.Equal(t, &oauth.UnlinkResult{UserID: user.ID, Provider: "google"}, result)
		_, err = app.ac.Store.FindAccount(ctx, "google", "g-ada")
		assert.True(t, errs.IsNotFound(err), "the account is gone: %v", err)
		require.NotEmpty(t, links.unlinkBefore)
		assert.Equal(t, behemoth.M{"userID": user.ID, "provider": "google"}, links.unlinkBefore[len(links.unlinkBefore)-1])
		require.Len(t, links.unlinkAfter, 1)
		assert.Equal(t, result, links.unlinkAfter[0])
		events := rec.Audit.OfType(string(oauth.HookUnlinkAfter))
		require.Len(t, events, 1)
		assert.Equal(t, user.ID, events[0].SubjectID)

		_, err = app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
		assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderNotLinked), "got %v", err)
		reason := links.lastUnlinkFailure(t)
		assert.Equal(t, "provider_not_linked", reason.Code)
		assert.Equal(t, user.ID, reason.SubjectID)

		_, err = app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "facebook"})
		assert.True(t, hasCode(err, errs.CategoryValidation, "provider_invalid_input"), "got %v", err)
	})

	t.Run("the last way to sign in", func(t *testing.T) {
		user := app.member(t, "grace@example.com", false)
		app.linkedTo(t, user, "google", "g-grace")
		app.linkedTo(t, user, "github", "gh-grace")
		caller, _ := app.sessionOf(t, user.ID)

		_, err := app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "github"})
		require.NoError(t, err, "google remains")
		_, err = app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
		assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeLastSignInMethod), "got %v", err)
		assert.Equal(t, "last_sign_in_method", links.lastUnlinkFailure(t).Code)
		_, err = app.ac.Store.FindAccount(ctx, "google", "g-grace")
		assert.NoError(t, err, "the delete was rolled back")
	})

	t.Run("what does not count as a way to sign in", func(t *testing.T) {
		user := app.member(t, "katherine@example.com", false)
		app.linkedTo(t, user, "google", "g-katherine")
		// A credential account without a password, and an account at a
		// provider the plugin no longer has.
		require.NoError(t, app.ac.Store.CreateAccount(ctx, &models.Account{UserID: user.ID, ProviderID: models.ProviderCredential, AccountID: user.ID}))
		app.linkedTo(t, user, "facebook", "fb-katherine")
		caller, _ := app.sessionOf(t, user.ID)

		_, err := app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
		assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeLastSignInMethod), "got %v", err)
	})

	t.Run("a handler can refuse", func(t *testing.T) {
		user := app.member(t, "dorothy@example.com", true)
		app.linkedTo(t, user, "google", "g-dorothy")
		caller, _ := app.sessionOf(t, user.ID)
		links.unlinkGate = func(*core.HookContext, behemoth.M) (behemoth.M, error) {
			return nil, errs.NewForbidden("test", "unlink_locked", "ask support")
		}
		defer func() { links.unlinkGate = nil }()

		_, err := app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
		assert.True(t, hasCode(err, errs.CategoryForbidden, "unlink_locked"), "got %v", err)
		assert.Equal(t, core.FailureRejectedByHook, links.lastUnlinkFailure(t).Code)
		_, err = app.ac.Store.FindAccount(ctx, "google", "g-dorothy")
		assert.NoError(t, err)
	})

	t.Run("it needs a signed-in user", func(t *testing.T) {
		_, err := app.plugin.Unlink(ctx, oauth.UnlinkRequest{Provider: "google"})
		assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionMissing), "got %v", err)
	})
}

// Unlink, like a link, needs a fresh session.
func TestOAuthUnlinkNeedsAFreshSession(t *testing.T) {
	app, err := bootOAuth(t, oauth.Options{}, nil, func(cfg *engine.BootConfig) { cfg.Session.FreshAge = 100 * time.Millisecond })
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	app.linkedTo(t, user, "google", "g-ada")
	ctx, _ := app.sessionOf(t, user.ID)
	time.Sleep(150 * time.Millisecond)

	_, err = app.plugin.Unlink(ctx, oauth.UnlinkRequest{Provider: "google"})
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionNotFresh), "got %v", err)
	assert.Equal(t, 1, app.count(t, "accounts", "provider_id = 'google'"))
}

// Accounts lists the caller's accounts at the configured providers, oldest
// first. A password and an account at a provider the plugin no longer has
// are not listed.
func TestOAuthAccounts(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}, &fakeProvider{id: "github"}}}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	app.linkedTo(t, user, "github", "gh-ada")
	app.linkedTo(t, user, "facebook", "fb-ada")
	app.linkedTo(t, user, "google", "g-ada")
	caller, _ := app.sessionOf(t, user.ID)

	accounts, err := app.plugin.Accounts(caller)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	assert.Equal(t, "github", accounts[0].Provider)
	assert.Equal(t, "google", accounts[1].Provider)
	assert.False(t, accounts[0].CreatedAt.After(accounts[1].CreatedAt), "oldest first")

	other := app.member(t, "grace@example.com", true)
	otherCaller, _ := app.sessionOf(t, other.ID)
	none, err := app.plugin.Accounts(otherCaller)
	require.NoError(t, err)
	assert.NotNil(t, none)
	assert.Empty(t, none)

	_, err = app.plugin.Accounts(ctx)
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionMissing), "got %v", err)
	unbooted := oauth.New(oauth.Options{CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}}, CallbackURL: "https://app.example.com/cb"})
	for name, call := range map[string]func() error{
		"Accounts":  func() error { _, err := unbooted.Accounts(caller); return err },
		"Unlink":    func() error { _, err := unbooted.Unlink(caller, oauth.UnlinkRequest{Provider: "google"}); return err },
		"BeginLink": func() error { _, err := unbooted.BeginLink(caller, oauth.BeginRequest{Provider: "google"}); return err },
	} {
		assert.True(t, errs.IsCategory(call(), errs.CategoryConfiguration), "%s before Boot", name)
	}
}
