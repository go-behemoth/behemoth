package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/httpapi"
	_ "github.com/mattn/go-sqlite3"
)

// usersDB opens a SQLite database with the users table models.User maps to.
// (Until core declares its tables, the test creates it by hand.)
func usersDB(t *testing.T) behemoth.Database {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "store.db")+"?_foreign_keys=on")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, firstname TEXT, lastname TEXT,
		email_verified BOOLEAN NOT NULL DEFAULT 0, image_url TEXT,
		created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(testutils.AuditLogSQLiteSchema)
	require.NoError(t, err)
	return sqliteAdapter.NewSQLiteAdapter(db, nil)
}

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func fixedClock(times ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		t := times[min(i, len(times)-1)]
		i++
		return t
	}
}

func sequentialIDs() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("id-%d", n) }
}

type recordingHooks struct {
	tables    []string
	rewrite   behemoth.M
	abort     error
	createdAs []behemoth.Model

	updateIDs     []any
	updateSeen    []behemoth.M
	updateRewrite behemoth.M
	updateAbort   error
	updatedAs     []behemoth.Model

	// deletes records the BeforeDelete and AfterDelete calls, in order.
	deletes     []string
	deleteAbort error
	afterDelete func(ctx context.Context, tx *store.Store) error

	// silent makes Fires report false, as for a table without hook points.
	silent bool
	// afterCreate and afterUpdate, if set, run as the after hooks with the
	// store bound to the write's transaction.
	afterCreate func(ctx context.Context, tx *store.Store, created behemoth.Model) error
	afterUpdate func(ctx context.Context, tx *store.Store, updated behemoth.Model) error
	inTx        []*store.Store

	// committed records the after-commit notifications, in order.
	committed []string

	// begun counts Begin calls; writes records, per Before and After call,
	// the number of the write its context belongs to.
	begun  int
	writes []any
}

// reportCommit queues an after-commit notification on the write's store, the
// way an implementation of store.Hooks does (the store has no call for it).
func (h *recordingHooks) reportCommit(ctx context.Context, tx *store.Store, what, table string, m behemoth.Model) {
	tx.AfterCommit(ctx, func(context.Context) {
		h.committed = append(h.committed, fmt.Sprint(what, " ", table, " ", m.PrimaryKeyField()))
	})
}

func (h *recordingHooks) Fires(string) bool { return !h.silent }

type writeKey struct{}

// Begin numbers the writes, so a test can tell that a write's Before and
// After calls ran with the context Begin returned.
func (h *recordingHooks) Begin(ctx context.Context, table string) context.Context {
	h.begun++
	return context.WithValue(ctx, writeKey{}, h.begun)
}

func (h *recordingHooks) BeforeUpdate(ctx context.Context, _ *store.Store, table string, id any, changes behemoth.M) (behemoth.M, error) {
	h.writes = append(h.writes, ctx.Value(writeKey{}))
	h.updateIDs = append(h.updateIDs, id)
	seen := behemoth.M{}
	for k, v := range changes {
		seen[k] = v
	}
	h.updateSeen = append(h.updateSeen, seen)
	if h.updateAbort != nil {
		return nil, h.updateAbort
	}
	for k, v := range h.updateRewrite {
		changes[k] = v
	}
	return changes, nil
}

func (h *recordingHooks) AfterUpdate(ctx context.Context, tx *store.Store, table string, updated behemoth.Model) error {
	h.updatedAs = append(h.updatedAs, updated)
	h.writes = append(h.writes, ctx.Value(writeKey{}))
	if h.afterUpdate != nil {
		if err := h.afterUpdate(ctx, tx, updated); err != nil {
			return err
		}
	}
	h.reportCommit(ctx, tx, "updated", table, updated)
	return nil
}

func (h *recordingHooks) BeforeDelete(_ context.Context, _ *store.Store, table string, row behemoth.Model) error {
	h.deletes = append(h.deletes, fmt.Sprint("before ", table, " ", row.PrimaryKeyField()))
	return h.deleteAbort
}

func (h *recordingHooks) AfterDelete(ctx context.Context, tx *store.Store, table string, deleted behemoth.Model, sessions []*models.Session) error {
	h.deletes = append(h.deletes, fmt.Sprint("after ", table, " ", deleted.PrimaryKeyField(), " sessions=", len(sessions)))
	if h.afterDelete != nil {
		if err := h.afterDelete(ctx, tx); err != nil {
			return err
		}
	}
	h.reportCommit(ctx, tx, "deleted", table, deleted)
	return nil
}

func (h *recordingHooks) BeforeCreate(ctx context.Context, tx *store.Store, table string, row behemoth.M) (behemoth.M, error) {
	h.writes = append(h.writes, ctx.Value(writeKey{}))
	h.tables = append(h.tables, table)
	h.inTx = append(h.inTx, tx)
	if h.abort != nil {
		return nil, h.abort
	}
	for k, v := range h.rewrite {
		row[k] = v
	}
	return row, nil
}

func (h *recordingHooks) AfterCreate(ctx context.Context, tx *store.Store, table string, created behemoth.Model) error {
	h.createdAs = append(h.createdAs, created)
	h.inTx = append(h.inTx, tx)
	h.writes = append(h.writes, ctx.Value(writeKey{}))
	if h.afterCreate != nil {
		if err := h.afterCreate(ctx, tx, created); err != nil {
			return err
		}
	}
	h.reportCommit(ctx, tx, "created", table, created)
	return nil
}

func TestCreateUserAssignsIDTimestampsAndNormalizedEmail(t *testing.T) {
	ctx := context.Background()
	s := store.New(usersDB(t), store.WithClock(fixedClock(t0)), store.WithIDs(sequentialIDs()))

	u := &models.User{Email: "  Ada@Example.COM ", Firstname: "ada"}
	require.NoError(t, s.CreateUser(ctx, u))
	assert.Equal(t, "id-1", u.ID)
	assert.Equal(t, "ada@example.com", u.Email)
	assert.True(t, u.CreatedAt.Equal(t0) && u.UpdatedAt.Equal(t0))

	found, err := s.FindUserByEmail(ctx, "ADA@example.com")
	require.NoError(t, err, "lookup normalizes the same way creation does")
	assert.Equal(t, "id-1", found.ID)
	assert.True(t, found.CreatedAt.Equal(t0), "created_at is a real timestamp, got %v", found.CreatedAt)

	byID, err := s.FindUserByID(ctx, "id-1")
	require.NoError(t, err)
	assert.Equal(t, "ada", byID.Firstname)
}

func TestCreateUserRunsHooks(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{rewrite: behemoth.M{models.UserFirstname: "from-hook"}}
	s := store.New(usersDB(t), store.WithHooks(h))

	u := &models.User{Email: "a@example.com", Firstname: "mine"}
	require.NoError(t, s.CreateUser(ctx, u))
	assert.Equal(t, []string{models.UserTable}, h.tables)
	assert.Equal(t, "from-hook", u.Firstname, "the caller's model reflects the hook's rewrite")

	stored, err := s.FindUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "from-hook", stored.Firstname, "the hook's rewrite is what was stored")
	require.Len(t, h.createdAs, 1)
	assert.Same(t, u, h.createdAs[0], "AfterCreate receives the stored model")
}

// An email a before-create hook sets is normalized like the caller's, so the
// user is stored in the form FindUserByEmail looks up.
func TestCreateUserNormalizesAHooksEmail(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{rewrite: behemoth.M{models.UserEmail: "  Grace@Example.COM "}}
	s := store.New(usersDB(t), store.WithHooks(h))

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))
	assert.Equal(t, "grace@example.com", u.Email, "the caller's model holds what was stored")
	found, err := s.FindUserByEmail(ctx, "grace@example.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, found.ID)
}

func TestCreateUserHookErrorAborts(t *testing.T) {
	ctx := context.Background()
	abort := errors.New("rejected by hook")
	h := &recordingHooks{abort: abort}
	s := store.New(usersDB(t), store.WithHooks(h))

	err := s.CreateUser(ctx, &models.User{Email: "a@example.com"})
	assert.ErrorIs(t, err, abort)
	_, err = s.FindUserByEmail(ctx, "a@example.com")
	assert.True(t, errs.IsNotFound(err), "nothing was written: %v", err)
	assert.Empty(t, h.createdAs, "AfterCreate never ran")
}

// An after-create hook runs in the create's transaction: its error fails the
// create and rolls the row back.
func TestCreateUserAfterHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	abort := errors.New("rejected after insert")
	h := &recordingHooks{}
	h.afterCreate = func(ctx context.Context, tx *store.Store, created behemoth.Model) error {
		_, err := tx.FindUserByID(ctx, created.PrimaryKeyField().(string))
		assert.NoError(t, err, "the hook's store sees the row it was fired for")
		return abort
	}
	s := store.New(usersDB(t), store.WithHooks(h))

	err := s.CreateUser(ctx, &models.User{Email: "a@example.com"})
	assert.ErrorIs(t, err, abort, "the hook's error is returned as-is")
	_, err = s.FindUserByEmail(ctx, "a@example.com")
	assert.True(t, errs.IsNotFound(err), "the insert was rolled back: %v", err)
}

// What a hook writes through the store it is given commits or rolls back
// with the write that fired it.
func TestAfterCreateHookWritesShareTheTransaction(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	var fail error
	h.afterCreate = func(ctx context.Context, tx *store.Store, created behemoth.Model) error {
		if created.(*models.User).Email == "shadow@example.com" {
			return fail
		}
		return tx.CreateUser(ctx, &models.User{Email: "shadow@example.com"})
	}
	s := store.New(usersDB(t), store.WithHooks(h))

	fail = errors.New("the hook's own write failed")
	err := s.CreateUser(ctx, &models.User{Email: "a@example.com"})
	assert.ErrorIs(t, err, fail)
	for _, email := range []string{"a@example.com", "shadow@example.com"} {
		_, err = s.FindUserByEmail(ctx, email)
		assert.True(t, errs.IsNotFound(err), "%s was rolled back: %v", email, err)
	}

	fail = nil
	require.NoError(t, s.CreateUser(ctx, &models.User{Email: "a@example.com"}))
	for _, email := range []string{"a@example.com", "shadow@example.com"} {
		_, err = s.FindUserByEmail(ctx, email)
		assert.NoError(t, err, "%s was committed", email)
	}
}

// In a transaction the caller opened, hooks join it: a later failure rolls
// back the row and what its hooks wrote, after the after hook has run.
func TestHooksJoinTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	h.afterCreate = func(ctx context.Context, tx *store.Store, created behemoth.Model) error {
		_, err := tx.UpdateUser(ctx, created.PrimaryKeyField().(string), behemoth.M{models.UserFirstname: "from-hook"})
		return err
	}
	s := store.New(usersDB(t), store.WithHooks(h))

	later := errors.New("a later write failed")
	var callers *store.Store
	err := s.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		callers = tx
		if err := tx.CreateUser(ctx, &models.User{Email: "a@example.com"}); err != nil {
			return err
		}
		return later
	})
	assert.ErrorIs(t, err, later)
	require.Len(t, h.createdAs, 1, "the after hook ran before the rollback")
	for _, tx := range h.inTx {
		assert.Same(t, callers, tx, "hooks get the caller's transaction, not a nested one")
	}
	_, err = s.FindUserByEmail(ctx, "a@example.com")
	assert.True(t, errs.IsNotFound(err), "the user was rolled back: %v", err)
}

// A hook writes tables the store has no operations for through tx.DB(), the
// adapter bound to the write's transaction. Here a raw users row stands in
// for a plugin's own table.
func TestHookWritesThroughTheAdapterShareTheTransaction(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	var fail error
	h.afterCreate = func(ctx context.Context, tx *store.Store, created behemoth.Model) error {
		raw := &models.User{ID: "raw-1", Email: "raw@example.com", CreatedAt: t0, UpdatedAt: t0}
		// Transaction on the bound adapter joins the open transaction.
		err := tx.DB().Transaction(ctx, func(ctx context.Context, db behemoth.Database) (any, error) {
			return nil, db.Create(ctx, raw)
		})
		if err != nil {
			return err
		}
		return fail
	}
	db := usersDB(t)
	s := store.New(db, store.WithHooks(h))
	assert.Same(t, db, s.DB(), "outside a transaction DB is the adapter the store was built with")
	rawExists := func() bool {
		_, err := s.FindUserByID(ctx, "raw-1")
		return err == nil
	}

	fail = errors.New("rejected after the raw write")
	assert.ErrorIs(t, s.CreateUser(ctx, &models.User{Email: "a@example.com"}), fail)
	assert.False(t, rawExists(), "the adapter write was rolled back with the user")

	fail = nil
	require.NoError(t, s.CreateUser(ctx, &models.User{Email: "a@example.com"}))
	assert.True(t, rawExists(), "the adapter write was committed with the user")
	assert.Len(t, h.createdAs, 2, "the raw insert fired no data hook")
}

// A table whose hooks don't fire writes directly, without a transaction.
func TestWritesWithoutHooksOpenNoTransaction(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{silent: true}
	s := store.New(usersDB(t), store.WithHooks(h))
	require.NoError(t, s.CreateUser(ctx, &models.User{Email: "a@example.com"}))
	for _, tx := range h.inTx {
		assert.Same(t, s, tx, "the write went through the store itself")
	}

	h = &recordingHooks{}
	s = store.New(usersDB(t), store.WithHooks(h))
	require.NoError(t, s.CreateUser(ctx, &models.User{Email: "a@example.com"}))
	require.NotEmpty(t, h.inTx)
	for _, tx := range h.inTx {
		assert.NotSame(t, s, tx, "a hooked write runs on a store bound to a transaction")
	}
}

func TestUpdateUserAfterHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	abort := errors.New("rejected after update")
	h := &recordingHooks{}
	s := store.New(usersDB(t), store.WithHooks(h))
	u := &models.User{Email: "a@example.com", Firstname: "Ada"}
	require.NoError(t, s.CreateUser(ctx, u))

	h.afterUpdate = func(context.Context, *store.Store, behemoth.Model) error { return abort }
	_, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Grace"})
	assert.ErrorIs(t, err, abort)
	require.Len(t, h.updatedAs, 1)
	assert.Equal(t, "Grace", h.updatedAs[0].(*models.User).Firstname, "the hook saw the updated row")
	got, err := s.FindUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "Ada", got.Firstname, "the update was rolled back")
}

func TestCreateHookAddingUnknownColumnIsRejected(t *testing.T) {
	ctx := context.Background()
	s := store.New(usersDB(t), store.WithHooks(&recordingHooks{rewrite: behemoth.M{"nickname": "x"}}))

	err := s.CreateUser(ctx, &models.User{Email: "a@example.com"})
	require.Error(t, err)
	assert.True(t, errs.IsValidationError(err), "%v", err)
	assert.Contains(t, errors.Unwrap(err).Error(), "nickname")
	_, err = s.FindUserByEmail(ctx, "a@example.com")
	assert.True(t, errs.IsNotFound(err), "nothing was written: %v", err)
}

func TestUpdateUserRunsHooks(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{updateRewrite: behemoth.M{models.UserEmail: " Hooked@Example.com"}}
	s := store.New(usersDB(t), store.WithHooks(h))
	u := &models.User{Email: "a@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))

	updates := behemoth.M{models.UserFirstname: "Ada"}
	updated, err := s.UpdateUser(ctx, u.ID, updates)
	require.NoError(t, err)

	require.Len(t, h.updateIDs, 1)
	assert.Equal(t, u.ID, h.updateIDs[0], "the hook is told which row")
	assert.Equal(t, behemoth.M{models.UserFirstname: "Ada"}, h.updateSeen[0], "the hook sees only the changes — no id, no updated_at")
	assert.Equal(t, "hooked@example.com", updated.Email, "the hook's rewrite is applied, and normalized")
	assert.Equal(t, "Ada", updated.Firstname)
	assert.Equal(t, behemoth.M{models.UserFirstname: "Ada"}, updates, "the caller's map is not modified")
	require.Len(t, h.updatedAs, 1)
	assert.Equal(t, "hooked@example.com", h.updatedAs[0].(*models.User).Email, "AfterUpdate receives the stored row")
}

func TestUpdateUserHookErrorsAndRewritesAreChecked(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	s := store.New(usersDB(t), store.WithHooks(h))
	u := &models.User{Email: "a@example.com", Firstname: "Ada"}
	require.NoError(t, s.CreateUser(ctx, u))
	unchanged := func() {
		t.Helper()
		got, err := s.FindUserByID(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, "Ada", got.Firstname, "nothing was written")
	}

	h.updateAbort = errors.New("rejected by hook")
	_, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Grace"})
	assert.ErrorIs(t, err, h.updateAbort)
	unchanged()

	h.updateAbort, h.updateRewrite = nil, behemoth.M{models.UserID: "someone-else"}
	_, err = s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Grace"})
	assert.True(t, errs.IsValidationError(err), "a hook can't change the primary key: %v", err)
	unchanged()

	h.updateRewrite = behemoth.M{"nickname": "x"}
	_, err = s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Grace"})
	assert.True(t, errs.IsValidationError(err), "a hook can't add an unknown column: %v", err)
	unchanged()
	assert.Empty(t, h.updatedAs, "AfterUpdate never ran")
}

func TestUpdateUserRejectsPrimaryKey(t *testing.T) {
	ctx := context.Background()
	s := store.New(usersDB(t))
	u := &models.User{Email: "a@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))
	_, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserID: "other"})
	assert.True(t, errs.IsValidationError(err), "%v", err)
}

func TestUpdateUser(t *testing.T) {
	ctx := context.Background()
	t1 := t0.Add(time.Hour)
	s := store.New(usersDB(t), store.WithClock(fixedClock(t0, t1)), store.WithIDs(sequentialIDs()))
	u := &models.User{Email: "a@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))

	updated, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserEmail: " New@Example.com", models.UserFirstname: "Ada"})
	require.NoError(t, err)
	assert.Equal(t, "new@example.com", updated.Email)
	assert.Equal(t, "Ada", updated.Firstname)
	assert.True(t, updated.UpdatedAt.Equal(t1), "updated_at is stamped, got %v", updated.UpdatedAt)
	assert.True(t, updated.CreatedAt.Equal(t0), "created_at is untouched")

	_, err = s.UpdateUser(ctx, u.ID, behemoth.M{"firstnmae": "typo"})
	require.Error(t, err)
	assert.True(t, errs.IsValidationError(err), "an unknown column is a validation error: %v", err)
	assert.Contains(t, errors.Unwrap(err).Error(), "firstnmae", "the cause names the column")
	unchanged, err := s.FindUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "Ada", unchanged.Firstname, "a rejected update writes nothing")
}

// A delete also reads and clears sessions, accounts and tokens, so it needs
// a database with core's tables.
func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	tables, resolver := coreSchema(t)
	s := store.New(contractBackends()["sql/sqlite"](t, tables, resolver), store.WithSchema(resolver))
	u := &models.User{Email: "a@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))
	require.NoError(t, s.DeleteUser(ctx, u.ID))
	_, err := s.FindUserByID(ctx, u.ID)
	assert.True(t, errs.IsNotFound(err), "%v", err)
}

// usernamePlugin registers a data.user.beforeCreate handler — the real
// dispatch path a plugin would use.
type usernamePlugin struct{}

func (usernamePlugin) Meta() core.PluginMeta                 { return core.PluginMeta{Name: "usernames"} }
func (usernamePlugin) Version() string                       { return "0.0.0" }
func (usernamePlugin) Init(*core.AuthContext) error          { return nil }
func (usernamePlugin) Routes() []httpapi.Route               { return nil }
func (usernamePlugin) Middlewares() []httpapi.Middleware     { return nil }
func (usernamePlugin) Declare(*core.PluginInitContext) error { return nil }
func (usernamePlugin) Register(reg core.HookRegistry) error {
	if err := reg.OnBefore(hooks.HookUserBeforeCreate, func(hctx *core.HookContext, row behemoth.M) (behemoth.M, error) {
		email, _ := row[models.UserEmail].(string)
		row[models.UserFirstname] = strings.SplitN(email, "@", 2)[0]
		return row, nil
	}, nil); err != nil {
		return err
	}
	// On update, keep the username in step with a changed email, and record
	// which user it was (published in Values, not in the payload).
	return reg.OnBefore(hooks.HookUserBeforeUpdate, func(hctx *core.HookContext, changes behemoth.M) (behemoth.M, error) {
		if email, ok := changes[models.UserEmail].(string); ok {
			changes[models.UserFirstname] = strings.SplitN(email, "@", 2)[0]
		}
		if id, _ := hctx.Values[hooks.HookValueUserID].(string); id != "" {
			changes[models.UserLastname] = "updated:" + id
		}
		return changes, nil
	}, nil)
}

// End to end: Boot wires AuthContext.Store to the Dispatcher, so a plugin's
// data.user.beforeCreate handler shapes the stored user.
func TestBootWiresStoreToPluginHooks(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare([]core.Plugin{usernamePlugin{}}, engine.PrepareConfig{})
	require.NoError(t, err)
	ac, err := engine.Boot(ctx, app, usersDB(t), engine.BootConfig{Crypto: crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
	}})
	require.NoError(t, err)
	require.NotNil(t, ac.Store)

	u := &models.User{Email: "grace@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, u))
	stored, err := ac.Store.FindUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "grace", stored.Firstname, "the plugin's before-create handler ran through the Dispatcher")

	updated, err := ac.Store.UpdateUser(ctx, u.ID, behemoth.M{models.UserEmail: "hopper@example.com"})
	require.NoError(t, err)
	assert.Equal(t, "hopper", updated.Firstname, "the plugin's before-update handler ran through the Dispatcher")
	assert.Equal(t, "updated:"+u.ID, updated.Lastname, "the handler was told which user")
}

// BootConfig.Hooks registers the application's handlers without a plugin.
// With no ordering constraint they run after the plugins' handlers.
func TestBootRegistersApplicationHooks(t *testing.T) {
	ctx := context.Background()
	cryptoCfg := crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
	}
	app, err := engine.Prepare([]core.Plugin{usernamePlugin{}}, engine.PrepareConfig{})
	require.NoError(t, err)

	var sawUsername any
	rejected := errors.New("rejected by the application")
	reject := false
	ac, err := engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		Crypto: cryptoCfg,
		Hooks: func(reg core.HookRegistry) error {
			if err := reg.OnBefore(hooks.HookUserBeforeCreate, func(_ *core.HookContext, row behemoth.M) (behemoth.M, error) {
				sawUsername = row[models.UserFirstname]
				row[models.UserFirstname] = "from-app"
				return row, nil
			}, nil); err != nil {
				return err
			}
			return reg.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, _ any) error {
				if hctx.Tx == nil {
					return errors.New("a data hook should get the transaction's store")
				}
				if reject {
					return rejected
				}
				return nil
			}, nil)
		},
	})
	require.NoError(t, err)

	u := &models.User{Email: "grace@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, u))
	assert.Equal(t, "grace", sawUsername, "the application's handler ran after the plugin's")
	stored, err := ac.Store.FindUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "from-app", stored.Firstname)

	reject = true
	err = ac.Store.CreateUser(ctx, &models.User{Email: "ada@example.com"})
	assert.ErrorIs(t, err, rejected)
	_, err = ac.Store.FindUserByEmail(ctx, "ada@example.com")
	assert.True(t, errs.IsNotFound(err), "the application's after-hook error rolled the user back: %v", err)

	// A registration error is a boot error.
	app, err = engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	_, err = engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		Crypto: cryptoCfg,
		Hooks: func(reg core.HookRegistry) error {
			return reg.OnBefore("no.such.point", func(_ *core.HookContext, p behemoth.M) (behemoth.M, error) { return p, nil }, nil)
		},
	})
	require.Error(t, err, "registering on an undeclared point fails Boot")
}

// The application's owner name comes from PrepareConfig.AppName, is usable
// in ordering constraints, and can't collide with core or a plugin.
func TestAppNameIsConfigurableAndChecked(t *testing.T) {
	ctx := context.Background()
	cryptoCfg := crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
	}
	pluginName := usernamePlugin{}.Meta().Name

	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	assert.Equal(t, "app", app.AppName, "the default name")

	app, err = engine.Prepare([]core.Plugin{usernamePlugin{}}, engine.PrepareConfig{AppName: "shop"})
	require.NoError(t, err)
	assert.Equal(t, "shop", app.AppName)

	// The application asks to run before the plugin, which names it "shop".
	var sawUsername any = "unset"
	ac, err := engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		Crypto: cryptoCfg,
		Hooks: func(reg core.HookRegistry) error {
			return reg.OnBefore(hooks.HookUserBeforeCreate, func(_ *core.HookContext, row behemoth.M) (behemoth.M, error) {
				sawUsername = row[models.UserFirstname]
				return row, nil
			}, &core.HookOptions{Before: []string{pluginName}})
		},
	})
	require.NoError(t, err)
	require.NoError(t, ac.Store.CreateUser(ctx, &models.User{Email: "grace@example.com"}))
	assert.NotEqual(t, "grace", sawUsername, "the application's handler ran before the plugin's")

	for name, cfg := range map[string]engine.PrepareConfig{
		"an application named core":   {AppName: "core"},
		"a plugin named like the app": {AppName: pluginName},
	} {
		_, err = engine.Prepare([]core.Plugin{usernamePlugin{}}, cfg)
		assert.Error(t, err, name)
	}
}

// An owner may register one handler per point; a second is a boot error
// instead of being dropped by the chain ordering.
func TestSecondHandlerOnAPointIsRejected(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	pass := func(_ *core.HookContext, row behemoth.M) (behemoth.M, error) { return row, nil }

	_, err = engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
		Hooks: func(reg core.HookRegistry) error {
			if err := reg.OnBefore(hooks.HookUserBeforeCreate, pass, nil); err != nil {
				return err
			}
			// A different priority doesn't make it a different handler slot.
			return reg.OnBefore(hooks.HookUserBeforeCreate, pass, &core.HookOptions{Priority: core.PriorityLow})
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(hooks.HookUserBeforeCreate))
	assert.Contains(t, err.Error(), `"app"`)
}

// capturingDriver keeps the routes Boot mounts, so a test can call them.
type capturingDriver struct{ routes []httpapi.Route }

func (d *capturingDriver) Mount(routes ...httpapi.Route) error {
	d.routes = append(d.routes, routes...)
	return nil
}

// signupPlugin creates a user from a route, through the store only; its
// data hook names the user after a request header — which the hook can only
// see if the request travelled through the context.
type signupPlugin struct{ ac *core.AuthContext }

func (p *signupPlugin) Meta() core.PluginMeta                 { return core.PluginMeta{Name: "signup"} }
func (p *signupPlugin) Version() string                       { return "0.0.0" }
func (p *signupPlugin) Init(ac *core.AuthContext) error       { p.ac = ac; return nil }
func (p *signupPlugin) Middlewares() []httpapi.Middleware     { return nil }
func (p *signupPlugin) Declare(*core.PluginInitContext) error { return nil }

func (p *signupPlugin) Routes() []httpapi.Route {
	return []httpapi.Route{{Method: http.MethodPost, Path: "/sign-up", Handler: func(rctx *httpapi.RequestContext) error {
		u := &models.User{Email: rctx.Request.URL.Query().Get("email")}
		if err := p.ac.Store.CreateUser(rctx.Ctx, u); err != nil {
			return err
		}
		return rctx.Response.JSON(http.StatusCreated, behemoth.M{"id": u.ID})
	}}}
}

func (p *signupPlugin) Register(reg core.HookRegistry) error {
	return reg.OnBefore(hooks.HookUserBeforeCreate, func(hctx *core.HookContext, row behemoth.M) (behemoth.M, error) {
		if rctx := httpapi.RequestFrom(hctx.Ctx); rctx == nil {
			row[models.UserFirstname] = "no-request"
		} else {
			row[models.UserFirstname] = rctx.Request.Header.Get("X-Username")
		}
		return row, nil
	}, nil)
}

func TestDataHooksSeeTheRequestBeingHandled(t *testing.T) {
	ctx := context.Background()
	plugin := &signupPlugin{}
	app, err := engine.Prepare([]core.Plugin{plugin}, engine.PrepareConfig{})
	require.NoError(t, err)
	driver := &capturingDriver{}
	ac, err := engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		HTTP: driver,
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
	})
	require.NoError(t, err)
	require.Len(t, driver.routes, 1)

	// Called the way every adapter calls a route: a RequestContext built from
	// the native request, with the request's own context.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/sign-up?email=ada@example.com", nil)
	req.Header.Set("X-Username", "from-header")
	rctx := &httpapi.RequestContext{Ctx: req.Context(), Request: req, Response: httpapi.NewResponseRecorder(), Values: behemoth.M{}}
	require.NoError(t, driver.routes[0].Handler(rctx))
	w := httptest.NewRecorder()
	rctx.Response.Flush(w)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	created, err := ac.Store.FindUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	assert.Equal(t, "from-header", created.Firstname, "the data hook saw the request the route was handling")

	// The same write outside any request: the hook is told there is none.
	outside := &models.User{Email: "job@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, outside))
	assert.Equal(t, "no-request", outside.Firstname)
}

// noTransactionsDB is a database whose deployment has no transactions, the
// way the MongoDB adapter reports a standalone server.
type noTransactionsDB struct {
	behemoth.Database
	err error
}

func (d noTransactionsDB) CheckTransactions(context.Context) error { return d.err }

// Boot asks a database that can tell whether its transactions work, and
// refuses to start when they don't: hooked writes would fail later.
func TestBootChecksTheDatabaseHasTransactions(t *testing.T) {
	ctx := context.Background()
	cfg := engine.BootConfig{Crypto: crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
	}}
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)

	standalone := errs.NewConfigurationError("test", "standalone server", nil)
	_, err = engine.Boot(ctx, app, noTransactionsDB{Database: usersDB(t), err: standalone}, cfg)
	require.Error(t, err)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	assert.ErrorIs(t, err, standalone)

	_, err = engine.Boot(ctx, app, noTransactionsDB{Database: usersDB(t)}, cfg)
	assert.NoError(t, err, "a database whose check passes boots")
}

// The store calls Begin once per hooked write and runs the write's Before and
// After calls with the context it returned. A table without hooks begins
// nothing.
func TestBeginScopesEachHookedWrite(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	s := store.New(usersDB(t), store.WithHooks(h))

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))
	_, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Ada"})
	require.NoError(t, err)
	assert.Equal(t, 2, h.begun)
	assert.Equal(t, []any{1, 1, 2, 2}, h.writes, "before and after of a write share the context Begin returned")

	h.silent = true
	require.NoError(t, s.CreateUser(ctx, &models.User{Email: "grace@example.com"}))
	assert.Equal(t, 2, h.begun, "a table that fires no hooks begins no write")
}

// Callbacks a hook queues with AfterCommit wait for the commit: they run
// after a single write, and not at all when the write is rolled back.
func TestCommittedHooksFireAfterCommitOnly(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	s := store.New(usersDB(t), store.WithHooks(h))

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, s.CreateUser(ctx, u))
	_, err := s.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Ada"})
	require.NoError(t, err)
	assert.Equal(t, []string{"created users " + u.ID, "updated users " + u.ID}, h.committed)

	// An after hook that fails rolls the write back: nothing committed.
	h.committed = nil
	h.afterCreate = func(context.Context, *store.Store, behemoth.Model) error { return errors.New("rejected") }
	require.Error(t, s.CreateUser(ctx, &models.User{Email: "grace@example.com"}))
	assert.Empty(t, h.committed, "a rolled-back write is never reported as committed")
}

// Inside a caller's transaction the notifications wait for the outermost
// commit, the way sign-up creates a user and its account together.
func TestCommittedHooksWaitForTheOutermostTransaction(t *testing.T) {
	ctx := context.Background()
	h := &recordingHooks{}
	s := store.New(usersDB(t), store.WithHooks(h))

	// A later step fails: the user is rolled back and never reported.
	failed := errors.New("account insert failed")
	err := s.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, &models.User{Email: "ada@example.com"}); err != nil {
			return err
		}
		return failed
	})
	require.ErrorIs(t, err, failed)
	assert.Empty(t, h.committed)
	_, err = s.FindUserByEmail(ctx, "ada@example.com")
	assert.True(t, errs.IsNotFound(err), "%v", err)

	// Everything succeeds: nothing is reported while the transaction is
	// open, and everything is, in order, once it has committed.
	var during []string
	var extra []string
	u := &models.User{Email: "ada@example.com"}
	err = s.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, u); err != nil {
			return err
		}
		tx.AfterCommit(ctx, func(context.Context) { extra = append(extra, "callback") })
		// A helper that opens its own transaction joins this one.
		if err := tx.Transaction(ctx, func(ctx context.Context, inner *store.Store) error {
			_, err := inner.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Ada"})
			return err
		}); err != nil {
			return err
		}
		during = append(during, h.committed...)
		during = append(during, extra...)
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, during, "nothing fires before the outermost commit")
	assert.Equal(t, []string{"created users " + u.ID, "updated users " + u.ID}, h.committed)
	assert.Equal(t, []string{"callback"}, extra)

	// Outside a transaction there is nothing to wait for.
	ran := false
	s.AfterCommit(ctx, func(context.Context) { ran = true })
	assert.True(t, ran)
}

// A callback that panics after the commit does not undo the caller's view of
// the write: Transaction returns nil, the callbacks queued behind it run, and
// the panic is logged. The same holds for a callback run at once on a store
// that is not bound to a transaction.
func TestAfterCommitCallbackPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	tel, rec := telemetrytest.New()
	s := store.New(usersDB(t), store.WithTelemetry(tel))

	var ran []string
	err := s.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, &models.User{Email: "ada@example.com"}); err != nil {
			return err
		}
		tx.AfterCommit(ctx, func(context.Context) { ran = append(ran, "first") })
		tx.AfterCommit(ctx, func(context.Context) { panic("boom") })
		tx.AfterCommit(ctx, func(context.Context) { ran = append(ran, "third") })
		return nil
	})
	require.NoError(t, err, "the write committed")
	assert.Equal(t, []string{"first", "third"}, ran, "the callback behind the panic still runs")
	_, err = s.FindUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)

	logged := rec.Logger.At(slog.LevelError)
	require.Len(t, logged, 1)
	assert.Equal(t, "after-commit callback panicked", logged[0].Message)
	assert.Equal(t, "store", logged[0].Fields[telemetry.FieldComponent])
	assert.Contains(t, logged[0].Fields[telemetry.FieldError], "boom")

	rec.Logger.Reset()
	assert.NotPanics(t, func() { s.AfterCommit(ctx, func(context.Context) { panic("unbound") }) })
	assert.Len(t, rec.Logger.At(slog.LevelError), 1)

	// Without a Telemetry there is no logger; the panic is still recovered.
	quiet := store.New(usersDB(t))
	assert.NotPanics(t, func() { quiet.AfterCommit(ctx, func(context.Context) { panic("quiet") }) })
}

// memoryKV is a KeyValueStorage in a map, enough to act as the session cache.
// It is a core.VersionedCache too, as the Redis adapter is. Wrap it in a
// plainKV for a storage without the capability.
type memoryKV struct {
	mu       sync.Mutex
	entries  map[string]string
	versions map[string]int64         // the version SetIfNewer wrote with an entry; none for Set's
	ttls     map[string]time.Duration // the lifetime an entry was written with
}

var _ core.VersionedCache = (*memoryKV)(nil)

func (k *memoryKV) Get(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.entries[key]
	if !ok {
		return "", errors.New("key not found")
	}
	return v, nil
}

func (k *memoryKV) Set(_ context.Context, key, value string, ttl int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.write(key, value, time.Duration(ttl)*time.Second)
	delete(k.versions, key)
	return nil
}

func (k *memoryKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, key)
	delete(k.versions, key)
	delete(k.ttls, key)
	return nil
}

func (k *memoryKV) SetIfNewer(_ context.Context, key, value string, version int64, ttl time.Duration) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if stored, ok := k.versions[key]; ok && stored >= version {
		return false, nil
	}
	k.write(key, value, ttl)
	if k.versions == nil {
		k.versions = map[string]int64{}
	}
	k.versions[key] = version
	return true, nil
}

func (k *memoryKV) GetVersioned(_ context.Context, key string) (string, int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	version, ok := k.versions[key]
	if !ok {
		return "", 0, errors.New("key not found")
	}
	return k.entries[key], version, nil
}

// write stores an entry. The caller holds mu.
func (k *memoryKV) write(key, value string, ttl time.Duration) {
	if k.entries == nil {
		k.entries = map[string]string{}
	}
	if k.ttls == nil {
		k.ttls = map[string]time.Duration{}
	}
	k.entries[key], k.ttls[key] = value, ttl
}

// clear drops every entry with its version, as a storage that evicts or
// loses its keys does.
func (k *memoryKV) clear() {
	k.mu.Lock()
	defer k.mu.Unlock()
	clear(k.entries)
	clear(k.versions)
	clear(k.ttls)
}

// session returns the copy of a session cached under its lookup hash, or nil
// when the entry holds none: no entry, or a value that is not a session.
func (k *memoryKV) session(lookupHash string) *models.Session {
	k.mu.Lock()
	defer k.mu.Unlock()
	var m models.Session
	if json.Unmarshal([]byte(k.entries["session:"+lookupHash]), &m) != nil {
		return nil
	}
	return &m
}

// ttl returns the lifetime the entry under a session's lookup hash was
// written with.
func (k *memoryKV) ttl(lookupHash string) time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ttls["session:"+lookupHash]
}

// plainKV hides the core.VersionedCache of the storage it wraps: a storage
// that can't order its writes.
type plainKV struct{ behemoth.KeyValueStorage }

// The store reports a delete to its hooks around the rows it removes, in the
// caller's transaction: a refusal or a failing after hook leaves every row,
// and the committed notification waits for the commit.
func TestDeleteUserCallsItsHooksInOneTransaction(t *testing.T) {
	ctx := context.Background()
	tables, resolver := coreSchema(t)
	h := &recordingHooks{}
	st := store.New(contractBackends()["sql/sqlite"](t, tables, resolver), store.WithSchema(resolver), store.WithHooks(h))

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, st.CreateUser(ctx, u))
	require.NoError(t, st.CreateSession(ctx, &models.Session{UserID: u.ID, LookupHash: "lookup-1", State: models.SessionActive}))
	require.NoError(t, st.CreateAccount(ctx, &models.Account{UserID: u.ID, ProviderID: models.ProviderCredential, AccountID: u.ID}))
	require.NoError(t, st.CreateToken(ctx, &models.Token{Kind: "reset", Subject: u.ID, LookupHash: "token-1"}))
	require.NoError(t, st.CreateToken(ctx, &models.Token{Kind: "reset", Subject: "someone-else", LookupHash: "token-2"}))
	h.committed = nil
	remaining := func() (users, sessions, accounts, tokens int64) {
		all := func(m behemoth.Model, pk string) int64 {
			n, err := st.DB().Count(ctx, m, clause.Expression{Conditions: []clause.Condition{{Field: pk, Operator: clause.OpNotEqual, Value: ""}}})
			require.NoError(t, err)
			return n
		}
		return all(&models.User{}, models.UserID), all(&models.Session{}, models.SessionID),
			all(&models.Account{}, models.AccountID), all(&models.Token{}, models.TokenID)
	}

	h.deleteAbort = errors.New("has an open invoice")
	require.ErrorIs(t, st.DeleteUser(ctx, u.ID), h.deleteAbort, "the before hook's error is returned as it is")
	h.deleteAbort = nil
	h.afterDelete = func(context.Context, *store.Store) error { return errors.New("after hook failed") }
	require.Error(t, st.DeleteUser(ctx, u.ID))
	users, sessions, accounts, tokens := remaining()
	assert.Equal(t, []int64{1, 1, 1, 2}, []int64{users, sessions, accounts, tokens}, "a refused or failed delete removes nothing")
	assert.Empty(t, h.committed)

	h.afterDelete, h.deletes = nil, nil
	require.NoError(t, st.DeleteUser(ctx, u.ID))
	assert.Equal(t, []string{"before users " + u.ID, "after users " + u.ID + " sessions=1"}, h.deletes)
	assert.Equal(t, []string{"deleted users " + u.ID}, h.committed)
	users, sessions, accounts, tokens = remaining()
	assert.Equal(t, []int64{0, 0, 0, 1}, []int64{users, sessions, accounts, tokens}, "another subject's token stays")

	err := st.DeleteUser(ctx, u.ID)
	assert.True(t, errs.IsNotFound(err), "a user that does not exist: %v", err)
}

// Under Boot a deleted user's sessions end the way a revoked one does: their
// cache entries are gone, auth.session.afterRevoke fires for each one that
// was live, and the delete has its own points. With a cached session and a
// cascading foreign key alone, the token would keep working from the cache.
func TestDeleteUserUnderBoot(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	sqlDB, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "delete.db")+"?_foreign_keys=on&_busy_timeout=10000")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	createTables(t, sqliteAdapter.NewSQLiteDriver(sqlDB, app.Resolver), app.Schemas.All())

	var seen []string
	var veto error
	before := func(name string) core.BeforeHookFunc {
		return func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			seen = append(seen, fmt.Sprint(name, " tx=", hctx.Tx != nil))
			if name == "beforeDelete" {
				assert.Equal(t, "ada@example.com", payload[models.UserEmail], "the payload is the user row")
				assert.Equal(t, payload[models.UserID], hctx.Values[hooks.HookValueUserID])
				return nil, veto
			}
			return nil, nil
		}
	}
	after := func(name string) core.AfterHookFunc {
		return func(hctx *core.HookContext, result any) error {
			line := fmt.Sprint(name, " tx=", hctx.Tx != nil)
			if sess, ok := result.(*models.Session); ok {
				line += fmt.Sprint(" ", sess.State, " ", sess.RevokedReason, " at=", sess.RevokedAt != nil)
			}
			seen = append(seen, line)
			return nil
		}
	}
	kv := &memoryKV{entries: map[string]string{}}
	tel, rec := telemetrytest.New()
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(sqlDB, app.Resolver), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
		Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute},
		KV:        kv,
		Telemetry: tel,
		Hooks: func(reg core.HookRegistry) error {
			for point, name := range map[core.HookPoint]string{
				hooks.HookUserBeforeDelete: "beforeDelete", hooks.HookSessionBeforeRevoke: "beforeRevoke",
			} {
				if err := reg.OnBefore(point, before(name), nil); err != nil {
					return err
				}
			}
			for point, name := range map[core.HookPoint]string{
				hooks.HookUserAfterDelete: "afterDelete", hooks.HookUserDeleted: "deleted", hooks.HookSessionAfterRevoke: "afterRevoke",
			} {
				if err := reg.OnAfter(point, after(name), nil); err != nil {
					return err
				}
			}
			return nil
		},
	})
	require.NoError(t, err)

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, u))
	liveSession, live, err := ac.SessionManager.Create(ctx, u.ID, core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	old, _, err := ac.SessionManager.Create(ctx, u.ID, core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	require.NoError(t, ac.SessionManager.Revoke(ctx, old.ID, "user_logout"))
	_, err = ac.SessionManager.Validate(ctx, live)
	require.NoError(t, err)
	require.NotNil(t, kv.session(liveSession.LookupHash), "the live session is cached")
	require.Equal(t, models.SessionRevoked, kv.session(old.LookupHash).State, "the revoked session is cached as revoked")

	// A handler on data.user.beforeDelete refuses: nothing changes.
	seen, veto = nil, errs.NewInvalidInputError("billing", "user", "the user has an open invoice", nil)
	require.ErrorIs(t, ac.Store.DeleteUser(ctx, u.ID), veto)
	assert.Equal(t, []string{"beforeDelete tx=true"}, seen)
	_, err = ac.SessionManager.Validate(ctx, live)
	require.NoError(t, err, "the session of a user that was not deleted still works")

	seen, veto = nil, nil
	require.NoError(t, ac.Store.DeleteUser(ctx, u.ID))
	assert.Equal(t, []string{
		"beforeDelete tx=true",
		"afterDelete tx=true",
		"afterRevoke tx=false revoked user_deleted at=true",
		"deleted tx=false",
	}, seen, "one afterRevoke: the session revoked earlier was reported then, and beforeRevoke does not fire")
	assert.Nil(t, kv.session(liveSession.LookupHash), "the cache holds no copy of the deleted session")
	assert.Nil(t, kv.session(old.LookupHash), "nor of the one revoked earlier")
	_, err = ac.SessionManager.Validate(ctx, live)
	assert.True(t, errs.IsNotFound(err), "the deleted user's token: %v", err)

	deleted := rec.Audit.OfType(telemetry.AuditUserDeleted)
	require.Len(t, deleted, 1)
	assert.Equal(t, u.ID, deleted[0].SubjectID)
	assert.Len(t, rec.Audit.OfType(string(hooks.HookSessionAfterRevoke)), 2, "the sign-out earlier, and the session that ended with the user")
}

// A lookup that read a session before its user was deleted can't put it
// back in the cache. Evict deletes the entry in the delete's transaction, and
// Discard, after the commit, leaves a gone value that no copy of the session
// can replace. See ADR-0011.
func TestDeletedUsersSessionStaysOutOfTheCache(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	sqlDB, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "delete.db")+"?_foreign_keys=on&_busy_timeout=10000")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	createTables(t, sqliteAdapter.NewSQLiteDriver(sqlDB, app.Resolver), app.Schemas.All())
	kv := &interleavingKV{memoryKV: &memoryKV{}}
	ac, err := engine.Boot(ctx, app, sqliteAdapter.NewSQLiteAdapter(sqlDB, app.Resolver), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour},
		KV:      kv,
	})
	require.NoError(t, err)

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, u))
	sess, raw, err := ac.SessionManager.Create(ctx, u.ID, core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	kv.clear() // the entry is evicted, so the next lookup reads the row

	kv.beforeSet = func() { require.NoError(t, ac.Store.DeleteUser(ctx, u.ID)) }
	_, err = ac.SessionManager.Validate(ctx, raw)
	require.NoError(t, err, "the lookup that read the row before the delete succeeds")
	require.Nil(t, kv.beforeSet, "the user was deleted between the read and the cache write")

	assert.Nil(t, kv.session(sess.LookupHash), "the cache holds no copy of the session")
	assert.Equal(t, time.Hour, kv.ttl(sess.LookupHash), "the gone value outlives any copy of the session")
	_, err = ac.SessionManager.Validate(ctx, raw)
	assert.True(t, errs.IsNotFound(err), "the deleted user's token: %v", err)
}

// pausingDB is a database that runs a function once, just before its next
// delete of sessions, in a transaction or not. It puts a concurrent
// operation between Store.DeleteUser's list of a user's sessions and its
// delete of them.
type pausingDB struct {
	behemoth.Database
	pause *func() // shared with the databases of its transactions
}

func newPausingDB(db behemoth.Database) *pausingDB {
	return &pausingDB{Database: db, pause: new(func())}
}

// beforeSessionsDelete sets the function to run.
func (d *pausingDB) beforeSessionsDelete(f func()) { *d.pause = f }

func (d *pausingDB) DeleteMany(ctx context.Context, m behemoth.Model, expr clause.Expression) error {
	if _, ok := m.(*models.Session); ok {
		if f := *d.pause; f != nil {
			*d.pause = nil
			f()
		}
	}
	return d.Database.DeleteMany(ctx, m, expr)
}

func (d *pausingDB) Transaction(ctx context.Context, fn behemoth.TransactionFunc) error {
	return d.Database.Transaction(ctx, func(ctx context.Context, tx behemoth.Database) (any, error) {
		return fn(ctx, &pausingDB{Database: tx, pause: d.pause})
	})
}

// A session that starts, or moves to a new token, between Store.DeleteUser's
// list of the user's sessions and its delete of them keeps working from the
// session cache after the delete (docs/ongoing.md, M35). The delete removes
// its row by user_id, but the cache entries it ends are those of the
// sessions it listed, under the tokens they had then. The test fails until
// that is fixed.
//
// It runs on PostgreSQL and MySQL, where another connection can write the
// user's sessions while the delete's transaction is open, until the delete
// reaches them.
func TestSessionsChangedDuringTheirUsersDeletionStayOutOfTheCache(t *testing.T) {
	t.Skip("M35")
	for _, backend := range []string{"sql/postgres", "sql/mysql"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			app, err := engine.Prepare(nil, engine.PrepareConfig{})
			require.NoError(t, err)
			db := newPausingDB(contractBackends()[backend](t, app.Schemas.All(), app.Resolver))
			ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
				Crypto: crypto.Config{
					Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
				},
				Session: core.SessionConfig{ExpiresIn: time.Hour},
				KV:      &memoryKV{},
			})
			require.NoError(t, err)

			t.Run("a promotion", func(t *testing.T) {
				u := &models.User{Email: "ada@example.com"}
				require.NoError(t, ac.Store.CreateUser(ctx, u))
				pending, _, err := ac.SessionManager.Create(ctx, u.ID, core.SessionMeta{State: models.SessionPending})
				require.NoError(t, err)

				var raw string
				db.beforeSessionsDelete(func() {
					var err error
					_, raw, err = ac.SessionManager.Promote(ctx, pending.ID, core.FactorTOTP)
					require.NoError(t, err, "the second factor passes, and the session moves to a new token")
				})
				require.NoError(t, ac.Store.DeleteUser(ctx, u.ID))
				require.NotEmpty(t, raw, "the promotion ran between the delete's list and its delete")
				_, err = ac.Store.FindSessionByID(ctx, pending.ID)
				require.True(t, errs.IsNotFound(err), "the delete removed the promoted session's row: %v", err)

				_, err = ac.SessionManager.Validate(ctx, raw)
				assert.True(t, errs.IsNotFound(err), "the token the promotion gave out: %v", err)
			})

			t.Run("a sign-in", func(t *testing.T) {
				u := &models.User{Email: "grace@example.com"}
				require.NoError(t, ac.Store.CreateUser(ctx, u))

				var sess *models.Session
				var raw string
				db.beforeSessionsDelete(func() {
					var err error
					sess, raw, err = ac.SessionManager.Create(ctx, u.ID, core.SessionMeta{State: models.SessionActive})
					require.NoError(t, err, "a sign-in creates its session")
				})
				require.NoError(t, ac.Store.DeleteUser(ctx, u.ID))
				require.NotNil(t, sess, "the sign-in ran between the delete's list and its delete")
				_, err = ac.Store.FindSessionByID(ctx, sess.ID)
				require.True(t, errs.IsNotFound(err), "the delete removed the new session's row: %v", err)

				_, err = ac.SessionManager.Validate(ctx, raw)
				assert.True(t, errs.IsNotFound(err), "the token of the sign-in: %v", err)
			})
		})
	}
}

// Under Boot the notifications are the data.user.created and
// data.user.updated points: a handler sees only committed users, has no
// transaction, and can't fail the write.
func TestCommittedDataPointsUnderBoot(t *testing.T) {
	ctx := context.Background()
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)

	var seen []string
	ac, err := engine.Boot(ctx, app, usersDB(t), engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
		Hooks: func(reg core.HookRegistry) error {
			if err := reg.OnAfter(hooks.HookUserCreated, func(hctx *core.HookContext, result any) error {
				seen = append(seen, "created "+result.(*models.User).Email)
				assert.Nil(t, hctx.Tx, "the transaction is over")
				// The row is visible on the root store's connection.
				_, err := hctx.Auth.Store.FindUserByID(hctx.Ctx, result.(*models.User).ID)
				assert.NoError(t, err)
				return errors.New("a handler error is logged, not returned")
			}, nil); err != nil {
				return err
			}
			return reg.OnAfter(hooks.HookUserUpdated, func(hctx *core.HookContext, result any) error {
				seen = append(seen, fmt.Sprint("updated ", hctx.Values[hooks.HookValueUserID] == result.(*models.User).ID))
				return nil
			}, nil)
		},
	})
	require.NoError(t, err)

	u := &models.User{Email: "ada@example.com"}
	require.NoError(t, ac.Store.CreateUser(ctx, u), "an after-commit handler's error does not fail the write")
	_, err = ac.Store.UpdateUser(ctx, u.ID, behemoth.M{models.UserFirstname: "Ada"})
	require.NoError(t, err)

	failed := errors.New("later step failed")
	err = ac.Store.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		if err := tx.CreateUser(ctx, &models.User{Email: "grace@example.com"}); err != nil {
			return err
		}
		return failed
	})
	require.ErrorIs(t, err, failed)

	assert.Equal(t, []string{"created ada@example.com", "updated true"}, seen)
}

// Purge runs every purge when one fails, and its error names each table that
// failed. This database has no rate_limits, verification_codes or
// reserved_values table.
func TestPurgeRunsEveryPurge(t *testing.T) {
	ctx := context.Background()
	st := store.New(sessionsTokensDB(t))
	tok := &models.Token{Kind: kindReset, Subject: "u1", LookupHash: "purge", TokenHash: "h", KeyVersion: 1, ExpiresAt: st.Now().Add(-time.Hour)}
	require.NoError(t, st.CreateToken(ctx, tok))

	err := st.Purge(ctx, store.PurgeOptions{TokensBefore: st.Now()})
	require.Error(t, err)
	for _, table := range []string{models.RateLimitTable, models.VerificationCodeTable, models.ReservedValueTable} {
		assert.Contains(t, err.Error(), "store: purge "+table+":")
	}
	assert.NotContains(t, err.Error(), "purge "+models.TokenTable)
	_, err = st.FindTokenByID(ctx, tok.ID)
	assert.True(t, errs.IsNotFound(err), "the tokens were purged: %v", err)
}
