package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/utils"
)

type DefaultTokenManager struct {
	st      *store.Store
	kv      behemoth.KeyValueStorage
	catalog core.TokenCatalog
	crypto  crypto.Services
	cfg     core.TokenConfig
	disp    core.Dispatcher
	auth    *core.AuthContext
}

// NewDefaultTokenManager builds the default TokenManager. auth is the
// AuthContext the manager belongs to; it is only handed to hook handlers as
// HookContext.Auth and is not read during construction, so Boot may pass it
// before all of its fields are set.
func NewDefaultTokenManager(
	st *store.Store,
	kv behemoth.KeyValueStorage,
	catalog core.TokenCatalog,
	crypto crypto.Services,
	dispatcher core.Dispatcher,
	cfg core.TokenConfig,
	auth *core.AuthContext,
) core.TokenManager {
	return &DefaultTokenManager{
		st:      st,
		kv:      kv,
		catalog: catalog,
		crypto:  crypto,
		disp:    dispatcher,
		cfg:     cfg,
		auth:    auth,
	}
}

// telemetry returns the AuthContext's Telemetry, or nil before Boot has set
// it. A nil Telemetry starts no spans.
func (tm *DefaultTokenManager) telemetry() *telemetry.Telemetry {
	if tm.auth == nil {
		return nil
	}
	return tm.auth.Telemetry
}

// log returns the manager's logger. It is read from the AuthContext on use,
// because Boot hands the manager its AuthContext before every field is set.
func (tm *DefaultTokenManager) log() telemetry.Logger {
	if tm.auth == nil || tm.auth.Telemetry == nil {
		return telemetry.NoOpLogger{}
	}
	return telemetry.Named(tm.auth.Telemetry.Logger, "token")
}

func kvKey(kind models.TokenKind, lookupHash string) string {
	return "token:" + string(kind) + ":" + lookupHash
}

func resolveTTL(def core.TokenKindDef, cfg core.TokenConfig) time.Duration {
	if override, ok := cfg.TTLOverrides[def.Kind]; ok {
		return override
	}
	return def.DefaultTTL
}

// checkConsumable reports why tok can't be used at now, the store's time.
func checkConsumable(tok *models.Token, now time.Time) error {
	const op = "TokenManager.checkConsumable"
	if tok.RevokedAt != nil {
		return errs.NewTokenError(op, "token_revoked", nil)
	}
	if !tok.ExpiresAt.IsZero() && now.After(tok.ExpiresAt) {
		return errs.NewTokenError(op, "token_expired", nil)
	}
	if tok.ConsumedAt != nil {
		return errs.NewTokenError(op, "token_already_consumed", nil)
	}
	return nil
}

func (tm *DefaultTokenManager) Issue(
	ctx context.Context,
	kind models.TokenKind,
	subject any,
	meta behemoth.M,
) (*models.Token, string, error) {
	var spanAttrs behemoth.M
	if tm.telemetry().TracingEnabled() {
		spanAttrs = behemoth.M{telemetry.AttrKind: string(kind)}
	}
	ctx, span := tm.telemetry().StartSpan(ctx, telemetry.SpanTokenIssue, spanAttrs)
	tok, rawToken, err := tm.issue(ctx, kind, subject, meta)
	telemetry.FinishSpan(span, err)
	return tok, rawToken, err
}

// issue is Issue without its span.
func (tm *DefaultTokenManager) issue(
	ctx context.Context,
	kind models.TokenKind,
	subject any,
	meta behemoth.M,
) (*models.Token, string, error) {
	const op = "TokenManager.Issue"
	ctx = core.BeginOperation(ctx) // beforeIssue and afterIssue share Values

	// The kind and subject are also published in Values, where a rate-limit
	// rule on token.beforeIssue keys on them: HookRateLimitRule.KeyFunc gets
	// the HookContext and not the payload. A token without a subject leaves
	// no subject entry, so a rule per subject does not apply to it.
	values := core.HookValuesFrom(ctx)
	values[hooks.HookValueTokenKind] = string(kind)
	if s := subjectString(subject); s != "" {
		values[hooks.HookValueTokenSubject] = s
	} else {
		delete(values, hooks.HookValueTokenSubject)
	}

	def, ok := tm.catalog.Lookup(kind)
	if !ok {
		return nil, "", errs.NewValidationError(op, "kind", fmt.Errorf("unknown token kind %q", kind))
	}

	if def.Backend == core.TokenBackendKV && tm.kv == nil {
		// this should already have been caught at Boot when
		// resolving backends, but a kind declared after boot misconfiguration
		// (or a bug in that check) must not silently fall through to a nil
		// pointer dereference below.
		return nil, "", errs.NewConfigurationError(op, fmt.Sprintf("kind %q requires KV storage, none configured", kind), nil)
	}

	rawToken, err := tm.crypto.Random.SecureRandomString(32)
	if err != nil {
		return nil, "", errs.NewSecurityError(op, "random_generation_failed", err)
	}

	hash, keyVersion, err := tm.crypto.Secrets.Hash(rawToken)
	if err != nil {
		return nil, "", err
	}

	// The expiry comes from the store's clock, in UTC like every time the
	// store writes: the purge compares it in the database, and SQLite
	// compares times as text.
	ttl := resolveTTL(def, tm.cfg)
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = tm.st.Now().Add(ttl)
	}

	tok := &models.Token{
		Kind:         kind,
		Subject:      subjectString(subject),
		LookupHash:   utils.LookupHashOf(rawToken),
		TokenHash:    hash,
		KeyVersion:   keyVersion,
		ExpiresAt:    expiresAt,
		MetadataJSON: meta,
	}

	// The returned payload is not read: a handler can stop the issue, but
	// it can't change the token's kind or subject.
	if _, err := tm.disp.RunBefore(hookContext(ctx, tm.auth, hooks.HookTokenBeforeIssue, core.BeforeHookPhase), hooks.HookTokenBeforeIssue, behemoth.M{
		hooks.HookValueTokenKind:    string(kind),
		hooks.HookValueTokenSubject: subjectString(subject),
	}); err != nil {
		return nil, "", err
	}

	if err := tm.persist(ctx, def, tok, ttl); err != nil {
		return nil, "", errs.WrapOp(op, "token", err)
	}

	if err := tm.disp.RunAfter(hookContext(ctx, tm.auth, hooks.HookTokenAfterIssue, core.AfterHookPhase), hooks.HookTokenAfterIssue, tok); err != nil {
		return nil, "", err
	}
	return tok, rawToken, nil
}

func subjectString(subject any) string {
	if subject == nil {
		return ""
	}
	return fmt.Sprint(subject)
}

func (tm *DefaultTokenManager) persist(ctx context.Context, def core.TokenKindDef, token *models.Token, ttl time.Duration) error {
	const op = "TokenManager.persist"
	switch def.Backend {
	case core.TokenBackendKV:
		// No store involved: the KV entry is the whole record, so its
		// identity and creation time are assigned here.
		token.ID, token.CreatedAt = utils.GenerateUUID(), tm.st.Now()
		data, err := json.Marshal(token)
		if err != nil {
			return errs.NewValidationError(op, "token", err)
		}
		ttlSeconds := int(ttl.Seconds())
		if ttlSeconds <= 0 {
			// A KV entry with no TTL never expires at the store level; for
			// BackendKV kinds this should never happen (they're declared with
			// a real DefaultTTL), but guard rather than silently persist forever.
			return errs.NewConfigurationError(op, "BackendKV token kind must have a positive TTL", nil)
		}
		return tm.kv.Set(ctx, kvKey(models.TokenKind(token.Kind), token.LookupHash), string(data), ttlSeconds)
	default: // BackendDB
		return tm.st.CreateToken(ctx, token)
	}
}

// fetch is the shared lookup & authenticity check path used by Verify and
// Consume, so the two entry points can never drift on that logic. A DB token
// is read through st — Consume passes its transaction's store.
func (tm *DefaultTokenManager) fetch(ctx context.Context, st *store.Store, def core.TokenKindDef, kind models.TokenKind, rawToken string) (*models.Token, error) {
	const op = "TokenManager.fetch"
	lookupHash := utils.LookupHashOf(rawToken)

	var m *models.Token
	switch def.Backend {
	case core.TokenBackendKV:
		raw, err := tm.kv.Get(ctx, kvKey(kind, lookupHash))
		if err != nil {
			return nil, errs.NewTokenError(op, errs.CodeTokenNotFound, err)
		}
		m = &models.Token{}
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			// A corrupted entry is treated as absent, so the caller sees an
			// ordinary "not found". The line is the only sign of it.
			tm.log().Warn(ctx, "token entry in the key-value store could not be decoded; treating it as absent",
				telemetry.ErrorFields(err, behemoth.M{"kind": string(kind)}))
			return nil, errs.NewTokenError(op, errs.CodeTokenNotFound, err)
		}
	default:
		found, err := st.FindTokenByLookupHash(ctx, kind, lookupHash)
		if err != nil {
			return nil, errs.WrapOp(op, "token", err) // NotFound re-contextualized from adapter's raw "FindOne"/"tokens"
		}
		m = found
	}

	ok, err := tm.crypto.Secrets.Verify(rawToken, m.TokenHash, m.KeyVersion)
	if err != nil {
		return nil, err
	}
	if !ok {
		// LookupHash matched but keyed HMAC didn't
		return nil, errs.NewTokenError(op, errs.CodeTokenNotFound, nil)
	}
	return m, nil
}

func (tm *DefaultTokenManager) Verify(ctx context.Context, kind models.TokenKind, rawToken string) (*models.Token, error) {
	const op = "TokenManager.Verify"

	def, ok := tm.catalog.Lookup(kind)
	if !ok {
		return nil, errs.NewValidationError(op, "kind", fmt.Errorf("unknown token kind %q", kind))
	}

	m, err := tm.fetch(ctx, tm.st, def, kind, rawToken)
	if err != nil {
		return nil, err // already fully classified inside fetch
	}
	if err := checkConsumable(m, tm.st.Now()); err != nil {
		return nil, err
	}
	return m, nil
}

func (tm *DefaultTokenManager) Consume(ctx context.Context, kind models.TokenKind, rawToken string) (*models.Token, error) {
	var spanAttrs behemoth.M
	if tm.telemetry().TracingEnabled() {
		spanAttrs = behemoth.M{telemetry.AttrKind: string(kind)}
	}
	ctx, span := tm.telemetry().StartSpan(ctx, telemetry.SpanTokenConsume, spanAttrs)
	tok, err := tm.consume(ctx, kind, rawToken)
	telemetry.FinishSpan(span, err)
	return tok, err
}

// consume is Consume without its span.
func (tm *DefaultTokenManager) consume(ctx context.Context, kind models.TokenKind, rawToken string) (*models.Token, error) {
	const op = "TokenManager.Consume"
	ctx = core.BeginOperation(ctx) // one operation for the consume and its consumed or failed point

	def, ok := tm.catalog.Lookup(kind)
	if !ok {
		return nil, errs.NewValidationError(op, "kind", fmt.Errorf("unknown token kind %q", kind))
	}

	if !def.SingleUse {
		// Calling Consume() a multi-use credential (an API key) would permanently
		// disable it on first use
		return nil, errs.NewTokenError(op, errs.CodeTokenInvalidUsage, nil)
	}
	if def.Backend != core.TokenBackendDB {
		// Only the database consumes a token at most once: KeyValueStorage
		// has no operation that reads and deletes an entry in one step.
		// DefaultTokenCatalog refuses such a kind, and this covers a catalog
		// that doesn't. Consuming it anyway would leave the token usable.
		return nil, errs.NewConfigurationError(op, fmt.Sprintf("kind %q is single-use and not in the database: it can't be consumed exactly once", kind), nil)
	}

	// Fetch, validate and consume in one transaction. ConsumeToken is what
	// makes it at-most-once: a concurrent Consume that read the token before
	// this one stamped it still loses.
	var result *models.Token
	consumeErr := tm.st.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		tok, err := tm.fetch(ctx, tx, def, kind, rawToken)
		if err != nil {
			return err // already fully classified inside fetch
		}
		if err := checkConsumable(tok, tx.Now()); err != nil {
			return err
		}
		at, err := tx.ConsumeToken(ctx, tok.ID)
		if err != nil {
			return err
		}
		tok.ConsumedAt = &at
		result = tok
		return nil
	})

	if consumeErr != nil {
		hctx := hookContext(ctx, tm.auth, hooks.HookTokenFailed, core.FailedHookPhase)
		if err := tm.disp.Fail(hctx, hooks.HookTokenFailed, core.FailureReason{Code: errs.ClassifyCode(consumeErr), Cause: consumeErr}); err != nil {
			return nil, err
		}
		return nil, consumeErr
	}
	if err := tm.disp.RunAfter(hookContext(ctx, tm.auth, hooks.HookTokenConsumed, core.AfterHookPhase), hooks.HookTokenConsumed, result); err != nil {
		return nil, err
	}
	return result, nil
}

// Revoke implements [core.TokenManager].
func (tm *DefaultTokenManager) Revoke(ctx context.Context, tokenID string) error {
	const op = "TokenManager.Revoke"

	m, err := tm.st.FindTokenByID(ctx, tokenID)
	if err != nil {
		return errs.WrapOp(op, "token", err)
	}
	if m.RevokedAt != nil {
		return nil // idempotent, matches Session.Revoke's posture
	}
	if err := tm.st.RevokeToken(ctx, tokenID); err != nil {
		return errs.WrapOp(op, "token", err)
	}

	return nil
}

// RevokeAllForSubject implements [core.TokenManager].
func (tm *DefaultTokenManager) RevokeAllForSubject(ctx context.Context, kind models.TokenKind, subject any) error {
	const op = "TokenManager.RevokeAllForSubject"

	def, ok := tm.catalog.Lookup(kind)
	if !ok {
		return errs.NewValidationError(op, "kind", fmt.Errorf("unknown token kind %q", kind))
	}
	if def.Backend == core.TokenBackendKV {
		return errs.NewConfigurationError(op, fmt.Sprintf("kind %q is KV-backed; bulk revoke by subject is not supported", kind), nil)
	}
	if err := tm.st.RevokeTokensForSubject(ctx, kind, subjectString(subject)); err != nil {
		return errs.WrapOp(op, "token", err)
	}
	return nil
}

// ListForSubject implements [core.TokenManager].
func (tm *DefaultTokenManager) ListForSubject(ctx context.Context, kind models.TokenKind, subject any) ([]*models.Token, error) {
	const op = "TokenManager.ListForSubject"

	def, ok := tm.catalog.Lookup(kind)
	if !ok {
		return nil, errs.NewValidationError(op, "kind", fmt.Errorf("unknown token kind %q", kind))
	}
	if def.Backend == core.TokenBackendKV {
		return nil, errs.NewConfigurationError(op, fmt.Sprintf("kind %q is KV-backed; listing by subject is not supported", kind), nil)
	}
	found, err := tm.st.ListTokensForSubject(ctx, kind, subjectString(subject))
	if err != nil {
		return nil, errs.WrapOp(op, "token", err)
	}
	usable, now := found[:0], tm.st.Now()
	for _, tok := range found {
		if checkConsumable(tok, now) == nil {
			usable = append(usable, tok)
		}
	}
	return usable, nil
}

var _ core.TokenManager = (*DefaultTokenManager)(nil)
