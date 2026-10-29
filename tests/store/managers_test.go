package store_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/auth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/models"
	pgAdapter "github.com/go-behemoth/behemoth/storage/adapters/postgres"
	redisAdapter "github.com/go-behemoth/behemoth/storage/adapters/redis"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sessionsTokensSQLite = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL, lookup_hash TEXT NOT NULL UNIQUE, token_hash TEXT NOT NULL,
	key_version INTEGER NOT NULL, state TEXT NOT NULL, expires_at TIMESTAMP NOT NULL, last_active_at TIMESTAMP,
	fresh_at TIMESTAMP, factors TEXT, ip_address TEXT, user_agent TEXT, impersonator_id TEXT, revoked_at TIMESTAMP,
	revoked_reason TEXT, version INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
CREATE TABLE tokens (
	id TEXT PRIMARY KEY, kind TEXT NOT NULL, subject TEXT, lookup_hash TEXT NOT NULL, token_hash TEXT NOT NULL,
	key_version INTEGER NOT NULL, metadata TEXT, expires_at TIMESTAMP, consumed_at TIMESTAMP, revoked_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL);`

// sessionsTokensDB opens a file SQLite database with the sessions and tokens
// tables. busy_timeout makes concurrent writers wait instead of failing, so
// concurrency tests exercise the logic, not lock errors.
func sessionsTokensDB(t *testing.T) behemoth.Database {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "managers.db")+"?_foreign_keys=on&_busy_timeout=10000")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(sessionsTokensSQLite + testutils.AuditLogSQLiteSchema)
	require.NoError(t, err)
	return sqliteAdapter.NewSQLiteAdapter(db, nil)
}

func testCrypto(t *testing.T) crypto.Services {
	t.Helper()
	c, err := crypto.New(context.Background(), crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("cd", 32)}, Current: 1},
	}, nil)
	require.NoError(t, err)
	return c
}

// passDispatcher passes every payload through and records the contexts.
type passDispatcher struct {
	mu   sync.Mutex
	seen []*core.HookContext
}

func (d *passDispatcher) record(h *core.HookContext) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, h)
}
func (d *passDispatcher) RunBefore(h *core.HookContext, _ core.HookPoint, p behemoth.M) (behemoth.M, error) {
	d.record(h)
	return p, nil
}
func (d *passDispatcher) RunAfter(h *core.HookContext, _ core.HookPoint, _ any) error {
	d.record(h)
	return nil
}
func (d *passDispatcher) RunAfterTx(h *core.HookContext, _ core.HookPoint, _ any) error {
	d.record(h)
	return nil
}
func (d *passDispatcher) Fail(h *core.HookContext, _ core.HookPoint, _ core.FailureReason) error {
	d.record(h)
	return nil
}

// ---- store: ConsumeToken ----

func issueRawToken(t *testing.T, s *store.Store) *models.Token {
	t.Helper()
	tok := &models.Token{Kind: "test", Subject: "u1", LookupHash: "lh", TokenHash: "th", KeyVersion: 1,
		ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, s.CreateToken(context.Background(), tok))
	return tok
}

func consumeConcurrently(t *testing.T, s *store.Store, id string, n int) (wins int, lost int) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ConsumeToken(context.Background(), id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errs.IsCode(err, errs.CodeTokenAlreadyConsumed):
				lost++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	return wins, lost
}

func TestConsumeTokenExactlyOnce(t *testing.T) {
	s := store.New(sessionsTokensDB(t))
	tok := issueRawToken(t, s)
	wins, lost := consumeConcurrently(t, s, tok.ID, 20)
	assert.Equal(t, 1, wins, "exactly one consumer wins")
	assert.Equal(t, 19, lost)
}

func TestConsumeTokenExactlyOncePostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Postgres container")
	}
	ctx := context.Background()
	db, cleanup := testutils.SetupPostgresTestDB(t, ctx)
	t.Cleanup(cleanup)
	require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 30*time.Second, 200*time.Millisecond)
	_, err := db.ExecContext(ctx, `CREATE TABLE tokens (
		id TEXT PRIMARY KEY, kind TEXT NOT NULL, subject TEXT, lookup_hash TEXT NOT NULL, token_hash TEXT NOT NULL,
		key_version INTEGER NOT NULL, metadata TEXT, expires_at TIMESTAMPTZ, consumed_at TIMESTAMPTZ,
		revoked_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL)`)
	require.NoError(t, err)

	s := store.New(pgAdapter.NewPostgresAdapter(db, nil))
	for round := 0; round < 5; round++ {
		tok := issueRawToken(t, s)
		wins, lost := consumeConcurrently(t, s, tok.ID, 20)
		assert.Equal(t, 1, wins, "round %d: exactly one consumer wins", round)
		assert.Equal(t, 19, lost, "round %d", round)
	}
}

func TestConsumeAndRevokeUnknownToken(t *testing.T) {
	s := store.New(sessionsTokensDB(t))
	_, err := s.ConsumeToken(context.Background(), "no-such-token")
	assert.True(t, errs.IsNotFound(err), "an unknown token is NotFound, not already consumed: %v", err)
	err = s.RevokeToken(context.Background(), "no-such-token")
	assert.True(t, errs.IsNotFound(err), "%v", err)
}

// ---- session manager ----

func newSessionManager(t *testing.T, cfg core.SessionConfig) (core.SessionManager, *passDispatcher) {
	t.Helper()
	d := &passDispatcher{}
	if cfg.ExpiresIn == 0 {
		cfg.ExpiresIn = time.Hour
	}
	if cfg.PendingExpiresIn == 0 {
		cfg.PendingExpiresIn = 5 * time.Minute
	}
	return auth.NewSessionManager(store.New(sessionsTokensDB(t)), nil, testCrypto(t), cfg, d, nil, managersAuth), d
}

// managersAuth is the AuthContext the managers under test are built with. It
// only has to be the pointer handlers get back as HookContext.Auth.
var managersAuth = &core.AuthContext{}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	sm, _ := newSessionManager(t, core.SessionConfig{})

	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID, "the store assigned the id")
	assert.False(t, sess.CreatedAt.IsZero(), "the store stamped created_at")

	got, err := sm.Get(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	_, err = sm.Validate(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionPending), "%v", err)

	promoted, promotedRaw, err := sm.Promote(ctx, sess.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.Equal(t, models.SessionActive, promoted.State)
	assert.True(t, promoted.UpdatedAt.After(sess.UpdatedAt) || promoted.UpdatedAt.Equal(sess.UpdatedAt), "updated_at stamped")
	_, err = sm.Validate(ctx, promotedRaw)
	require.NoError(t, err)

	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
	_, err = sm.Get(ctx, promotedRaw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
	listed, err := sm.ListForUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "user_logout", listed[0].RevokedReason)
	assert.NotNil(t, listed[0].RevokedAt)
}

func TestSessionTouchExtendsExpiry(t *testing.T) {
	ctx := context.Background()
	sm, _ := newSessionManager(t, core.SessionConfig{UpdateAge: 2 * time.Hour}) // always due
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	extended, err := sm.Touch(ctx, sess)
	require.NoError(t, err)
	require.NotNil(t, extended, "Touch returns the session it extended")
	got, err := sm.Get(ctx, raw)
	require.NoError(t, err)
	assert.True(t, got.ExpiresAt.After(sess.ExpiresAt), "expiry moved forward")
	assert.True(t, extended.ExpiresAt.Equal(got.ExpiresAt), "and that is the expiry Touch reports")

	// Not due: with UpdateAge at zero a session is never extended, and
	// Touch says so by returning no session.
	fixed, _ := newSessionManager(t, core.SessionConfig{})
	sess, _, err = fixed.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	extended, err = fixed.Touch(ctx, sess)
	require.NoError(t, err)
	assert.Nil(t, extended)
	extended, err = fixed.Touch(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, extended, "no session, nothing to extend")
}

// Touch runs on every authenticated request. It decides from the session it
// is handed, and goes to the database only when that session is due. The row
// then has the last word, because the copy may be older than the row.
func TestSessionTouchDecidesFromTheSessionItIsGiven(t *testing.T) {
	ctx := context.Background()
	sm, _ := newSessionManager(t, core.SessionConfig{UpdateAge: 30 * time.Minute}) // due in the second half of the hour a session lasts

	// Not due: the row is not looked for. A session that was never stored
	// shows it, since reading it would fail.
	unstored := &models.Session{ID: "not-in-the-database", State: models.SessionActive, ExpiresAt: time.Now().Add(time.Hour)}
	extended, err := sm.Touch(ctx, unstored)
	require.NoError(t, err, "a session that is not due costs no read")
	assert.Nil(t, extended)

	// Due by the copy, not by the row: another request has extended the
	// session already. Nothing is written.
	fresh, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	stale := *fresh
	stale.ExpiresAt = time.Now().Add(time.Minute)
	extended, err = sm.Touch(ctx, &stale)
	require.NoError(t, err)
	assert.Nil(t, extended, "the row is not due")
	stored, err := sm.Get(ctx, raw)
	require.NoError(t, err)
	assert.True(t, stored.ExpiresAt.Equal(fresh.ExpiresAt), "the expiry is where it was")

	// Due by the copy, and the row has been revoked since.
	require.NoError(t, sm.Revoke(ctx, fresh.ID, "test"))
	extended, err = sm.Touch(ctx, &stale)
	require.NoError(t, err)
	assert.Nil(t, extended, "a revoked session is not extended")

	// Due by the copy and by the row is TestSessionTouchExtendsExpiry.
}

// interleavingKV is a session cache that runs beforeSet once, at the next
// write, before the write lands. It puts a concurrent operation between a
// manager's database read or write and the cache write that follows it,
// without depending on timing.
type interleavingKV struct {
	*memoryKV
	beforeSet func()
}

func (k *interleavingKV) interleave() {
	if f := k.beforeSet; f != nil {
		k.beforeSet = nil
		f()
	}
}

func (k *interleavingKV) Set(ctx context.Context, key, value string, ttl int) error {
	k.interleave()
	return k.memoryKV.Set(ctx, key, value, ttl)
}

func (k *interleavingKV) SetIfNewer(ctx context.Context, key, value string, version int64, ttl time.Duration) (bool, error) {
	k.interleave()
	return k.memoryKV.SetIfNewer(ctx, key, value, version, ttl)
}

// A revoke that lands while a cache miss is being served takes effect. Get
// reads the row and then writes it to the cache, and the revoke updates the
// row and the cache between the two. The revoked copy has the later
// version, so the active copy the Get read can't replace it. See ADR-0011.
func TestSessionRevokedDuringACacheMissIsRefused(t *testing.T) {
	ctx := context.Background()
	kv := &interleavingKV{memoryKV: &memoryKV{}}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour}, &passDispatcher{}, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	kv.clear() // the entry Create wrote is evicted, so the next Get misses

	kv.beforeSet = func() { require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout")) }
	_, err = sm.Get(ctx, raw)
	require.NoError(t, err, "the Get that read the row before the revoke succeeds")
	require.Nil(t, kv.beforeSet, "the revoke ran between the read and the cache write")

	assert.Equal(t, map[string]models.SessionState{sess.ID: models.SessionRevoked}, sessionStates(t, sm, "u1"), "the row is revoked")
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "a Get after the revoke is refused: %v", err)
}

// The same race through Touch, which needs no cache miss: it writes the row
// it extended to the cache, and a revoke lands between its write and the
// cache write. See ADR-0011.
func TestSessionRevokedDuringATouchIsRefused(t *testing.T) {
	ctx := context.Background()
	kv := &interleavingKV{memoryKV: &memoryKV{}}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, UpdateAge: 2 * time.Hour}, // always due
		&passDispatcher{}, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	_, err = sm.Get(ctx, raw)
	require.NoError(t, err, "the session is in the cache")

	kv.beforeSet = func() { require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout")) }
	extended, err := sm.Touch(ctx, sess)
	require.NoError(t, err)
	require.NotNil(t, extended, "Touch extended the session before the revoke")
	require.Nil(t, kv.beforeSet, "the revoke ran between Touch's write and the cache write")

	assert.Equal(t, map[string]models.SessionState{sess.ID: models.SessionRevoked}, sessionStates(t, sm, "u1"), "the row is revoked")
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "a Get after the revoke is refused: %v", err)
}

// A Get of the pending token that read the session before its promotion
// can't put the pending copy back. The promotion leaves a gone value under
// the pending token, of a later version than the copy, which lives as long
// as the pending session would have. See ADR-0011.
func TestSessionPromotedDuringACacheMissStaysGone(t *testing.T) {
	ctx := context.Background()
	kv := &interleavingKV{memoryKV: &memoryKV{}}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute}, &passDispatcher{}, nil, managersAuth)
	pending, pendingRaw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	kv.clear()

	var raw string
	kv.beforeSet = func() {
		var err error
		_, raw, err = sm.Promote(ctx, pending.ID, core.FactorTOTP)
		require.NoError(t, err)
	}
	_, err = sm.Get(ctx, pendingRaw)
	require.NoError(t, err, "the Get that read the row before the promotion succeeds")
	require.Nil(t, kv.beforeSet, "the promotion ran between the read and the cache write")

	_, err = sm.Get(ctx, pendingRaw)
	assert.True(t, errs.IsNotFound(err), "the pending token stops working: %v", err)
	assert.Nil(t, kv.session(pending.LookupHash), "no copy of the session under the pending token")
	assert.Greater(t, kv.ttl(pending.LookupHash), 4*time.Minute)
	assert.LessOrEqual(t, kv.ttl(pending.LookupHash), 5*time.Minute, "the gone value lives as long as the pending session would have")
	_, err = sm.Validate(ctx, raw)
	assert.NoError(t, err, "the new token works")
}

// A Get that read a session before a Touch can't replace the extended copy
// with the earlier expiry, so the cache never expires a session sooner than
// its row does. See ADR-0011.
func TestSessionExtendedDuringACacheMissKeepsItsExpiry(t *testing.T) {
	ctx := context.Background()
	kv := &interleavingKV{memoryKV: &memoryKV{}}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, UpdateAge: 2 * time.Hour}, // always due
		&passDispatcher{}, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	kv.clear()

	var extended *models.Session
	kv.beforeSet = func() {
		var err error
		extended, err = sm.Touch(ctx, sess)
		require.NoError(t, err)
		require.NotNil(t, extended)
	}
	_, err = sm.Get(ctx, raw)
	require.NoError(t, err)
	require.Nil(t, kv.beforeSet, "the Touch ran between the Get's read and its cache write")

	cached := kv.session(sess.LookupHash)
	require.NotNil(t, cached)
	assert.Equal(t, extended.Version, cached.Version, "the extended copy stays")
	assert.True(t, cached.ExpiresAt.Equal(extended.ExpiresAt), "with the extended expiry")
}

// Every update of a session row writes the next version, and an update
// from an earlier version is refused. Of several Touches of one session at
// once, one extends it, and the others give up. See ADR-0011.
func TestSessionUpdatesAreVersioned(t *testing.T) {
	ctx := context.Background()
	st := store.New(sessionsTokensDB(t))
	sm := auth.NewSessionManager(st, nil, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute, UpdateAge: 2 * time.Hour}, // always due
		&passDispatcher{}, nil, managersAuth)

	pending, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	assert.Zero(t, pending.Version)
	extended, err := sm.Touch(ctx, pending)
	require.NoError(t, err)
	assert.EqualValues(t, 1, extended.Version, "a Touch")
	promoted, _, err := sm.Promote(ctx, pending.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.EqualValues(t, 2, promoted.Version, "a promotion")
	require.NoError(t, sm.Revoke(ctx, pending.ID, "user_logout"))
	revoked, err := st.FindSessionByID(ctx, pending.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 3, revoked.Version, "a revoke")

	_, err = st.UpdateSessionAt(ctx, pending.ID, 2, behemoth.M{models.SessionRevokedReason: "late"})
	assert.True(t, errs.IsNotFound(err), "an update from an earlier version: %v", err)
	_, err = st.UpdateSessionAt(ctx, pending.ID, 3, behemoth.M{models.SessionVersion: int64(9)})
	assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "the store writes the version: %v", err)

	// A session in its last minute, with extensions due in the last half
	// hour: once extended, it is not due again.
	rolling := auth.NewSessionManager(st, nil, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, UpdateAge: 30 * time.Minute}, &passDispatcher{}, nil, managersAuth)
	sess, _, err := rolling.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	sess, err = st.UpdateSessionAt(ctx, sess.ID, sess.Version, behemoth.M{models.SessionExpiresAt: st.Now().Add(time.Minute)})
	require.NoError(t, err)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			extended, err := rolling.Touch(ctx, sess)
			assert.NoError(t, err, "a Touch that loses gives up without an error")
			if extended != nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load(), "one Touch extends the session")
	stored, err := st.FindSessionByID(ctx, sess.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, stored.Version, "the expiry set above, and the one extension")
}

// interceptDispatcher is a passDispatcher that runs a function once, at the
// first before chain of a point: a concurrent update that lands between a
// manager's read of a session and its write.
type interceptDispatcher struct {
	passDispatcher
	once map[core.HookPoint]func()
}

func (d *interceptDispatcher) RunBefore(h *core.HookContext, point core.HookPoint, p behemoth.M) (behemoth.M, error) {
	if f := d.once[point]; f != nil {
		delete(d.once, point)
		f()
	}
	return d.passDispatcher.RunBefore(h, point, p)
}

// fired counts the hook points the dispatcher ran for point.
func (d *passDispatcher) fired(point core.HookPoint) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, h := range d.seen {
		if h.Point == point {
			n++
		}
	}
	return n
}

// A revoke whose write loses to a concurrent update of its session reads the
// session again and retries: the session ends revoked, and the handlers of
// auth.session.afterRevoke hear of it once.
func TestSessionRevokeRetriesAfterAConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	kv := &memoryKV{}
	d := &interceptDispatcher{}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, UpdateAge: 2 * time.Hour}, d, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	d.once = map[core.HookPoint]func(){hooks.HookSessionBeforeRevoke: func() {
		extended, err := sm.Touch(ctx, sess)
		require.NoError(t, err)
		require.NotNil(t, extended, "a Touch lands between the revoke's read and its write")
	}}

	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
	assert.Equal(t, map[string]models.SessionState{sess.ID: models.SessionRevoked}, sessionStates(t, sm, "u1"))
	assert.Equal(t, models.SessionRevoked, kv.session(sess.LookupHash).State, "the cache holds the revoked copy")
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
	assert.Equal(t, 1, d.fired(hooks.HookSessionAfterRevoke))

	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"), "a second revoke has nothing to do")
	assert.Equal(t, 1, d.fired(hooks.HookSessionAfterRevoke), "and reports nothing")
}

// A promotion whose write loses to a concurrent update of its session reads
// the session again and retries while it is still pending. Here the update
// is a Touch from a handler of the revoke that makes room for the session.
func TestSessionPromotionRetriesAfterAConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	d := &interceptDispatcher{}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), &memoryKV{}, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute, UpdateAge: 2 * time.Hour,
			MaxConcurrent: 1, EvictOldestOnLimit: true},
		d, nil, managersAuth)
	oldest, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	pending, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	d.once = map[core.HookPoint]func(){hooks.HookSessionBeforeRevoke: func() {
		extended, err := sm.Touch(ctx, pending)
		require.NoError(t, err)
		require.NotNil(t, extended, "a Touch lands between the promotion's read and its write")
	}}

	promoted, raw, err := sm.Promote(ctx, pending.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.Equal(t, models.SessionActive, promoted.State)
	assert.EqualValues(t, 2, promoted.Version, "written after the Touch's version")
	assert.Equal(t, map[string]models.SessionState{oldest.ID: models.SessionRevoked, pending.ID: models.SessionActive}, sessionStates(t, sm, "u1"))
	_, err = sm.Validate(ctx, raw)
	assert.NoError(t, err)
}

// interleavingRedis is interleavingKV over the Redis adapter.
type interleavingRedis struct {
	*redisAdapter.RedisAdapter
	beforeSet func()
}

func (k *interleavingRedis) SetIfNewer(ctx context.Context, key, value string, version int64, ttl time.Duration) (bool, error) {
	if f := k.beforeSet; f != nil {
		k.beforeSet = nil
		f()
	}
	return k.RedisAdapter.SetIfNewer(ctx, key, value, version, ttl)
}

// Over the Redis adapter, a revoke during a cache miss takes effect as it
// does over the test's memory cache: the adapter's versioned values keep the
// revoked copy. See ADR-0011.
func TestSessionCacheOnRedis(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Redis container")
	}
	ctx := context.Background()
	client, cleanup := testutils.SetupRedisClient(t, ctx)
	t.Cleanup(cleanup)
	kv := &interleavingRedis{RedisAdapter: redisAdapter.NewRedisAdapter(client)}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour}, &passDispatcher{}, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	key := "session:" + sess.LookupHash
	_, version, err := kv.GetVersioned(ctx, key)
	require.NoError(t, err, "the new session is cached")
	assert.Zero(t, version)

	require.NoError(t, client.Del(ctx, key).Err()) // evicted, so the next Get misses
	kv.beforeSet = func() { require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout")) }
	_, err = sm.Get(ctx, raw)
	require.NoError(t, err, "the Get that read the row before the revoke succeeds")
	require.Nil(t, kv.beforeSet, "the revoke ran between the read and the cache write")

	_, version, err = kv.GetVersioned(ctx, key)
	require.NoError(t, err)
	assert.EqualValues(t, 1, version, "the revoked copy stays")
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
}

// A key-value storage without core.VersionedCache can't order the cache's
// writes. The manager then caches a session for UnversionedCacheTTL at
// most, and ends an entry by deleting it. The race of ADR-0011 stays
// possible there, bounded by that lifetime.
func TestSessionCacheWithoutVersions(t *testing.T) {
	ctx := context.Background()
	mem := &memoryKV{}
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), plainKV{mem}, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Second}, &passDispatcher{}, nil, managersAuth)
	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NotNil(t, mem.session(sess.LookupHash))
	assert.Equal(t, core.DefaultUnversionedCacheTTL, mem.ttl(sess.LookupHash), "the default lifetime")
	_, _, err = mem.GetVersioned(ctx, "session:"+sess.LookupHash)
	assert.Error(t, err, "written without a version")
	_, err = sm.Validate(ctx, raw)
	require.NoError(t, err)
	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
	assert.Nil(t, mem.session(sess.LookupHash), "a revoke deletes the entry")

	pending, pendingRaw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	assert.LessOrEqual(t, mem.ttl(pending.LookupHash), 5*time.Second, "a session that ends sooner is cached until it ends")
	_, _, err = sm.Promote(ctx, pending.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.Nil(t, mem.session(pending.LookupHash), "a promotion deletes the pending token's entry")
	_, err = sm.Get(ctx, pendingRaw)
	assert.True(t, errs.IsNotFound(err), "%v", err)

	kv := &interleavingKV{memoryKV: &memoryKV{}}
	capped := auth.NewSessionManager(store.New(sessionsTokensDB(t)), plainKV{kv}, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, UnversionedCacheTTL: 10 * time.Second}, &passDispatcher{}, nil, managersAuth)
	sess, raw, err = capped.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	kv.clear()
	kv.beforeSet = func() { require.NoError(t, capped.Revoke(ctx, sess.ID, "user_logout")) }
	_, err = capped.Get(ctx, raw)
	require.NoError(t, err)
	require.Nil(t, kv.beforeSet)
	stale := kv.session(sess.LookupHash)
	require.NotNil(t, stale, "the copy read before the revoke went back in")
	assert.Equal(t, models.SessionActive, stale.State)
	assert.Equal(t, 10*time.Second, kv.ttl(sess.LookupHash), "for the lifetime the application chose")
}

// RequireSession refuses a request with a typed session error and writes no
// response of its own, so the router answers every refusal the same way: 401
// with a public message and a code. A failure of the system is not a
// refusal. It passes through as it is, and the router answers it with 500
// and keeps its text from the client.
func TestRequireSessionRefusesWithTypedErrors(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "refusals.db")+"?_foreign_keys=on&_busy_timeout=10000")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	_, err = sqlDB.Exec(sessionsTokensSQLite + testutils.AuditLogSQLiteSchema)
	require.NoError(t, err)
	sm := auth.NewSessionManager(store.New(sqliteAdapter.NewSQLiteAdapter(sqlDB, nil)), nil, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		&passDispatcher{}, nil, managersAuth)

	reached := 0
	handler := requireSession(sm, httpapi.DeliveryHeader, func(*httpapi.RequestContext) error {
		reached++
		return nil
	})
	// request returns what the router would answer with: the status and
	// body its error mapper gives the middleware's error.
	request := func(token string) (rctx *httpapi.RequestContext, status int, body behemoth.M, err error) {
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rctx = &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		if err = handler(rctx); err != nil {
			status, body = (&httpapi.DefaultErrorMapper{}).Map(err)
		}
		return rctx, status, body, err
	}

	_, liveToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	revoked, revokedToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NoError(t, sm.Revoke(ctx, revoked.ID, "test"))
	_, pendingToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)

	for name, tc := range map[string]struct{ token, code, message string }{
		"no token":                    {"", errs.CodeSessionMissing, "missing session token"},
		"a token of no session":       {"never-issued", errs.CodeSessionInvalid, "invalid session"},
		"a revoked session":           {revokedToken, errs.CodeSessionRevoked, "session has been revoked"},
		"waiting for a second factor": {pendingToken, errs.CodeSessionPending, "additional verification required"},
	} {
		rctx, status, body, err := request(tc.token)
		require.Error(t, err, name)
		assert.True(t, errs.IsCategory(err, errs.CategorySession), "%s: a session error: %v", name, err)
		assert.Equal(t, http.StatusUnauthorized, status, name)
		assert.Equal(t, behemoth.M{"error": tc.message, "code": tc.code}, body, name)
		assert.Equal(t, http.StatusOK, rctx.Response.Code, "%s: the middleware wrote no response of its own", name)
		assert.Empty(t, rctx.Response.Headers, name)
	}
	assert.Zero(t, reached, "no refused request reached the handler")

	_, _, _, err = request(liveToken)
	require.NoError(t, err)
	assert.Equal(t, 1, reached)

	// The database fails. The session may be perfectly good, so this is not
	// a 401: a client would take that for the end of its session.
	require.NoError(t, sqlDB.Close())
	_, status, body, err := request(liveToken)
	require.Error(t, err)
	assert.False(t, errs.IsCategory(err, errs.CategorySession), "not a refusal: %v", err)
	assert.True(t, errs.IsCategory(err, errs.CategoryDatabase), "the store's error, unchanged: %v", err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, behemoth.M{"error": "an internal error occurred", "code": "database_error"}, body, "the error's own text stays on the server")
	assert.Equal(t, 1, reached, "the request did not reach the handler")
}

// A session can end in another request: it is revoked from another device,
// it expires, or it is deleted with its user. The browser still holds the
// cookie, and can't drop an HttpOnly cookie itself. The session check takes
// it back with the refusal. A cookie that may still be needed stays.
func TestRequireSessionTakesBackTheCookieOfASessionThatIsGone(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "gone.db")+"?_foreign_keys=on&_busy_timeout=10000")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	_, err = sqlDB.Exec(sessionsTokensSQLite + testutils.AuditLogSQLiteSchema)
	require.NoError(t, err)
	// Every manager shares the database, so one can create what another
	// is asked about.
	manager := func(cfg core.SessionConfig) core.SessionManager {
		if cfg.ExpiresIn == 0 {
			cfg.ExpiresIn = time.Hour
		}
		cfg.PendingExpiresIn = time.Minute
		return auth.NewSessionManager(store.New(sqliteAdapter.NewSQLiteAdapter(sqlDB, nil)), nil, testCrypto(t), cfg, &passDispatcher{}, nil, managersAuth)
	}
	// request sends cookie and bearer, carried the way delivery says,
	// through the middleware mw builds, and returns the response and the
	// middleware's error.
	request := func(sm core.SessionManager, delivery httpapi.TokenDelivery, mw func(core.SessionManager) httpapi.Middleware, cookie, bearer string) (*httpapi.RequestContext, error) {
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "session_token", Value: cookie})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		return rctx, authenticated(sm, delivery, mw(sm)(func(*httpapi.RequestContext) error { return nil }))(rctx)
	}
	// requireSessionMW is RequireSession in the form request takes.
	requireSessionMW := func(core.SessionManager) httpapi.Middleware { return httpapi.RequireSession() }
	// removed reports whether the response removes the session cookie.
	removed := func(rctx *httpapi.RequestContext) bool {
		set := (&http.Response{Header: rctx.Response.Headers}).Cookies()
		return len(set) == 1 && set[0].Name == "session_token" && set[0].Value == "" && set[0].MaxAge < 0
	}

	sm := manager(core.SessionConfig{})
	revoked, revokedToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NoError(t, sm.Revoke(ctx, revoked.ID, "revoked_elsewhere"))
	_, expiredToken, err := manager(core.SessionConfig{ExpiresIn: 20 * time.Millisecond}).Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)
	_, pendingToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	_, liveToken, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)

	// Gone for good: the cookie goes with the refusal.
	for name, tc := range map[string]struct{ token, code string }{
		"revoked":      {revokedToken, errs.CodeSessionRevoked},
		"expired":      {expiredToken, errs.CodeSessionExpired},
		"never issued": {"never-issued", errs.CodeSessionInvalid},
	} {
		rctx, err := request(sm, "", requireSessionMW, tc.token, "")
		assert.True(t, errs.IsCode(err, tc.code), "%s: %v", name, err)
		assert.True(t, removed(rctx), "%s: the cookie is removed: %q", name, rctx.Response.Headers.Values("Set-Cookie"))
	}

	// Not gone: the client still needs its cookie, or has none.
	rctx, err := request(sm, "", requireSessionMW, pendingToken, "")
	assert.True(t, errs.IsCode(err, errs.CodeSessionPending), "%v", err)
	assert.Empty(t, rctx.Response.Headers, "a session that waits for its second factor keeps its cookie")

	rctx, err = request(sm, "", requireSessionMW, "", "")
	assert.True(t, errs.IsCode(err, errs.CodeSessionMissing), "%v", err)
	assert.Empty(t, rctx.Response.Headers, "no cookie came, none is removed")

	neverFresh := manager(core.SessionConfig{FreshAge: time.Nanosecond})
	rctx, err = request(neverFresh, "", httpapi.RequireFreshSession, liveToken, "")
	assert.True(t, errs.IsCode(err, errs.CodeSessionNotFresh), "%v", err)
	assert.Empty(t, rctx.Response.Headers, "a session that is only too old for this route is still live")

	// Both deliveries: the bearer token is read first. A dead one says
	// nothing about the cookie next to it, which is another session's.
	rctx, err = request(sm, httpapi.DeliveryBoth, requireSessionMW, liveToken, revokedToken)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
	assert.Empty(t, rctx.Response.Headers, "the live session's cookie stays")
	rctx, err = request(sm, httpapi.DeliveryBoth, requireSessionMW, revokedToken, "")
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
	assert.True(t, removed(rctx), "the dead session's own cookie goes")

	// The database fails: nothing is known about the session, so nothing
	// is taken from the client.
	require.NoError(t, sqlDB.Close())
	rctx, err = request(sm, "", requireSessionMW, liveToken, "")
	require.Error(t, err)
	assert.True(t, errs.IsCategory(err, errs.CategoryDatabase), "%v", err)
	assert.Empty(t, rctx.Response.Headers, "a failed lookup removes no cookie")
}

// RequireSession runs on every authenticated request, so what it costs
// matters. With a session cache, a request whose session is not due for an
// extension is served without a statement to the database. Without a cache
// it costs the one read that validates the token.
func TestRequireSessionReadsNoMoreThanItNeeds(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		cfg    core.SessionConfig
		cached bool
		want   int // statements one request runs
	}{
		"cache, rolling off":         {core.SessionConfig{}, true, 0},
		"cache, rolling on, not due": {core.SessionConfig{UpdateAge: 30 * time.Minute}, true, 0},
		"no cache, rolling off":      {core.SessionConfig{}, false, 1},
		"no cache, not due":          {core.SessionConfig{UpdateAge: 30 * time.Minute}, false, 1},
	} {
		// The adapter logs each statement at debug, which is how they are
		// counted.
		_, rec := telemetrytest.New()
		sqlDB, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "cost.db")+"?_foreign_keys=on&_busy_timeout=10000")
		require.NoError(t, err, name)
		t.Cleanup(func() { sqlDB.Close() })
		_, err = sqlDB.Exec(sessionsTokensSQLite + testutils.AuditLogSQLiteSchema)
		require.NoError(t, err, name)
		var kv behemoth.KeyValueStorage
		if tc.cached {
			kv = &memoryKV{entries: map[string]string{}}
		}
		tc.cfg.ExpiresIn = time.Hour
		sm := auth.NewSessionManager(store.New(sqliteAdapter.NewSQLiteAdapter(sqlDB, nil).WithLogger(rec.Logger)),
			kv, testCrypto(t), tc.cfg, &passDispatcher{}, nil, managersAuth)
		_, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
		require.NoError(t, err, name)
		require.NotEmpty(t, rec.Logger.At(slog.LevelDebug), "%s: creating the session ran statements, so they are being counted", name)

		reached := false
		handler := requireSession(sm, "", func(*httpapi.RequestContext) error {
			reached = true
			return nil
		})
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.AddCookie(&http.Cookie{Name: "session_token", Value: raw})
		rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		rec.Logger.Reset()
		require.NoError(t, handler(rctx), name)
		require.True(t, reached, name)
		assert.Len(t, rec.Logger.At(slog.LevelDebug), tc.want, "%s: statements run by one authenticated request", name)
	}
}

func TestSessionRevokeAllAndEviction(t *testing.T) {
	ctx := context.Background()
	sm, _ := newSessionManager(t, core.SessionConfig{MaxConcurrent: 2, EvictOldestOnLimit: true})
	first, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	second, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	third, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)

	states := func() map[string]models.SessionState {
		listed, err := sm.ListForUser(ctx, "u1")
		require.NoError(t, err)
		out := map[string]models.SessionState{}
		for _, s := range listed {
			out[s.ID] = s.State
		}
		return out
	}
	assert.Equal(t, map[string]models.SessionState{
		first.ID: models.SessionRevoked, second.ID: models.SessionActive, third.ID: models.SessionActive,
	}, states(), "the oldest session was evicted at the limit")

	require.NoError(t, sm.RevokeAllForUser(ctx, "u1", "password_changed", third.ID))
	assert.Equal(t, map[string]models.SessionState{
		first.ID: models.SessionRevoked, second.ID: models.SessionRevoked, third.ID: models.SessionActive,
	}, states(), "everything but the excepted session is revoked")
}

// The session limit counts the sessions a user can use: active ones that
// have not expired. A pending session neither counts nor makes room, so
// whoever has only the password can't end the user's sessions, and an
// expired session leaves its place. The test runs east of UTC: SQLite stores
// a time as text with its zone, so expiries stamped in local time would
// compare wrongly with the store's clock.
func TestSessionLimitCountsActiveSessionsOnly(t *testing.T) {
	inZone(t, time.FixedZone("UTC+3", 3*60*60))
	ctx := context.Background()
	active := core.SessionMeta{State: models.SessionActive}
	pending := core.SessionMeta{State: models.SessionPending}

	t.Run("an expired session leaves its place", func(t *testing.T) {
		sm, _ := newSessionManager(t, core.SessionConfig{MaxConcurrent: 1, ExpiresIn: 50 * time.Millisecond})
		_, _, err := sm.Create(ctx, "u1", active)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		_, _, err = sm.Create(ctx, "u1", active)
		assert.NoError(t, err)
	})

	t.Run("eviction picks among the sessions that count", func(t *testing.T) {
		// Two managers over one store: sessions of the first expire at once.
		st := store.New(sessionsTokensDB(t))
		brief := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{ExpiresIn: 50 * time.Millisecond}, &passDispatcher{}, nil, managersAuth)
		sm := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{MaxConcurrent: 1, EvictOldestOnLimit: true}, &passDispatcher{}, nil, managersAuth)
		expired, _, err := brief.Create(ctx, "u1", active)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		oldest, _, err := sm.Create(ctx, "u1", active)
		require.NoError(t, err)
		newest, _, err := sm.Create(ctx, "u1", active)
		require.NoError(t, err)
		assert.Equal(t, map[string]models.SessionState{
			expired.ID: models.SessionActive, oldest.ID: models.SessionRevoked, newest.ID: models.SessionActive,
		}, sessionStates(t, sm, "u1"), "the expired session doesn't count and is left alone")
	})

	t.Run("a pending session doesn't count", func(t *testing.T) {
		sm, _ := newSessionManager(t, core.SessionConfig{MaxConcurrent: 1})
		_, _, err := sm.Create(ctx, "u1", pending)
		require.NoError(t, err)
		_, _, err = sm.Create(ctx, "u1", active)
		assert.NoError(t, err)
	})

	t.Run("a pending session makes no room, and its promotion does", func(t *testing.T) {
		sm, _ := newSessionManager(t, core.SessionConfig{MaxConcurrent: 1, EvictOldestOnLimit: true})
		first, _, err := sm.Create(ctx, "u1", active)
		require.NoError(t, err)
		waiting, _, err := sm.Create(ctx, "u1", pending)
		require.NoError(t, err)
		assert.Equal(t, map[string]models.SessionState{
			first.ID: models.SessionActive, waiting.ID: models.SessionPending,
		}, sessionStates(t, sm, "u1"), "a pending session evicts nothing")

		_, _, err = sm.Promote(ctx, waiting.ID, core.FactorTOTP)
		require.NoError(t, err)
		assert.Equal(t, map[string]models.SessionState{
			first.ID: models.SessionRevoked, waiting.ID: models.SessionActive,
		}, sessionStates(t, sm, "u1"), "the promotion evicts the oldest active session")
	})

	t.Run("without eviction a full limit refuses a pending session and a promotion", func(t *testing.T) {
		sm, _ := newSessionManager(t, core.SessionConfig{MaxConcurrent: 1})
		waiting, _, err := sm.Create(ctx, "u1", pending)
		require.NoError(t, err)
		_, _, err = sm.Create(ctx, "u1", active)
		require.NoError(t, err)

		_, _, err = sm.Create(ctx, "u1", pending)
		assert.True(t, errs.IsCode(err, errs.CodeSessionLimitReached), "refused before the second factor: %v", err)
		_, _, err = sm.Promote(ctx, waiting.ID, core.FactorTOTP)
		assert.True(t, errs.IsCode(err, errs.CodeSessionLimitReached), "%v", err)
		assert.Equal(t, models.SessionPending, sessionStates(t, sm, "u1")[waiting.ID], "a refused promotion leaves the session pending")
	})
}

// A session records the factor its sign-in checked, and keeps it in the
// database, in the session cache, and on the caller, a pending caller
// included. A name that is not lowercase snake case is refused: the column
// holds the names separated by spaces.
func TestSessionRecordsItsFirstFactor(t *testing.T) {
	ctx := context.Background()
	st := store.New(sessionsTokensDB(t))
	sm := auth.NewSessionManager(st, &memoryKV{entries: map[string]string{}}, testCrypto(t), core.SessionConfig{}, &passDispatcher{}, nil, managersAuth)
	password := []string{core.FactorPassword}

	created, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending, Factor: core.FactorPassword})
	require.NoError(t, err)
	assert.Equal(t, password, created.Factors)
	cached, err := sm.Get(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, password, cached.Factors, "from the session cache")
	stored, err := st.FindSessionByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, password, stored.Factors, "from the database")

	caller, _ := auth.NewAuthenticator(sm, nil).Authenticate(ctx, &core.Credential{Kind: core.CredentialSession, Value: raw}, identity.Peer{})
	ref, ok := caller.Session()
	require.True(t, ok, "a pending session stays on the caller")
	assert.Equal(t, password, ref.Factors)

	none, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	stored, err = st.FindSessionByID(ctx, none.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.Factors, "without a factor none is recorded")

	for _, name := range []string{"Password", "pass word", "pass-word", "2fa", strings.Repeat("a", 33)} {
		_, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive, Factor: name})
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%q: %v", name, err)
	}
}

// Promotion gives the session a new token and records the second factor.
// The pending token stops working, from the database and from the session
// cache, so a copy taken before the second factor never authenticates the
// active session. Of several promotions of one session, one wins.
func TestSessionPromotionRenewsTheToken(t *testing.T) {
	ctx := context.Background()
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), &memoryKV{entries: map[string]string{}}, testCrypto(t), core.SessionConfig{}, &passDispatcher{}, nil, managersAuth)
	both := []string{core.FactorPassword, core.FactorTOTP}

	pending, pendingRaw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending, Factor: core.FactorPassword})
	require.NoError(t, err)
	promoted, raw, err := sm.Promote(ctx, pending.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.NotEqual(t, pendingRaw, raw)
	assert.Equal(t, pending.ID, promoted.ID, "the same session")
	assert.Equal(t, both, promoted.Factors)
	assert.True(t, promoted.ExpiresAt.After(pending.ExpiresAt), "the active session's expiry")

	_, err = sm.Get(ctx, pendingRaw)
	assert.True(t, errs.IsNotFound(err), "the pending token stops working, in the cache too: %v", err)
	caller, outcome := auth.NewAuthenticator(sm, nil).Authenticate(ctx, &core.Credential{Kind: core.CredentialSession, Value: pendingRaw}, identity.Peer{})
	assert.True(t, errs.IsCode(caller.AuthError(), errs.CodeSessionInvalid), "%v", caller.AuthError())
	assert.True(t, outcome.Ended, "a client takes the pending token back")
	active, err := sm.Validate(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, both, active.Factors)

	_, _, err = sm.Promote(ctx, pending.ID, core.FactorTOTP)
	assert.True(t, errs.IsCode(err, errs.CodeInvalidSessionState), "an active session is not promoted again: %v", err)

	racing, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending, Factor: core.FactorPassword})
	require.NoError(t, err)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := sm.Promote(ctx, racing.ID, core.FactorTOTP); err == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load(), "the write is guarded on the version, which pins the pending state")

	waiting, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	_, _, err = sm.Promote(ctx, waiting.ID, "TOTP")
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "the second factor's name is checked like the first's: %v", err)
}

// inZone sets the process's local time zone to zone for the rest of the
// test, so that a time taken with time.Now carries it.
func inZone(t *testing.T, zone *time.Location) {
	t.Helper()
	saved := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = saved })
}

// sessionStates returns the state of each of userID's sessions, by id.
func sessionStates(t *testing.T, sm core.SessionManager, userID string) map[string]models.SessionState {
	t.Helper()
	listed, err := sm.ListForUser(context.Background(), userID)
	require.NoError(t, err)
	states := map[string]models.SessionState{}
	for _, s := range listed {
		states[s.ID] = s.State
	}
	return states
}

// The manager's own hooks get a complete HookContext: the point and phase
// being dispatched, the AuthContext, the request being handled, and Values
// that last for one operation: a create's two points share a map, a revoke's
// two share another, and both start as a copy of the caller's operation.
func TestSessionHooksGetACompleteContext(t *testing.T) {
	sm, d := newSessionManager(t, core.SessionConfig{})
	rc := &httpapi.RequestContext{Request: httptest.NewRequest("POST", "/sign-in", nil)}
	flow := behemoth.M{"flow": "sign-in"}
	ctx := core.ContextWithHookValues(httpapi.ContextWithRequest(context.Background(), rc), flow)
	sess, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))

	type dispatch struct {
		point core.HookPoint
		phase core.HookPhase
	}
	var got []dispatch
	for _, h := range d.seen {
		got = append(got, dispatch{h.Point, h.Phase})
		assert.Same(t, managersAuth, h.Auth, "%s", h.Point)
		assert.Same(t, rc, httpapi.RequestFrom(h.Ctx), "%s", h.Point)
		assert.Nil(t, h.Tx, "%s: a Tier 2 point has no transaction", h.Point)
		require.NotNil(t, h.Values, "%s: a handler writing Values must not panic", h.Point)
		assert.Equal(t, "sign-in", h.Values["flow"], "%s: the caller's operation is visible", h.Point)
	}
	assert.Equal(t, []dispatch{
		{hooks.HookSessionBeforeCreate, core.BeforeHookPhase}, {hooks.HookSessionAfterCreate, core.AfterHookPhase},
		{hooks.HookSessionBeforeRevoke, core.BeforeHookPhase}, {hooks.HookSessionAfterRevoke, core.AfterHookPhase},
	}, got)
	beforeCreate, afterCreate, beforeRevoke, afterRevoke := d.seen[0], d.seen[1], d.seen[2], d.seen[3]
	beforeCreate.Values["note"] = "from beforeCreate"
	assert.Equal(t, "from beforeCreate", afterCreate.Values["note"], "a create's points share Values")
	beforeRevoke.Values["why"] = "from beforeRevoke"
	assert.Equal(t, "from beforeRevoke", afterRevoke.Values["why"], "a revoke's points share Values")
	assert.NotContains(t, beforeRevoke.Values, "note", "another operation's notes are not visible")
	assert.Equal(t, behemoth.M{"flow": "sign-in"}, flow, "nothing a nested operation writes reaches the caller's Values")
}

// A session records the IP address and user agent of the caller on the
// context. SessionMeta overrides them, a caller no transport authenticated
// leaves them empty, and nothing is recorded unless CaptureIPAndAgent is on.
func TestSessionCreateTakesIPAndUserAgentFromTheCaller(t *testing.T) {
	inRequest := identity.WithCaller(context.Background(),
		identity.Anonymous(identity.Peer{Transport: "http", IPAddress: "203.0.113.7", UserAgent: "test-agent/1.0"}))
	capture := core.SessionConfig{CaptureIPAndAgent: true}
	active := core.SessionMeta{State: models.SessionActive}

	t.Run("from the caller", func(t *testing.T) {
		sm, _ := newSessionManager(t, capture)
		sess, _, err := sm.Create(inRequest, "u1", active)
		require.NoError(t, err)
		assert.Equal(t, "203.0.113.7", sess.IPAddress)
		assert.Equal(t, "test-agent/1.0", sess.UserAgent)
	})
	t.Run("the caller's values win", func(t *testing.T) {
		sm, _ := newSessionManager(t, capture)
		sess, _, err := sm.Create(inRequest, "u1", core.SessionMeta{State: models.SessionActive, IPAddress: "192.0.2.1", UserAgent: "job"})
		require.NoError(t, err)
		assert.Equal(t, "192.0.2.1", sess.IPAddress)
		assert.Equal(t, "job", sess.UserAgent)
	})
	t.Run("no transport", func(t *testing.T) {
		sm, _ := newSessionManager(t, capture)
		sess, _, err := sm.Create(identity.WithCaller(context.Background(), identity.System("job").For("u1")), "u1", active)
		require.NoError(t, err, "a session can be created outside a request")
		assert.Empty(t, sess.IPAddress)
		assert.Empty(t, sess.UserAgent)
	})
	t.Run("capture off", func(t *testing.T) {
		sm, _ := newSessionManager(t, core.SessionConfig{})
		sess, _, err := sm.Create(inRequest, "u1", active)
		require.NoError(t, err)
		assert.Empty(t, sess.IPAddress)
		assert.Empty(t, sess.UserAgent)
	})
	t.Run("a before handler's rewrite is stored", func(t *testing.T) {
		sess, err := createWithRewrite(t, capture, inRequest)
		require.NoError(t, err)
		assert.Equal(t, "203.0.113.0", sess.IPAddress)
		assert.Equal(t, "masked", sess.UserAgent)
		assert.Equal(t, "u1", sess.UserID, "the user is not read back")
		assert.Equal(t, models.SessionActive, sess.State, "the state is not read back")
	})
	t.Run("capture off ignores a before handler's rewrite", func(t *testing.T) {
		sess, err := createWithRewrite(t, core.SessionConfig{}, inRequest)
		require.NoError(t, err)
		assert.Empty(t, sess.IPAddress)
		assert.Empty(t, sess.UserAgent)
	})
	// Over HTTP the authentication stage resolves the address, with the
	// router's proxy settings, and the session records what it resolved.
	signIn := func(t *testing.T, ipCfg *httpapi.ClientIPConfig) *models.Session {
		t.Helper()
		sm, _ := newSessionManager(t, capture)
		req := httptest.NewRequest(http.MethodPost, "/sign-in", nil)
		req.RemoteAddr = "203.0.113.7:51234"
		req.Header.Set("User-Agent", "test-agent/1.0")
		req.Header.Set("X-Forwarded-For", "198.51.100.9")
		var sess *models.Session
		stage := httpapi.AuthenticateRequests(auth.NewAuthenticator(sm, nil), httpapi.NewTokenCarrier(httpapi.TokenCarrierConfig{}), ipCfg)
		handler := stage(func(rctx *httpapi.RequestContext) error {
			var err error
			sess, _, err = sm.Create(rctx.Ctx, "u1", active)
			return err
		})
		rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, handler(rctx))
		return sess
	}
	t.Run("over HTTP", func(t *testing.T) {
		sess := signIn(t, nil)
		assert.Equal(t, "203.0.113.7", sess.IPAddress, "the direct peer, without its port; an untrusted peer's forwarding header is ignored")
		assert.Equal(t, "test-agent/1.0", sess.UserAgent)
	})
	t.Run("behind a trusted proxy", func(t *testing.T) {
		ipCfg, err := httpapi.NewClientConfig([]string{"203.0.113.0/24"}, "")
		require.NoError(t, err)
		assert.Equal(t, "198.51.100.9", signIn(t, ipCfg).IPAddress, "the client the proxy forwarded for")
	})
}

// rewriteDispatcher is a passDispatcher whose before chain rewrites every key
// of the auth.session.beforeCreate payload.
type rewriteDispatcher struct{ passDispatcher }

func (d *rewriteDispatcher) RunBefore(h *core.HookContext, point core.HookPoint, p behemoth.M) (behemoth.M, error) {
	if point == hooks.HookSessionBeforeCreate {
		p[hooks.HookValueIPAddress], p[hooks.HookValueUserAgent] = "203.0.113.0", "masked"
		p[hooks.HookValueUserID], p[hooks.HookValueState] = "someone-else", string(models.SessionPending)
	}
	return p, nil
}

// createWithRewrite creates an active session for u1 under a rewriteDispatcher.
func createWithRewrite(t *testing.T, cfg core.SessionConfig, ctx context.Context) (*models.Session, error) {
	t.Helper()
	cfg.ExpiresIn, cfg.PendingExpiresIn = time.Hour, time.Minute
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), nil, testCrypto(t), cfg,
		&rewriteDispatcher{}, nil, managersAuth)
	sess, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	return sess, err
}

// The token manager's hooks get the same: issue, a consume, and a failed
// second consume each dispatch with their own point and phase.
func TestTokenHooksGetACompleteContext(t *testing.T) {
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"}))
	d := &passDispatcher{}
	tm := auth.NewDefaultTokenManager(store.New(sessionsTokensDB(t)), nil, catalog, testCrypto(t), d, core.TokenConfig{}, managersAuth)

	ctx := context.Background()
	_, raw, err := tm.Issue(ctx, kindReset, "u1", nil)
	require.NoError(t, err)
	_, err = tm.Consume(ctx, kindReset, raw)
	require.NoError(t, err)
	_, err = tm.Consume(ctx, kindReset, raw)
	require.Error(t, err)

	type dispatch struct {
		point core.HookPoint
		phase core.HookPhase
	}
	var got []dispatch
	for _, h := range d.seen {
		got = append(got, dispatch{h.Point, h.Phase})
		assert.Same(t, managersAuth, h.Auth, "%s", h.Point)
		assert.NotNil(t, h.Values, "%s", h.Point)
		assert.Nil(t, httpapi.RequestFrom(h.Ctx), "%s: no request outside one", h.Point)
	}
	assert.Equal(t, []dispatch{
		{hooks.HookTokenBeforeIssue, core.BeforeHookPhase}, {hooks.HookTokenAfterIssue, core.AfterHookPhase},
		{hooks.HookTokenConsumed, core.AfterHookPhase},
		{hooks.HookTokenFailed, core.FailedHookPhase},
	}, got)
}

// ---- token manager ----

const kindReset models.TokenKind = "password_reset"
const kindAPI models.TokenKind = "api_key"

func newTokenManager(t *testing.T) (core.TokenManager, *store.Store) {
	t.Helper()
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"}))
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindAPI, Backend: core.TokenBackendDB, Owner: "core"}))
	st := store.New(sessionsTokensDB(t))
	return auth.NewDefaultTokenManager(st, nil, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth), st
}

func TestTokenIssueVerifyConsume(t *testing.T) {
	ctx := context.Background()
	tm, _ := newTokenManager(t)

	tok, raw, err := tm.Issue(ctx, kindReset, "u1", behemoth.M{"redirect": "/reset"})
	require.NoError(t, err)
	assert.NotEmpty(t, tok.ID)

	verified, err := tm.Verify(ctx, kindReset, raw)
	require.NoError(t, err)
	assert.Equal(t, "/reset", verified.MetadataJSON["redirect"], "metadata round-trips through the database")
	assert.Equal(t, "u1", verified.Subject)

	consumed, err := tm.Consume(ctx, kindReset, raw)
	require.NoError(t, err)
	require.NotNil(t, consumed.ConsumedAt)

	_, err = tm.Consume(ctx, kindReset, raw)
	assert.True(t, errs.IsCode(err, "token_already_consumed"), "%v", err)
	_, err = tm.Verify(ctx, kindReset, raw)
	assert.Error(t, err, "a consumed token no longer verifies")
}

func TestTokenConsumeConcurrently(t *testing.T) {
	ctx := context.Background()
	tm, _ := newTokenManager(t)
	_, raw, err := tm.Issue(ctx, kindReset, "u1", nil)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tm.Consume(ctx, kindReset, raw); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins, "a single-use token is consumed exactly once")
}

func TestTokenRevoke(t *testing.T) {
	ctx := context.Background()
	tm, _ := newTokenManager(t)

	tok, raw, err := tm.Issue(ctx, kindAPI, "u1", nil)
	require.NoError(t, err)
	require.NoError(t, tm.Revoke(ctx, tok.ID))
	require.NoError(t, tm.Revoke(ctx, tok.ID), "revoking twice is a no-op")
	_, err = tm.Verify(ctx, kindAPI, raw)
	assert.True(t, errs.IsCode(err, "token_revoked"), "%v", err)

	_, a, err := tm.Issue(ctx, kindAPI, "u2", nil)
	require.NoError(t, err)
	_, b, err := tm.Issue(ctx, kindAPI, "u2", nil)
	require.NoError(t, err)
	_, other, err := tm.Issue(ctx, kindAPI, "u3", nil)
	require.NoError(t, err)
	require.NoError(t, tm.RevokeAllForSubject(ctx, kindAPI, "u2"))
	for _, r := range []string{a, b} {
		_, err := tm.Verify(ctx, kindAPI, r)
		assert.Error(t, err, "u2's tokens are revoked")
	}
	_, err = tm.Verify(ctx, kindAPI, other)
	assert.NoError(t, err, "another subject's token is untouched")
}

// The token manager takes its time from the store's clock, in UTC like
// every time the store writes: an expiry is stamped from it, and a token is
// checked against it.
func TestTokenExpiryFollowsTheStoreClock(t *testing.T) {
	ctx := context.Background()
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"}))
	now := t0
	st := store.New(sessionsTokensDB(t), store.WithClock(func() time.Time { return now }))
	tm := auth.NewDefaultTokenManager(st, nil, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth)

	tok, raw, err := tm.Issue(ctx, kindReset, "u1", nil)
	require.NoError(t, err)
	assert.True(t, tok.ExpiresAt.Equal(t0.Add(time.Hour)), "expires_at %v", tok.ExpiresAt)
	assert.Equal(t, time.UTC, tok.ExpiresAt.Location())
	_, err = tm.Verify(ctx, kindReset, raw)
	require.NoError(t, err)

	now = t0.Add(2 * time.Hour)
	_, err = tm.Verify(ctx, kindReset, raw)
	assert.True(t, errs.IsCode(err, errs.CodeTokenExpired), "%v", err)
	_, err = tm.Consume(ctx, kindReset, raw)
	assert.True(t, errs.IsCode(err, errs.CodeTokenExpired), "%v", err)
}

// ListForSubject returns what a subject can still use of one kind, oldest
// first: a consumed, a revoked and an expired token are left out, and so are
// another subject's and another kind's.
func TestTokenListForSubject(t *testing.T) {
	ctx := context.Background()
	catalog := auth.NewDefaultTokenCatalog()
	const kindShort models.TokenKind = "short_lived"
	for _, def := range []core.TokenKindDef{
		{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"},
		{Kind: kindShort, SingleUse: true, DefaultTTL: time.Millisecond, Backend: core.TokenBackendDB, Owner: "core"},
		{Kind: "in_kv", DefaultTTL: time.Hour, Backend: core.TokenBackendKV, Owner: "core"},
	} {
		require.NoError(t, catalog.Declare(def))
	}
	tm := auth.NewDefaultTokenManager(store.New(sessionsTokensDB(t)), nil, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth)

	issue := func(kind models.TokenKind, subject string) (*models.Token, string) {
		tok, raw, err := tm.Issue(ctx, kind, subject, behemoth.M{"n": subject})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond) // distinct creation times
		return tok, raw
	}
	first, _ := issue(kindReset, "u1")
	_, usedRaw := issue(kindReset, "u1")
	revoked, _ := issue(kindReset, "u1")
	last, _ := issue(kindReset, "u1")
	issue(kindReset, "u2")
	issue(kindShort, "u1") // expired by the time it is listed
	_, err := tm.Consume(ctx, kindReset, usedRaw)
	require.NoError(t, err)
	require.NoError(t, tm.Revoke(ctx, revoked.ID))

	listed, err := tm.ListForSubject(ctx, kindReset, "u1")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, []string{first.ID, last.ID}, []string{listed[0].ID, listed[1].ID}, "oldest first")
	assert.Equal(t, behemoth.M{"n": "u1"}, listed[0].MetadataJSON, "the record, with its metadata")

	expired, err := tm.ListForSubject(ctx, kindShort, "u1")
	require.NoError(t, err)
	assert.Empty(t, expired)
	none, err := tm.ListForSubject(ctx, kindReset, "nobody")
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = tm.ListForSubject(ctx, "in_kv", "u1")
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a kind in the key-value storage can't be listed: %v", err)
	_, err = tm.ListForSubject(ctx, "undeclared", "u1")
	assert.Error(t, err)
}

// A multi-use kind can live in the key-value storage: an entry per token,
// under its lookup hash, that expires with the token. Listing and revoking by
// subject need the tokens table.
func TestTokenInTheKeyValueStorage(t *testing.T) {
	ctx := context.Background()
	kv := &memoryKV{}
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: "invite", DefaultTTL: time.Hour, Backend: core.TokenBackendKV, Owner: "core"}))
	cfg := core.TokenConfig{TTLOverrides: map[models.TokenKind]time.Duration{"invite": 2 * time.Hour}}
	tm := auth.NewDefaultTokenManager(store.New(sessionsTokensDB(t)), kv, catalog, testCrypto(t), &passDispatcher{}, cfg, managersAuth)

	tok, raw, err := tm.Issue(ctx, "invite", "u1", behemoth.M{"team": "t1"})
	require.NoError(t, err)
	assert.NotEmpty(t, tok.ID)
	key := "token:invite:" + tok.LookupHash
	assert.Equal(t, 2*time.Hour, kv.ttls[key], "the entry lives as long as the token, overridden in the config")

	for range 2 {
		verified, err := tm.Verify(ctx, "invite", raw)
		require.NoError(t, err, "a multi-use token verifies every time")
		assert.Equal(t, tok.ID, verified.ID)
		assert.Equal(t, behemoth.M{"team": "t1"}, verified.MetadataJSON)
	}
	_, err = tm.Consume(ctx, "invite", raw)
	assert.True(t, errs.IsCode(err, errs.CodeTokenInvalidUsage), "%v", err)
	err = tm.RevokeAllForSubject(ctx, "invite", "u1")
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)

	// A missing entry, another token's entry under this token's key, and an
	// entry that does not decode all read as no token.
	_, err = tm.Verify(ctx, "invite", "not-a-token")
	assert.True(t, errs.IsCode(err, errs.CodeTokenNotFound), "%v", err)
	other, _, err := tm.Issue(ctx, "invite", "u2", nil)
	require.NoError(t, err)
	forged, err := kv.Get(ctx, "token:invite:"+other.LookupHash)
	require.NoError(t, err)
	require.NoError(t, kv.Set(ctx, key, forged, 3600))
	_, err = tm.Verify(ctx, "invite", raw)
	assert.True(t, errs.IsCode(err, errs.CodeTokenNotFound), "the lookup hash matches and the keyed hash doesn't: %v", err)
	require.NoError(t, kv.Set(ctx, key, "{not json", 3600))
	_, err = tm.Verify(ctx, "invite", raw)
	assert.True(t, errs.IsCode(err, errs.CodeTokenNotFound), "%v", err)
}

// A kind in the key-value storage is refused a token when there is no
// storage, or when its tokens would never expire there.
func TestTokenInTheKeyValueStorageNeedsAStorageAndAnExpiry(t *testing.T) {
	ctx := context.Background()
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: "invite", DefaultTTL: time.Hour, Backend: core.TokenBackendKV, Owner: "core"}))
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: "forever", Backend: core.TokenBackendKV, Owner: "core"}))
	st := store.New(sessionsTokensDB(t))

	withoutKV := auth.NewDefaultTokenManager(st, nil, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth)
	_, _, err := withoutKV.Issue(ctx, "invite", "u1", nil)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)

	kv := &memoryKV{}
	tm := auth.NewDefaultTokenManager(st, kv, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth)
	_, _, err = tm.Issue(ctx, "forever", "u1", nil)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	assert.Empty(t, kv.entries)
}

// A single-use kind needs the database, which consumes a token exactly once:
// the key-value storage has no operation that reads and deletes an entry in
// one step. DefaultTokenCatalog refuses such a kind, and Consume refuses one
// from any other catalog, every time, so the token is never reported
// consumed while it stays usable.
func TestTokenConsumeNeedsTheDatabase(t *testing.T) {
	ctx := context.Background()
	def := core.TokenKindDef{Kind: "in_kv", SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendKV, Owner: "core"}
	tm := auth.NewDefaultTokenManager(store.New(sessionsTokensDB(t)), &memoryKV{}, lenientCatalog{def.Kind: def},
		testCrypto(t), &passDispatcher{}, core.TokenConfig{}, managersAuth)

	_, raw, err := tm.Issue(ctx, def.Kind, "u1", nil)
	require.NoError(t, err)
	for range 2 {
		_, err := tm.Consume(ctx, def.Kind, raw)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	}
}

// lenientCatalog is a TokenCatalog that takes any kind.
type lenientCatalog map[models.TokenKind]core.TokenKindDef

func (c lenientCatalog) Declare(def core.TokenKindDef) error { c[def.Kind] = def; return nil }

func (c lenientCatalog) Lookup(kind models.TokenKind) (core.TokenKindDef, bool) {
	def, ok := c[kind]
	return def, ok
}

// The token manager names an undeclared kind, and passes on what fails under
// it: a hook handler that refuses, and a database it can't reach.
func TestTokenManagerReportsFailures(t *testing.T) {
	ctx := context.Background()
	refused := errors.New("refused")

	t.Run("undeclared kind", func(t *testing.T) {
		tm, _ := newTokenManager(t)
		_, _, err := tm.Issue(ctx, "undeclared", "u1", nil)
		assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "Issue: %v", err)
		_, err = tm.Verify(ctx, "undeclared", "raw")
		assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "Verify: %v", err)
		_, err = tm.Consume(ctx, "undeclared", "raw")
		assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "Consume: %v", err)
		err = tm.RevokeAllForSubject(ctx, "undeclared", "u1")
		assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "RevokeAllForSubject: %v", err)
	})

	t.Run("hook handlers", func(t *testing.T) {
		for _, point := range []core.HookPoint{hooks.HookTokenBeforeIssue, hooks.HookTokenAfterIssue} {
			tm := tokenManagerWith(t, store.New(sessionsTokensDB(t)), &failingDispatcher{point: point, err: refused})
			_, _, err := tm.Issue(ctx, kindReset, "u1", nil)
			assert.ErrorIs(t, err, refused, point)
		}

		tm := tokenManagerWith(t, store.New(sessionsTokensDB(t)), &failingDispatcher{point: hooks.HookTokenConsumed, err: refused})
		_, raw, err := tm.Issue(ctx, kindReset, "u1", nil)
		require.NoError(t, err)
		_, err = tm.Consume(ctx, kindReset, raw)
		assert.ErrorIs(t, err, refused)

		tm = tokenManagerWith(t, store.New(sessionsTokensDB(t)), &failingDispatcher{point: hooks.HookTokenFailed, err: refused})
		_, err = tm.Consume(ctx, kindReset, "not-a-token")
		assert.ErrorIs(t, err, refused, "the failed point's error takes the place of the token's")
	})

	t.Run("unreachable database", func(t *testing.T) {
		tm := tokenManagerWith(t, unreachableStore(t), &passDispatcher{})
		_, _, err := tm.Issue(ctx, kindReset, "u1", nil)
		assert.Error(t, err, "Issue")
		assert.Error(t, tm.Revoke(ctx, "t1"), "Revoke")
		assert.Error(t, tm.RevokeAllForSubject(ctx, kindReset, "u1"), "RevokeAllForSubject")
		_, err = tm.ListForSubject(ctx, kindReset, "u1")
		assert.Error(t, err, "ListForSubject")
	})
}

// tokenManagerWith builds a token manager like newTokenManager does, over st
// and with d.
func tokenManagerWith(t *testing.T, st *store.Store, d core.Dispatcher) core.TokenManager {
	t.Helper()
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"}))
	return auth.NewDefaultTokenManager(st, nil, catalog, testCrypto(t), d, core.TokenConfig{}, managersAuth)
}

// failingDispatcher is a passDispatcher that returns err at one hook point.
type failingDispatcher struct {
	passDispatcher
	point core.HookPoint
	err   error
}

func (d *failingDispatcher) RunBefore(h *core.HookContext, point core.HookPoint, p behemoth.M) (behemoth.M, error) {
	if point == d.point {
		return nil, d.err
	}
	return d.passDispatcher.RunBefore(h, point, p)
}

func (d *failingDispatcher) RunAfter(h *core.HookContext, point core.HookPoint, v any) error {
	if point == d.point {
		return d.err
	}
	return d.passDispatcher.RunAfter(h, point, v)
}

func (d *failingDispatcher) RunAfterTx(h *core.HookContext, point core.HookPoint, v any) error {
	if point == d.point {
		return d.err
	}
	return d.passDispatcher.RunAfterTx(h, point, v)
}

func (d *failingDispatcher) Fail(h *core.HookContext, point core.HookPoint, r core.FailureReason) error {
	if point == d.point {
		return d.err
	}
	return d.passDispatcher.Fail(h, point, r)
}

// unreachableStore is a store over a closed database: every call to it
// fails.
func unreachableStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	_, err = db.Exec(sessionsTokensSQLite)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return store.New(sqliteAdapter.NewSQLiteAdapter(db, nil))
}

// cryptoWith builds crypto services over secrets, by version, that hash
// under current.
func cryptoWith(t *testing.T, secrets map[int]string, current int) crypto.Services {
	t.Helper()
	c, err := crypto.New(context.Background(), crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: secrets, Current: current},
	}, nil)
	require.NoError(t, err)
	return c
}

// A session hashed under a secret that has since been replaced is hashed
// again under the current one when it is next used, so that the old secret
// can be retired. Before that, a manager without the old secret refuses the
// session. Another secret under the same version matches nothing.
func TestSessionMovesToTheCurrentSecretOnUse(t *testing.T) {
	ctx := context.Background()
	old, current := strings.Repeat("cd", 32), strings.Repeat("ef", 32)
	st := store.New(sessionsTokensDB(t))
	manager := func(secrets map[int]string, version int) core.SessionManager {
		return auth.NewSessionManager(st, nil, cryptoWith(t, secrets, version), core.SessionConfig{ExpiresIn: time.Hour},
			&passDispatcher{}, nil, managersAuth)
	}

	sess, raw, err := manager(map[int]string{1: old}, 1).Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	retired := manager(map[int]string{2: current}, 2)
	_, err = retired.Get(ctx, raw)
	assert.Error(t, err, "the secret the session was hashed under is gone")

	got, err := manager(map[int]string{1: old, 2: current}, 2).Get(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, 2, got.KeyVersion)
	stored, err := st.FindSessionByID(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, stored.KeyVersion, "the new hash is written")
	_, err = retired.Get(ctx, raw)
	assert.NoError(t, err, "the old secret can go")

	_, err = manager(map[int]string{2: strings.Repeat("01", 32)}, 2).Get(ctx, raw)
	assert.True(t, errs.IsNotFound(err), "another secret under version 2: %v", err)
}

// Only a pending session that has not expired can be promoted: a revoked one
// never comes back, and an abandoned one starts again from the sign-in.
func TestSessionPromoteNeedsALivePendingSession(t *testing.T) {
	ctx := context.Background()
	now := t0
	st := store.New(sessionsTokensDB(t), store.WithClock(func() time.Time { return now }))
	sm := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute},
		&passDispatcher{}, nil, managersAuth)
	pending := core.SessionMeta{State: models.SessionPending}

	revoked, _, err := sm.Create(ctx, "u1", pending)
	require.NoError(t, err)
	require.NoError(t, sm.Revoke(ctx, revoked.ID, "test"))
	_, _, err = sm.Promote(ctx, revoked.ID, "totp")
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)

	abandoned, _, err := sm.Create(ctx, "u1", pending)
	require.NoError(t, err)
	now = now.Add(10 * time.Minute)
	_, _, err = sm.Promote(ctx, abandoned.ID, "totp")
	assert.True(t, errs.IsCode(err, errs.CodeSessionExpired), "%v", err)
}

// Of two revokes of one session, the one whose write comes second finds the
// session revoked. It returns without an error, and the handlers of
// auth.session.afterRevoke hear of the revoke once, with the first reason.
func TestSessionRevokeLosesToAConcurrentRevoke(t *testing.T) {
	ctx := context.Background()
	d := &interceptDispatcher{}
	st := store.New(sessionsTokensDB(t))
	sm := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{ExpiresIn: time.Hour}, d, nil, managersAuth)
	sess, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)

	d.once = map[core.HookPoint]func(){hooks.HookSessionBeforeRevoke: func() {
		require.NoError(t, sm.Revoke(ctx, sess.ID, "first"))
	}}
	require.NoError(t, sm.Revoke(ctx, sess.ID, "second"))
	assert.Equal(t, 1, d.fired(hooks.HookSessionAfterRevoke))
	stored, err := st.FindSessionByID(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "first", stored.RevokedReason)
}

// The session manager refuses a state a session can't start in, and passes
// on what fails under it: a hook handler that refuses, and a database it
// can't reach.
func TestSessionManagerReportsFailures(t *testing.T) {
	ctx := context.Background()
	refused := errors.New("refused")
	active := core.SessionMeta{State: models.SessionActive}
	cfg := core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute}
	newManager := func(st *store.Store, d core.Dispatcher, cfg core.SessionConfig) core.SessionManager {
		return auth.NewSessionManager(st, nil, testCrypto(t), cfg, d, nil, managersAuth)
	}

	t.Run("a state to start in", func(t *testing.T) {
		sm := newManager(store.New(sessionsTokensDB(t)), &passDispatcher{}, cfg)
		_, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionRevoked})
		assert.True(t, errs.IsCategory(err, errs.CategoryValidation), "%v", err)
	})

	t.Run("hook handlers", func(t *testing.T) {
		sm := newManager(store.New(sessionsTokensDB(t)), &failingDispatcher{point: hooks.HookSessionAfterCreate, err: refused}, cfg)
		_, _, err := sm.Create(ctx, "u1", active)
		assert.ErrorIs(t, err, refused, "afterCreate")

		st := store.New(sessionsTokensDB(t))
		sm = newManager(st, &failingDispatcher{point: hooks.HookSessionBeforeRevoke, err: refused}, cfg)
		sess, _, err := sm.Create(ctx, "u1", active)
		require.NoError(t, err)
		assert.ErrorIs(t, sm.Revoke(ctx, sess.ID, "test"), refused, "Revoke")
		assert.ErrorIs(t, sm.RevokeAllForUser(ctx, "u1", "test", ""), refused, "RevokeAllForUser")
		stored, err := st.FindSessionByID(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, models.SessionActive, stored.State, "a refused revoke leaves the session")

		sm = newManager(st, &failingDispatcher{point: hooks.HookSessionAfterRevoke, err: refused}, cfg)
		assert.ErrorIs(t, sm.Discard(ctx, []*models.Session{stored}, "user_deleted"), refused, "Discard")
	})

	t.Run("unreachable database", func(t *testing.T) {
		st := unreachableStore(t)
		sm := newManager(st, &passDispatcher{}, cfg)
		_, _, err := sm.Create(ctx, "u1", active)
		assert.Error(t, err, "Create")
		limited := cfg
		limited.MaxConcurrent = 1
		_, _, err = newManager(st, &passDispatcher{}, limited).Create(ctx, "u1", active)
		assert.Error(t, err, "Create, counting the user's sessions")
		_, _, err = sm.Promote(ctx, "s1", "totp")
		assert.Error(t, err, "Promote")
		assert.Error(t, sm.Revoke(ctx, "s1", "test"), "Revoke")
		assert.Error(t, sm.RevokeAllForUser(ctx, "u1", "test", ""), "RevokeAllForUser")
		_, err = sm.ListForUser(ctx, "u1")
		assert.Error(t, err, "ListForUser")
		rolling := cfg
		rolling.UpdateAge = time.Hour
		_, err = newManager(st, &passDispatcher{}, rolling).Touch(ctx, &models.Session{ID: "s1", ExpiresAt: time.Now()})
		assert.Error(t, err, "Touch")
	})
}

// A session cache that fails costs only speed: each operation still
// succeeds against the database, and each failed write is logged.
func TestSessionCacheFailuresAreLogged(t *testing.T) {
	ctx := context.Background()
	for name, kv := range map[string]behemoth.KeyValueStorage{
		"versioned": brokenKV{},
		"plain":     plainKV{brokenKV{}},
	} {
		t.Run(name, func(t *testing.T) {
			tel, rec := telemetrytest.New()
			sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), kv, testCrypto(t), core.SessionConfig{ExpiresIn: time.Hour},
				&passDispatcher{}, tel, managersAuth)
			sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
			require.NoError(t, err)
			_, err = sm.Get(ctx, raw)
			require.NoError(t, err)
			require.NoError(t, sm.Revoke(ctx, sess.ID, "test"))
			_, err = sm.Get(ctx, raw)
			assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "the database decides: %v", err)
			assert.NotEmpty(t, rec.Logger.At(slog.LevelWarn))
		})
	}
}

// brokenKV is a session cache whose every call fails.
type brokenKV struct{}

var errUnreachable = errors.New("unreachable")

func (brokenKV) Get(context.Context, string) (string, error)    { return "", errUnreachable }
func (brokenKV) Set(context.Context, string, string, int) error { return errUnreachable }
func (brokenKV) Delete(context.Context, string) error           { return errUnreachable }
func (brokenKV) SetIfNewer(context.Context, string, string, int64, time.Duration) (bool, error) {
	return false, errUnreachable
}
func (brokenKV) GetVersioned(context.Context, string) (string, int64, error) {
	return "", 0, errUnreachable
}

// A session is fresh for FreshAge after it was created or promoted.
// RequireFreshSession lets a fresh one through and refuses one that is
// still valid but older.
func TestSessionFreshness(t *testing.T) {
	ctx := context.Background()
	sm, _ := newSessionManager(t, core.SessionConfig{FreshAge: 150 * time.Millisecond})
	active, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	pending, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	assert.True(t, sm.IsFresh(active), "just created")
	assert.False(t, sm.IsFresh(pending), "a pending session has not finished signing in")
	assert.False(t, sm.IsFresh(nil))

	reached := 0
	handler := authenticated(sm, httpapi.DeliveryHeader, httpapi.RequireFreshSession(sm)(func(rctx *httpapi.RequestContext) error {
		reached++
		_, ok := identity.CallerFrom(rctx.Ctx).Session()
		assert.True(t, ok, "it does what RequireSession does first")
		return nil
	}))
	request := func(header string) (*httpapi.RequestContext, error) {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		return rctx, handler(rctx)
	}
	_, err = request("Bearer " + raw)
	require.NoError(t, err)
	require.Equal(t, 1, reached)

	time.Sleep(250 * time.Millisecond)
	stale, err := sm.Validate(ctx, raw)
	require.NoError(t, err, "the session is still valid")
	assert.False(t, sm.IsFresh(stale))
	_, err = request("Bearer " + raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionNotFresh), "%v", err)
	assert.True(t, errs.IsCategory(err, errs.CategorySession), "a session error, answered with 401: %v", err)
	_, err = request("")
	assert.True(t, errs.IsCode(err, errs.CodeSessionMissing), "without a session it answers as RequireSession does: %v", err)
	assert.Equal(t, 1, reached, "neither request reached the handler")

	promoted, _, err := sm.Promote(ctx, pending.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.True(t, sm.IsFresh(promoted), "passing the second factor is a credential check")

	// Left at zero, FreshAge is 15 minutes.
	byDefault, _ := newSessionManager(t, core.SessionConfig{})
	session, _, err := byDefault.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	assert.True(t, byDefault.IsFresh(session))
	session.FreshAt = time.Now().Add(-core.DefaultFreshAge - time.Second)
	assert.False(t, byDefault.IsFresh(session))
}

// A session manager built with a zero SessionConfig works: sessions last
// the documented default.
func TestSessionManagerWithAZeroConfig(t *testing.T) {
	ctx := context.Background()
	sm := auth.NewSessionManager(store.New(sessionsTokensDB(t)), nil, testCrypto(t), core.SessionConfig{}, &passDispatcher{}, nil, managersAuth)

	active, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(core.DefaultSessionExpiresIn), active.ExpiresAt, time.Minute)
	pending, _, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(core.DefaultSessionPendingExpiresIn), pending.ExpiresAt, time.Minute)
	_, err = sm.Validate(ctx, raw)
	require.NoError(t, err, "the session did not expire the moment it was created")
}

// RequireSession applies the rolling expiration, and the cookie has to
// follow it: a browser drops a cookie at the date it was set with, however
// long the session lasts on the server.
func TestRequireSessionExtendsTheCookieOfARollingSession(t *testing.T) {
	ctx := context.Background()
	const ownCacheControl = "private, max-age=60"
	// request sends one request, with its token carried the way delivery
	// says, through RequireSession to a handler that sets a Cache-Control
	// of its own. It returns the response and the session the handler saw.
	request := func(sm core.SessionManager, delivery httpapi.TokenDelivery, cookie, bearer string) (*httpapi.RequestContext, identity.SessionRef) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "session_token", Value: cookie})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		var seen identity.SessionRef
		reached := false
		handler := requireSession(sm, delivery, func(rctx *httpapi.RequestContext) error {
			seen, reached = identity.CallerFrom(rctx.Ctx).Session()
			rctx.Response.SetHeader("Cache-Control", ownCacheControl)
			return rctx.Response.JSON(http.StatusOK, behemoth.M{})
		})
		rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
		require.NoError(t, handler(rctx))
		require.True(t, reached, "the request reached the handler")
		return rctx, seen
	}
	cookies := func(rctx *httpapi.RequestContext) []*http.Cookie {
		return (&http.Response{Header: rctx.Response.Headers}).Cookies()
	}
	alwaysDue := 2 * time.Hour // above the hour a session of newSessionManager lasts

	// A session that is due: the cookie comes back with the new date.
	rolling, _ := newSessionManager(t, core.SessionConfig{UpdateAge: alwaysDue})
	created, raw, err := rolling.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	rctx, seen := request(rolling, "", raw, "")
	assert.True(t, seen.ExpiresAt.After(created.ExpiresAt), "the handler sees the session with its new expiry")
	set := cookies(rctx)
	require.Len(t, set, 1)
	assert.Equal(t, "session_token", set[0].Name)
	assert.Equal(t, raw, set[0].Value, "the same token")
	assert.True(t, seen.ExpiresAt.UTC().Truncate(time.Second).Equal(set[0].Expires), "the cookie expires with the extended session: %s", set[0].Expires)
	assert.Equal(t, "/", set[0].Path)
	assert.True(t, set[0].HttpOnly && set[0].Secure, "the attributes of the cookie it replaces")
	assert.Equal(t, "no-store", rctx.Response.Headers.Get("Cache-Control"), "the response carries the token, so it is not cached, whatever the handler set")

	// A session that is not due: nothing is written, and the handler's
	// Cache-Control stands.
	fixed, _ := newSessionManager(t, core.SessionConfig{})
	created, raw, err = fixed.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	rctx, seen = request(fixed, "", raw, "")
	assert.True(t, seen.ExpiresAt.Equal(created.ExpiresAt))
	assert.Empty(t, cookies(rctx))
	assert.Equal(t, ownCacheControl, rctx.Response.Headers.Get("Cache-Control"))

	// The header delivery: the session is extended, and its client keeps
	// the token without a date, so the response hands out nothing.
	created, raw, err = rolling.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	rctx, seen = request(rolling, httpapi.DeliveryHeader, "", raw)
	assert.True(t, seen.ExpiresAt.After(created.ExpiresAt))
	assert.Empty(t, cookies(rctx))
	assert.Empty(t, rctx.Response.Headers.Get(httpapi.SessionTokenHeader))
	assert.Equal(t, ownCacheControl, rctx.Response.Headers.Get("Cache-Control"))

	// Both deliveries: the cookie is extended for the client that sent the
	// token in it. A bearer token is read first, and the cookie next to it
	// may be another session's, which must not be overwritten.
	_, asBearer, err := rolling.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	_, inCookie, err := rolling.Create(ctx, "u2", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	rctx, seen = request(rolling, httpapi.DeliveryBoth, inCookie, asBearer)
	assert.Equal(t, "u1", seen.UserID, "the bearer token's session")
	assert.Empty(t, cookies(rctx), "the cookie is another session's")
	assert.Equal(t, ownCacheControl, rctx.Response.Headers.Get("Cache-Control"))
	rctx, seen = request(rolling, httpapi.DeliveryBoth, inCookie, "")
	assert.Equal(t, "u2", seen.UserID)
	set = cookies(rctx)
	require.Len(t, set, 1)
	assert.Equal(t, inCookie, set[0].Value)
}

// Under a real Boot the session manager dispatches through the default
// dispatcher, which rejects undeclared points. Create and Revoke each fire a
// before and an after point, and core declares all four.
func TestSessionPointsAreDeclaredUnderBoot(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)

	var fired []core.HookPoint
	var auths []*core.AuthContext
	before := func(hctx *core.HookContext, p behemoth.M) (behemoth.M, error) {
		fired, auths = append(fired, hctx.Point), append(auths, hctx.Auth)
		return p, nil
	}
	after := func(hctx *core.HookContext, _ any) error {
		fired, auths = append(fired, hctx.Point), append(auths, hctx.Auth)
		return nil
	}
	ac, err := engine.Boot(ctx, app, sessionsTokensDB(t), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("cd", 32)}, Current: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute},
		Hooks: func(reg core.HookRegistry) error {
			if err := reg.OnBefore(hooks.HookSessionBeforeCreate, before, nil); err != nil {
				return err
			}
			if err := reg.OnAfter(hooks.HookSessionAfterCreate, after, nil); err != nil {
				return err
			}
			if err := reg.OnBefore(hooks.HookSessionBeforeRevoke, before, nil); err != nil {
				return err
			}
			return reg.OnAfter(hooks.HookSessionAfterRevoke, after, nil)
		},
	})
	require.NoError(t, err)

	sess, _, err := ac.SessionManager.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NoError(t, ac.SessionManager.Revoke(ctx, sess.ID, "user_logout"))
	assert.Equal(t, []core.HookPoint{
		hooks.HookSessionBeforeCreate, hooks.HookSessionAfterCreate,
		hooks.HookSessionBeforeRevoke, hooks.HookSessionAfterRevoke,
	}, fired)
	for i, got := range auths {
		assert.Same(t, ac, got, "%s: handlers get the booted AuthContext", fired[i])
	}
}

// authenticated wraps next in the stage the router puts in front of every
// route, over sm's sessions, with the token carried the way delivery says
// ("" is the cookie).
func authenticated(sm core.SessionManager, delivery httpapi.TokenDelivery, next httpapi.HandlerFunc) httpapi.HandlerFunc {
	carrier := httpapi.NewTokenCarrier(httpapi.TokenCarrierConfig{Delivery: delivery})
	return httpapi.AuthenticateRequests(auth.NewAuthenticator(sm, nil), carrier, nil)(next)
}

// requireSession is what a route behind RequireSession runs: the
// authentication stage, then RequireSession, then next.
func requireSession(sm core.SessionManager, delivery httpapi.TokenDelivery, next httpapi.HandlerFunc) httpapi.HandlerFunc {
	return authenticated(sm, delivery, httpapi.RequireSession()(next))
}

// The authentication stage replaces any caller the context carried, so a
// middleware in front of Behemoth can't plant an identity: a request without
// a token is anonymous, whatever its context said.
func TestAuthenticationReplacesAPlantedCaller(t *testing.T) {
	sm, _ := newSessionManager(t, core.SessionConfig{})
	reached := false
	handler := requireSession(sm, httpapi.DeliveryHeader, func(*httpapi.RequestContext) error {
		reached = true
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	planted := identity.WithCaller(req.Context(), identity.System("planted").For("victim"))
	rctx := &httpapi.RequestContext{Ctx: planted, Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}

	err := handler(rctx)
	assert.True(t, errs.IsCode(err, errs.CodeSessionMissing), "got %v", err)
	assert.False(t, reached)
	assert.True(t, identity.CallerFrom(rctx.Ctx).IsAnonymous())
}
