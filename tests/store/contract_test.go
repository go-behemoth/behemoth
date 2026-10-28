package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/auth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/schema"
	bunAdapter "github.com/go-behemoth/behemoth/storage/adapters/bun"
	gormAdapter "github.com/go-behemoth/behemoth/storage/adapters/gorm"
	mongoAdapter "github.com/go-behemoth/behemoth/storage/adapters/mongo"
	mysqlAdapter "github.com/go-behemoth/behemoth/storage/adapters/mysql"
	pgAdapter "github.com/go-behemoth/behemoth/storage/adapters/postgres"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	sqlserverAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlserver"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mssqldialect"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.mongodb.org/mongo-driver/bson"
	gormmysql "gorm.io/driver/mysql"
	gormpostgres "gorm.io/driver/postgres"
	gormsqlite "gorm.io/driver/sqlite"
	gormsqlserver "gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// handle is a unique column a plugin contributes to users, stored under
// another physical name and with a conflict code of its own.
var handle = schema.Field[string]{Table: models.UserTable, Name: "handle"}

// coreSchema is what Prepare would freeze: core's tables, plus three plugin
// contributions to users (plan stored under another physical name, and
// handle, unique, under another physical name too). The
// tables come without their foreign keys: the contract writes sessions and
// accounts for users that don't exist.
func coreSchema(t *testing.T) ([]schema.Table, behemoth.SchemaResolver) {
	t.Helper()
	tables, resolver := coreSchemaWithForeignKeys(t)
	for i := range tables {
		tables[i].ForeignKeys = nil
	}
	return tables, resolver
}

// coreSchemaWithForeignKeys is coreSchema as a migration creates it, with
// the foreign keys of sessions and accounts to users.
func coreSchemaWithForeignKeys(t *testing.T) ([]schema.Table, behemoth.SchemaResolver) {
	t.Helper()
	reg := schema.NewRegistry()
	require.NoError(t, reg.Declare(&models.User{}, models.UserTableSchema()))
	require.NoError(t, reg.Declare(&models.Session{}, models.SessionTableSchema()))
	require.NoError(t, reg.Declare(&models.Token{}, models.TokenTableSchema()))
	require.NoError(t, reg.Declare(&models.Account{}, models.AccountTableSchema()))
	require.NoError(t, reg.Declare(&models.RateLimit{}, models.RateLimitTableSchema()))
	require.NoError(t, reg.Declare(&models.AuditLog{}, models.AuditLogTableSchema()))
	require.NoError(t, reg.Declare(&models.VerificationCode{}, models.VerificationCodeTableSchema()))
	require.NoError(t, reg.Declare(&models.ReservedValue{}, models.ReservedValueTableSchema()))
	tfa := twoFactorEnabled.Contribution(schema.Column{Type: schema.ColTypeBoolean, Default: false})
	tfa.Owner = "two-factor"
	require.NoError(t, reg.ExtendColumn(tfa))
	p := plan.Contribution(schema.Column{Type: schema.ColTypeString, Length: 32, Nullable: true, PhysicalName: "subscription_plan"})
	p.Owner = "billing"
	require.NoError(t, reg.ExtendColumn(p))
	h := handle.Contribution(schema.Column{Type: schema.ColTypeString, Length: 64, Nullable: true, Unique: true,
		PhysicalName: "user_handle", ConflictCode: "handle_in_use"})
	h.Owner = "profiles"
	require.NoError(t, reg.ExtendColumn(h))
	require.NoError(t, reg.Freeze())

	resolver := migration.NewSchemaResolver()
	resolver.Freeze(migration.BuildSchemaResolverTable(reg, migration.NewMigrationConfig(migration.MigrationConfig{})))
	var tables []schema.Table
	for _, name := range []string{models.UserTable, models.SessionTable, models.TokenTable, models.AccountTable, models.RateLimitTable, models.AuditLogTable, models.VerificationCodeTable, models.ReservedValueTable} {
		t, _ := reg.Lookup(name)
		tables = append(tables, t)
	}
	return tables, resolver
}

func createTables(t *testing.T, driver migration.SchemaDriver, tables []schema.Table) {
	t.Helper()
	m := migration.Migration{ID: "0001"}
	snapshot := migration.SchemaSnapshot{Version: m.ID, Tables: map[string]schema.Table{}}
	for _, table := range tables {
		m.Up = append(m.Up, migration.SchemaOperation{ID: "create_" + table.Name, Kind: migration.OpCreateTable, Table: table.Name, NewTable: &table})
		for _, idx := range table.Indexes { // CreateTable doesn't: a generated migration adds them as their own operations
			m.Up = append(m.Up, migration.SchemaOperation{ID: "add_" + idx.Name, Kind: migration.OpAddIndex, Table: table.Name, Index: &idx})
		}
		snapshot.Tables[table.Name] = table
	}
	for _, table := range tables { // after every table exists, as their own operations too
		for _, fk := range table.ForeignKeys {
			m.Up = append(m.Up, migration.SchemaOperation{ID: "add_" + fk.Name, Kind: migration.OpAddForeignKey, Table: table.Name, ForeignKey: &fk})
		}
	}
	require.NoError(t, driver.ApplyMigration(context.Background(), migration.MigrationRequest{
		Migration:      m,
		LedgerEntry:    migration.MigrationLedgerEntry{ID: m.ID, AppliedAt: time.Now()},
		SnapshotUpdate: snapshot,
		LedgerTable:    "behemoth_auth_schema",
		SnapshotTable:  "behemoth_auth_schema_snapshot",
	}))
}

// contractBackends open a database with core's tables and return an adapter
// over it. The raw adapters are the control: the contract holds for them.
func contractBackends() map[string]func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
	openSQLite := func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) *sql.DB {
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "contract.db")+"?_foreign_keys=on&_busy_timeout=10000")
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		createTables(t, sqliteAdapter.NewSQLiteDriver(db, r), tables)
		return db
	}
	openPostgres := func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) *sql.DB {
		if testing.Short() {
			t.Skip("starts a Postgres container")
		}
		ctx := context.Background()
		db, cleanup := testutils.SetupPostgresTestDB(t, ctx)
		t.Cleanup(cleanup)
		require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 30*time.Second, 200*time.Millisecond)
		createTables(t, pgAdapter.NewPostgreSQLDriver(db, r), tables)
		return db
	}
	openMySQL := func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) *sql.DB {
		if testing.Short() {
			t.Skip("starts a MySQL container")
		}
		ctx := context.Background()
		db, cleanup := testutils.SetupMySQLTestDB(t, ctx)
		t.Cleanup(cleanup)
		require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 30*time.Second, 200*time.Millisecond)
		createTables(t, mysqlAdapter.NewMySQLDriver(db, r), tables)
		return db
	}
	openSQLServer := func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) *sql.DB {
		if testing.Short() {
			t.Skip("starts a SQL Server container")
		}
		db, cleanup := testutils.SetupMSSQLTestDB(t)
		t.Cleanup(cleanup)
		require.Eventually(t, func() bool { return db.PingContext(context.Background()) == nil }, 30*time.Second, 200*time.Millisecond)
		createTables(t, sqlserverAdapter.NewSQLServerDriver(db, r), tables)
		return db
	}
	// A new Config for every database. gorm.Open stores the dialect's clause
	// builders and callbacks on the Config it is given, so one shared between
	// backends carries SQLite's rules to MySQL: SQLite drops FOR clauses, and
	// the MySQL re-count then runs without its FOR SHARE.
	quiet := func() *gorm.Config { return &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)} }

	return map[string]func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database{
		"sql/sqlite": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return sqliteAdapter.NewSQLiteAdapter(openSQLite(t, tables, r), r)
		},
		"sql/postgres": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return pgAdapter.NewPostgresAdapter(openPostgres(t, tables, r), r)
		},
		"sql/mysql": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return mysqlAdapter.NewMySQLAdapter(openMySQL(t, tables, r), r)
		},
		"sql/sqlserver": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return sqlserverAdapter.NewSQLServerAdapter(openSQLServer(t, tables, r), r)
		},
		"gorm/sqlite": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			gdb, err := gorm.Open(gormsqlite.New(gormsqlite.Config{Conn: openSQLite(t, tables, r)}), quiet())
			require.NoError(t, err)
			return gormAdapter.NewGormAdapter(gdb, r)
		},
		"gorm/postgres": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: openPostgres(t, tables, r)}), quiet())
			require.NoError(t, err)
			return gormAdapter.NewGormAdapter(gdb, r)
		},
		"gorm/mysql": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: openMySQL(t, tables, r)}), quiet())
			require.NoError(t, err)
			return gormAdapter.NewGormAdapter(gdb, r)
		},
		"bun/mysql": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return bunAdapter.NewBunAdapter(bun.NewDB(openMySQL(t, tables, r), mysqldialect.New()), r)
		},
		"gorm/sqlserver": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			gdb, err := gorm.Open(gormsqlserver.New(gormsqlserver.Config{Conn: openSQLServer(t, tables, r)}), quiet())
			require.NoError(t, err)
			return gormAdapter.NewGormAdapter(gdb, r)
		},
		"bun/sqlserver": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return bunAdapter.NewBunAdapter(bun.NewDB(openSQLServer(t, tables, r), mssqldialect.New()), r)
		},
		"bun/sqlite": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return bunAdapter.NewBunAdapter(bun.NewDB(openSQLite(t, tables, r), sqlitedialect.New()), r)
		},
		"bun/postgres": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			return bunAdapter.NewBunAdapter(bun.NewDB(openPostgres(t, tables, r), pgdialect.New()), r)
		},
		// MongoDB has no migration driver. Its indexes come from the adapter.
		"mongo": func(t *testing.T, tables []schema.Table, r behemoth.SchemaResolver) behemoth.Database {
			if testing.Short() {
				t.Skip("starts a MongoDB container")
			}
			ctx := context.Background()
			client, cleanup := testutils.SetupMongoTestDB(ctx, t)
			t.Cleanup(cleanup)
			db := mongoAdapter.NewMongoAdapter(client, "contract", r)
			require.NoError(t, db.EnsureIndexes(ctx, tables))
			return db
		},
	}
}

// TestStoreContract runs the store and the managers over every backend with
// behemoth's own models — the shapes the store really persists.
func TestStoreContract(t *testing.T) {
	tables, resolver := coreSchema(t)
	for name, open := range contractBackends() {
		t.Run(name, func(t *testing.T) {
			db := open(t, tables, resolver)
			st := store.New(db, store.WithSchema(resolver), store.WithEncryptor(testCrypto(t).AtRest))
			t.Run("Users", func(t *testing.T) { usersContract(t, st) })
			t.Run("Sessions", func(t *testing.T) { sessionsContract(t, st) })
			t.Run("Tokens", func(t *testing.T) { tokensContract(t, st) })
			t.Run("Accounts", func(t *testing.T) { accountsContract(t, st, db) })
			t.Run("RateLimits", func(t *testing.T) { rateLimitsContract(t, db, resolver) })
			t.Run("AuditLog", func(t *testing.T) { auditLogContract(t, st) })
			t.Run("VerificationCodes", func(t *testing.T) { verificationCodesContract(t, st) })
			t.Run("ReservedValues", func(t *testing.T) { reservedValuesContract(t, st) })
			t.Run("UniqueKeys", func(t *testing.T) { uniqueKeysContract(t, st, db, resolver) })
			t.Run("Purge", func(t *testing.T) { purgeContract(t, st, db, resolver) })
		})
	}
}

// A write that breaks a unique key gets an error that names the key's
// columns, canonical names, whatever the database calls the key. Through the
// store, a duplicate value of a column declared unique is the column's
// conflict (409, "<column>_taken" or the column's own code), and still a
// duplicate key. A duplicate of any other key stays a duplicate key error.
//
// The primary key is declared by no unique key, so the adapter names its
// columns from the database's catalog. The write runs in a transaction,
// which PostgreSQL aborts with the error: the lookup has to run outside it.
func uniqueKeysContract(t *testing.T, st *store.Store, db behemoth.Database, resolver behemoth.SchemaResolver) {
	ctx := context.Background()
	taken := func(err error, code string, columns []string, msgAndArgs ...any) {
		t.Helper()
		assert.True(t, errs.IsCategory(err, errs.CategoryConflict) && errs.IsCode(err, code), "want a conflict with %s, got %v", code, err)
		assert.True(t, errs.IsDuplicateKey(err), "the duplicate key stays in the chain: %v", err)
		assert.Equal(t, columns, errs.DuplicateKeyColumns(err), msgAndArgs...)
	}

	first := &models.User{Email: "keys@example.com"}
	require.NoError(t, handle.Set(first, "ada"))
	require.NoError(t, st.CreateUser(ctx, first))
	second := &models.User{Email: "keys-2@example.com"}
	require.NoError(t, st.CreateUser(ctx, second))

	err := st.CreateUser(ctx, &models.User{Email: "keys@example.com"})
	taken(err, "email_taken", []string{models.UserEmail}, "a core column: the code is the column's name with _taken")
	_, err = st.UpdateUser(ctx, second.ID, behemoth.M{models.UserEmail: "keys@example.com"})
	taken(err, "email_taken", []string{models.UserEmail}, "an update")

	again := &models.User{Email: "keys-3@example.com"}
	require.NoError(t, handle.Set(again, "ada"))
	err = st.CreateUser(ctx, again)
	taken(err, "handle_in_use", []string{handle.Name}, "a contributed column under another physical name, with a code of its own")
	_, err = st.UpdateUser(ctx, second.ID, behemoth.M{handle.Name: "ada"})
	taken(err, "handle_in_use", []string{handle.Name})

	account := &models.Account{UserID: first.ID, ProviderID: "github", AccountID: "gh-keys"}
	require.NoError(t, st.CreateAccount(ctx, account))
	err = st.CreateAccount(ctx, &models.Account{UserID: second.ID, ProviderID: "github", AccountID: "gh-keys"})
	assert.True(t, errs.IsCategory(err, errs.CategoryDuplicateKey), "a unique index stays a duplicate key: %v", err)
	assert.Equal(t, []string{models.AccountProviderID, models.AccountAccountID}, errs.DuplicateKeyColumns(err), "the index's columns, in its order")

	fixed := store.New(db, store.WithSchema(resolver), store.WithIDs(func() string { return "u-keys-fixed" }))
	require.NoError(t, fixed.CreateUser(ctx, &models.User{Email: "keys-4@example.com"}))
	err = fixed.Transaction(ctx, func(ctx context.Context, tx *store.Store) error {
		return tx.CreateUser(ctx, &models.User{Email: "keys-5@example.com"})
	})
	assert.True(t, errs.IsCategory(err, errs.CategoryDuplicateKey), "the primary key stays a duplicate key: %v", err)
	assert.Equal(t, []string{models.UserID}, errs.DuplicateKeyColumns(err), "the primary key's column, from the catalog")
}

// TestDeleteUserRemovesSessionsAndAccounts checks that nothing of a deleted
// user is left to sign in with: the user's sessions and accounts go with the
// row, and another user's stay.
//
// The databases are created the way a migration creates them, with the
// foreign keys of sessions and accounts to users (ON DELETE CASCADE). The
// store deletes the rows itself and does not rely on the cascade, so the
// result is the same where no foreign key is enforced: on MongoDB, which has
// none, and on SQLite opened without _foreign_keys=on, where SQLite ignores
// them. Both failed when the store deleted the user row only.
func TestDeleteUserRemovesSessionsAndAccounts(t *testing.T) {
	tables, resolver := coreSchemaWithForeignKeys(t)
	backends := contractBackends()
	open := map[string]func(t *testing.T) behemoth.Database{
		"sqlite, foreign keys on": func(t *testing.T) behemoth.Database {
			db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "delete-user.db")+"?_foreign_keys=on&_busy_timeout=10000&_foreign_keys=on")
			require.NoError(t, err)
			t.Cleanup(func() { db.Close() })
			createTables(t, sqliteAdapter.NewSQLiteDriver(db, resolver), tables)
			return sqliteAdapter.NewSQLiteAdapter(db, resolver)
		},
		// SQLite's default: foreign keys are declared and not enforced.
		"sqlite, foreign keys off": func(t *testing.T) behemoth.Database { return backends["sql/sqlite"](t, tables, resolver) },
		"postgres":                 func(t *testing.T) behemoth.Database { return backends["sql/postgres"](t, tables, resolver) },
		"mysql":                    func(t *testing.T) behemoth.Database { return backends["sql/mysql"](t, tables, resolver) },
		"sqlserver":                func(t *testing.T) behemoth.Database { return backends["sql/sqlserver"](t, tables, resolver) },
		"mongo":                    func(t *testing.T) behemoth.Database { return backends["mongo"](t, tables, resolver) },
	}
	for name, openDB := range open {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := store.New(openDB(t), store.WithSchema(resolver), store.WithEncryptor(testCrypto(t).AtRest))
			sm := auth.NewSessionManager(st, nil, testCrypto(t),
				core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute}, &passDispatcher{}, nil, nil)

			ada, grace := &models.User{Email: "ada@example.com"}, &models.User{Email: "grace@example.com"}
			tokens := map[*models.User]string{}
			sessions := map[*models.User]*models.Session{}
			for _, u := range []*models.User{ada, grace} {
				require.NoError(t, st.CreateUser(ctx, u))
				sess, raw, err := sm.Create(ctx, u.ID, core.SessionMeta{State: models.SessionActive})
				require.NoError(t, err)
				sessions[u], tokens[u] = sess, raw
				require.NoError(t, st.CreateAccount(ctx, &models.Account{
					UserID: u.ID, ProviderID: models.ProviderCredential, AccountID: u.ID, PasswordHash: "hash",
				}))
			}

			require.NoError(t, st.DeleteUser(ctx, ada.ID))

			_, err := st.FindUserByID(ctx, ada.ID)
			assert.True(t, errs.IsNotFound(err), "the user: %v", err)
			_, err = sm.Get(ctx, tokens[ada])
			assert.Error(t, err, "a deleted user's session token still signs a request in")
			_, err = st.FindSessionByID(ctx, sessions[ada].ID)
			assert.True(t, errs.IsNotFound(err), "the deleted user's session row is still there: %v", err)
			_, err = st.FindAccount(ctx, models.ProviderCredential, ada.ID)
			assert.True(t, errs.IsNotFound(err), "the deleted user's account, with its password hash, is still there: %v", err)

			// Another user loses nothing.
			_, err = st.FindUserByID(ctx, grace.ID)
			require.NoError(t, err)
			_, err = sm.Get(ctx, tokens[grace])
			assert.NoError(t, err, "another user's session")
			_, err = st.FindAccount(ctx, models.ProviderCredential, grace.ID)
			assert.NoError(t, err, "another user's account")
		})
	}
}

// TestGuardedUpdateInATransactionOnMySQL checks that the loser of a guarded
// update gets NotFound when it read the row earlier in its transaction. MySQL
// reports changed rows, so an adapter re-counts after an update that changed
// none; with a plain read that count sees the transaction's snapshot, where
// the guard still matches, and the lost update is reported as applied.
func TestGuardedUpdateInATransactionOnMySQL(t *testing.T) {
	ctx := context.Background()
	tables, resolver := coreSchema(t)
	for _, name := range []string{"sql/mysql", "gorm/mysql", "bun/mysql"} {
		t.Run(name, func(t *testing.T) {
			db := contractBackends()[name](t, tables, resolver)
			for round := range 3 {
				key := fmt.Sprintf("guarded-%d", round)
				require.NoError(t, db.Create(ctx, &models.RateLimit{Key: key, ExpiresAt: t0.Add(time.Hour)}))
				guard := clause.Expression{Logic: clause.OpAnd, Conditions: []clause.Condition{
					{Field: models.RateLimitKey, Operator: clause.OpEqual, Value: key},
					{Field: models.RateLimitCount, Operator: clause.OpEqual, Value: int64(0)},
				}}

				const calls = 12
				var applied, lost atomic.Int64
				var wg sync.WaitGroup
				for range calls {
					wg.Go(func() {
						err := db.Transaction(ctx, func(ctx context.Context, tx behemoth.Database) (any, error) {
							// A plain read first: it fixes the transaction's snapshot.
							if _, err := tx.Count(ctx, &models.RateLimit{}, guard); err != nil {
								return nil, err
							}
							time.Sleep(20 * time.Millisecond) // let every transaction read before one writes
							return nil, tx.UpdateOne(ctx, &models.RateLimit{}, guard, behemoth.M{models.RateLimitCount: int64(1)})
						})
						switch {
						case err == nil:
							applied.Add(1)
						case errs.IsNotFound(err):
							lost.Add(1)
						default:
							assert.NoError(t, err)
						}
					})
				}
				wg.Wait()
				assert.EqualValues(t, 1, applied.Load(), "one update wins")
				assert.EqualValues(t, calls-1, lost.Load(), "the others get NotFound")
			}
		})
	}
}

// Boot refuses a MongoDB database that lacks a declared unique index, starts
// once EnsureIndexes has created them, and only warns about a plain index.
func TestBootChecksMongoIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a MongoDB container")
	}
	ctx := context.Background()
	client, cleanup := testutils.SetupMongoTestDB(ctx, t)
	t.Cleanup(cleanup)
	app, err := engine.Prepare(nil, engine.PrepareConfig{})
	require.NoError(t, err)
	db := mongoAdapter.NewMongoAdapter(client, "boot", app.Resolver)
	tel, rec := telemetrytest.New()
	boot := func() error {
		_, err := engine.Boot(ctx, app, db, engine.BootConfig{
			Crypto: crypto.Config{
				Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("cd", 32)}, Current: 1},
			},
			Session:   core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: 5 * time.Minute},
			Telemetry: tel,
		})
		return err
	}

	err = boot()
	require.Error(t, err, "no index exists yet")
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%v", err)
	assert.Contains(t, err.Error(), `"users" (email)`)
	assert.Contains(t, err.Error(), "EnsureIndexes")

	require.NoError(t, db.EnsureIndexes(ctx, app.Schemas.All()))
	require.NoError(t, boot())
	assert.Empty(t, rec.Logger.At(slog.LevelWarn))

	// A plain index only speeds reads up: Boot starts and says so.
	_, err = client.Database("boot").Collection(models.SessionTable).Indexes().DropOne(ctx, "idx_sessions_user_id")
	require.NoError(t, err)
	require.NoError(t, boot())
	warned := rec.Logger.At(slog.LevelWarn)
	require.Len(t, warned, 1)
	assert.Contains(t, warned[0].Fields["index"], "idx_sessions_user_id")

	// A unique index is not optional.
	_, err = client.Database("boot").Collection(models.UserTable).Indexes().DropOne(ctx, "uq_users_email")
	require.NoError(t, err)
	require.Error(t, boot())
}

// TestStoreAuditLogMongo reads an audit event and a session back through the
// MongoDB adapter after the audit log part of the contract, to check how a
// date and an integer are decoded. TestStoreContract runs the contract
// itself on MongoDB.
func TestStoreAuditLogMongo(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a MongoDB container")
	}
	ctx := context.Background()
	_, resolver := coreSchema(t)
	client, cleanup := testutils.SetupMongoTestDB(ctx, t)
	t.Cleanup(cleanup)
	db := mongoAdapter.NewMongoAdapter(client, "contract", resolver)
	st := store.New(db, store.WithSchema(resolver), store.WithEncryptor(testCrypto(t).AtRest))
	auditLogContract(t, st)

	// The adapter's registry decodes a BSON date as a time.Time in UTC. With
	// the driver's default it is a primitive.DateTime, and the model reads a
	// zero time. The contract above purges its oldest events, so record one more.
	require.NoError(t, st.RecordAuditEvent(ctx, telemetry.AuditEvent{
		Type: "auth.signIn.after", Outcome: telemetry.OutcomeSuccess, ActorType: telemetry.ActorUser, ActorID: "user-1",
		RequestID: "req-utc", Timestamp: time.Date(2026, 10, 1, 14, 0, 0, 0, time.FixedZone("EAT", 3*60*60)),
	}))
	found, err := db.FindOne(ctx, &models.AuditLog{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.AuditLogRequestID, Operator: clause.OpEqual, Value: "req-utc"},
	}})
	require.NoError(t, err)
	createdAt := found.(*models.AuditLog).CreatedAt
	assert.False(t, createdAt.IsZero())
	assert.Equal(t, time.UTC, createdAt.Location())

	// The driver stores a Go int as a 32-bit integer when it fits. The
	// registry decodes it as an int64, which the model accepts; with the
	// driver's default it is an int32 and the key version reads back as zero.
	require.NoError(t, db.Create(ctx, &models.Session{ID: "s-int", UserID: "user-1", LookupHash: "lookup", KeyVersion: 3}))
	var raw bson.Raw
	raw, err = client.Database("contract").Collection(resolver.Resolve(models.SessionTable)).
		FindOne(ctx, bson.M{resolver.ResolveColumn(models.SessionTable, models.SessionID): "s-int"}).Raw()
	require.NoError(t, err)
	assert.Equal(t, bson.TypeInt32, raw.Lookup(resolver.ResolveColumn(models.SessionTable, models.SessionKeyVersion)).Type,
		"the premise: key_version is stored as a 32-bit integer")
	found, err = db.FindOne(ctx, &models.Session{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.SessionID, Operator: clause.OpEqual, Value: "s-int"},
	}})
	require.NoError(t, err)
	assert.Equal(t, 3, found.(*models.Session).KeyVersion)
}

// A session document stored before sessions had a version has no version
// field. It reads as version 0, and an update from version 0 finds it, so
// such a session can still be extended, promoted and revoked.
func TestSessionWithoutAVersionOnMongo(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a MongoDB container")
	}
	ctx := context.Background()
	tables, resolver := coreSchema(t)
	client, cleanup := testutils.SetupMongoTestDB(ctx, t)
	t.Cleanup(cleanup)
	db := mongoAdapter.NewMongoAdapter(client, "contract", resolver)
	require.NoError(t, db.EnsureIndexes(ctx, tables))
	st := store.New(db, store.WithSchema(resolver))
	sm := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{ExpiresIn: time.Hour}, &passDispatcher{}, nil, nil)

	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	sessions := client.Database("contract").Collection(resolver.Resolve(models.SessionTable))
	versionField := resolver.ResolveColumn(models.SessionTable, models.SessionVersion)
	_, err = sessions.UpdateOne(ctx, bson.M{resolver.ResolveColumn(models.SessionTable, models.SessionID): sess.ID},
		bson.M{"$unset": bson.M{versionField: ""}})
	require.NoError(t, err)

	old, err := st.FindSessionByID(ctx, sess.ID)
	require.NoError(t, err)
	assert.Zero(t, old.Version, "a missing version reads as 0")
	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
	revoked, err := st.FindSessionByID(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, models.SessionRevoked, revoked.State)
	assert.EqualValues(t, 1, revoked.Version)
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
}

func usersContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	u := &models.User{Email: " Ada@Example.com", Firstname: "Ada"}
	require.NoError(t, plan.Set(u, "pro"))
	require.NoError(t, st.CreateUser(ctx, u))
	var found *models.User
	err := st.CreateUser(ctx, &models.User{Email: "ada@example.com"})
	assert.True(t, errs.IsDuplicateKey(err), "duplicate email: %v", err)

	found, err = st.FindUserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, found.ID)
	assert.Equal(t, "Ada", found.Firstname)
	assert.False(t, found.CreatedAt.IsZero(), "timestamps read back as time.Time")
	p, _, err := plan.Get(found)
	require.NoError(t, err)
	assert.Equal(t, "pro", p, "a contributed column under another physical name round-trips")

	// A bool as the value of a condition, and a limit without an order. Both
	// need the Bun adapter's help on SQL Server, where there are no boolean
	// literals and a limit needs an order.
	unverified := clause.Expression{Conditions: []clause.Condition{
		{Field: models.UserEmailVerified, Operator: clause.OpEqual, Value: false}}}
	n, err := st.DB().Count(ctx, &models.User{}, unverified)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "a bool condition matches the stored bool")
	limited, err := st.DB().FindMany(ctx, &models.User{}, unverified, &behemoth.QueryOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, u.ID, limited[0].(*models.User).ID, "a limited read without an order returns the row's own columns")

	changes, err := twoFactorEnabled.Update(true)
	require.NoError(t, err)
	updated, err := st.UpdateUser(ctx, u.ID, changes)
	require.NoError(t, err)
	on, _, err := twoFactorEnabled.Get(updated)
	require.NoError(t, err)
	assert.True(t, on)

	// A guarded update writes only while the email is the one given.
	guarded, err := st.UpdateUserIfEmail(ctx, u.ID, u.Email, behemoth.M{models.UserFirstname: "Guarded"})
	require.NoError(t, err)
	assert.Equal(t, "Guarded", guarded.Firstname)
	_, err = st.UpdateUserIfEmail(ctx, u.ID, "someone-else@example.com", behemoth.M{models.UserFirstname: "Stale"})
	assert.True(t, errs.IsNotFound(err), "update guarded on another email: %v", err)

	_, err = st.UpdateUser(ctx, "no-such-user", behemoth.M{models.UserFirstname: "x"})
	assert.True(t, errs.IsNotFound(err), "update of a missing row: %v", err)
	require.NoError(t, st.DeleteUser(ctx, u.ID))
	assert.True(t, errs.IsNotFound(st.DeleteUser(ctx, u.ID)), "second delete is NotFound")
	_, err = st.FindUserByID(ctx, u.ID)
	assert.True(t, errs.IsNotFound(err), "%v", err)
}

func sessionsContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	sm := auth.NewSessionManager(st, nil, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute}, &passDispatcher{}, nil, nil)

	sess, raw, err := sm.Create(ctx, "u1", core.SessionMeta{State: models.SessionPending, Factor: core.FactorPassword})
	require.NoError(t, err)
	got, err := sm.Get(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, []string{core.FactorPassword}, got.Factors, "the factors column round-trips")
	promoted, promotedRaw, err := sm.Promote(ctx, sess.ID, core.FactorTOTP)
	require.NoError(t, err)
	assert.Equal(t, models.SessionActive, promoted.State)
	assert.Equal(t, []string{core.FactorPassword, core.FactorTOTP}, promoted.Factors, "the second factor is appended")
	assert.EqualValues(t, 1, promoted.Version, "the update from version 0 wrote 1, and the version column round-trips")
	_, err = sm.Get(ctx, raw)
	assert.True(t, errs.IsNotFound(err), "the pending token stops working: %v", err)
	_, err = st.UpdateSessionAt(ctx, sess.ID, sess.Version, behemoth.M{models.SessionRevokedReason: "unused"})
	assert.True(t, errs.IsNotFound(err), "the version is checked in the write: %v", err)
	require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
	_, err = sm.Get(ctx, promotedRaw)
	assert.True(t, errs.IsCode(err, errs.CodeSessionRevoked), "%v", err)
	listed, err := sm.ListForUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.NotNil(t, listed[0].RevokedAt)

	// The session limit compares expires_at with the store's clock in the
	// database. An hour in the past keeps the case clear of the backends that
	// store whole seconds.
	limited := auth.NewSessionManager(st, nil, testCrypto(t),
		core.SessionConfig{ExpiresIn: time.Hour, MaxConcurrent: 1}, &passDispatcher{}, nil, nil)
	expired, _, err := limited.Create(ctx, "u2", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	setSessionExpiry(t, st, expired.ID, st.Now().Add(-time.Hour))
	_, _, err = limited.Create(ctx, "u2", core.SessionMeta{State: models.SessionPending})
	require.NoError(t, err)
	_, _, err = limited.Create(ctx, "u2", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err, "neither the expired nor the pending session counts")
	_, _, err = limited.Create(ctx, "u2", core.SessionMeta{State: models.SessionActive})
	assert.True(t, errs.IsCode(err, errs.CodeSessionLimitReached), "the active session counts: %v", err)

	// The purge removes the sessions that expired before its cutoff, revoked
	// or not, and keeps the rest. The times are an hour or more apart, for
	// the backends that store whole seconds.
	now := st.Now()
	session := func(expires time.Time, revoked bool) string {
		t.Helper()
		sess, _, err := sm.Create(ctx, "u-purge", core.SessionMeta{State: models.SessionActive})
		require.NoError(t, err)
		if revoked {
			require.NoError(t, sm.Revoke(ctx, sess.ID, "user_logout"))
		}
		setSessionExpiry(t, st, sess.ID, expires)
		return sess.ID
	}
	kept := func(id string) bool {
		t.Helper()
		_, err := st.FindSessionByID(ctx, id)
		if errs.IsNotFound(err) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	old := session(now.Add(-2*time.Hour), false)
	oldRevoked := session(now.Add(-2*time.Hour), true)
	recent := session(now.Add(-30*time.Minute), false)
	revoked := session(now.Add(time.Hour), true)
	live := session(now.Add(time.Hour), false)
	require.NoError(t, st.PurgeExpiredSessions(ctx, now.Add(-time.Hour)))
	assert.False(t, kept(old), "expired before the cutoff")
	assert.False(t, kept(oldRevoked), "revoked, and expired before the cutoff")
	assert.True(t, kept(recent), "expired after the cutoff")
	assert.True(t, kept(revoked), "revoked, and not expired")
	assert.True(t, kept(live))

	// A cutoff later than the store's time is taken as the store's time.
	require.NoError(t, st.PurgeExpiredSessions(ctx, now.Add(24*time.Hour)))
	assert.False(t, kept(recent), "expired before now")
	assert.True(t, kept(revoked), "a revoked session waits for its expiry")
	assert.True(t, kept(live), "a live session is never purged")
	require.NoError(t, st.PurgeExpiredSessions(ctx, now.Add(-time.Hour)), "a second pass finds nothing to delete")
}

// setSessionExpiry moves the expiry of the session with id, from the version
// the session is at.
func setSessionExpiry(t *testing.T, st *store.Store, id string, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.FindSessionByID(ctx, id)
	require.NoError(t, err)
	_, err = st.UpdateSessionAt(ctx, id, sess.Version, behemoth.M{models.SessionExpiresAt: expires})
	require.NoError(t, err)
}

func tokensContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	catalog := auth.NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindReset, SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "core"}))
	require.NoError(t, catalog.Declare(core.TokenKindDef{Kind: kindAPI, Backend: core.TokenBackendDB, Owner: "core"}))
	tm := auth.NewDefaultTokenManager(st, nil, catalog, testCrypto(t), &passDispatcher{}, core.TokenConfig{}, nil)

	_, raw, err := tm.Issue(ctx, kindReset, "u1", behemoth.M{"redirect": "/reset"})
	require.NoError(t, err)
	verified, err := tm.Verify(ctx, kindReset, raw)
	require.NoError(t, err)
	assert.Equal(t, "/reset", verified.MetadataJSON["redirect"], "metadata round-trips")

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 20 {
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

	// ListForSubject returns what a subject can still use: the filters on
	// the consumed and revoked columns have to hold on every backend.
	keep, _, err := tm.Issue(ctx, kindReset, "u-list", behemoth.M{"n": 1})
	require.NoError(t, err)
	used, usedRaw, err := tm.Issue(ctx, kindReset, "u-list", nil)
	require.NoError(t, err)
	gone, _, err := tm.Issue(ctx, kindReset, "u-list", nil)
	require.NoError(t, err)
	_, _, err = tm.Issue(ctx, kindReset, "u-other", nil)
	require.NoError(t, err)
	_, err = tm.Consume(ctx, kindReset, usedRaw)
	require.NoError(t, err)
	require.NoError(t, tm.Revoke(ctx, gone.ID))
	listed, err := tm.ListForSubject(ctx, kindReset, "u-list")
	require.NoError(t, err)
	require.Len(t, listed, 1, "the consumed and the revoked token are left out, and so is another subject's")
	assert.Equal(t, keep.ID, listed[0].ID)
	assert.EqualValues(t, 1, listed[0].MetadataJSON["n"], "with its metadata")

	// The store's other listing keeps the consumed token, which can still
	// be revoked.
	unrevoked, err := st.ListUnrevokedTokensForSubject(ctx, kindReset, "u-list")
	require.NoError(t, err)
	var ids []string
	for _, tok := range unrevoked {
		ids = append(ids, tok.ID)
	}
	assert.ElementsMatch(t, []string{keep.ID, used.ID}, ids, "the revoked token and another subject's are left out")
	require.NoError(t, tm.Revoke(ctx, used.ID))
	unrevoked, err = st.ListUnrevokedTokensForSubject(ctx, kindReset, "u-list")
	require.NoError(t, err)
	require.Len(t, unrevoked, 1, "the consumed token was revoked")
	assert.Equal(t, keep.ID, unrevoked[0].ID)

	// A kind with no expiry stores none, and verifies.
	apiKey, apiRaw, err := tm.Issue(ctx, kindAPI, "u-purge", nil)
	require.NoError(t, err)
	found, err := st.FindTokenByID(ctx, apiKey.ID)
	require.NoError(t, err)
	assert.True(t, found.ExpiresAt.IsZero(), "expires_at %v", found.ExpiresAt)
	_, err = tm.Verify(ctx, kindAPI, apiRaw)
	require.NoError(t, err)

	// The purge removes the tokens that expired before its cutoff, and the
	// ones with no expiry that were consumed or revoked before it. A token
	// with no expiry that is still usable is kept.
	now := st.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	n := 0
	token := func(kind models.TokenKind, subject any, expires time.Time, consumed, revoked *time.Time) string {
		t.Helper()
		n++
		tok := &models.Token{Kind: kind, Subject: subject, LookupHash: fmt.Sprintf("purge-%d", n), TokenHash: "h", KeyVersion: 1,
			ExpiresAt: expires, ConsumedAt: consumed, RevokedAt: revoked}
		require.NoError(t, st.CreateToken(ctx, tok))
		return tok.ID
	}
	kept := func(id string) bool {
		t.Helper()
		_, err := st.FindTokenByID(ctx, id)
		if errs.IsNotFound(err) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	expired := token(kindReset, "u-purge", now.Add(-2*time.Hour), nil, nil)
	expiredUsed := token(kindReset, "u-purge", now.Add(-2*time.Hour), ago(3*time.Hour), nil)
	noSubject := token(kindReset, nil, now.Add(-2*time.Hour), nil, nil)
	recent := token(kindReset, "u-purge", now.Add(-30*time.Minute), nil, nil)
	revokedLive := token(kindReset, "u-purge", now.Add(time.Hour), nil, ago(2*time.Hour))
	keyRevoked := token(kindAPI, "u-purge", time.Time{}, nil, ago(2*time.Hour))
	keyUsed := token(kindAPI, "u-purge", time.Time{}, ago(2*time.Hour), nil)
	keyRevokedRecently := token(kindAPI, "u-purge", time.Time{}, nil, ago(30*time.Minute))
	require.NoError(t, st.PurgeExpiredTokens(ctx, now.Add(-time.Hour)))
	assert.False(t, kept(expired), "expired before the cutoff")
	assert.False(t, kept(expiredUsed), "consumed, and expired before the cutoff")
	assert.False(t, kept(noSubject), "a token with no subject")
	assert.False(t, kept(keyRevoked), "no expiry, revoked before the cutoff")
	assert.False(t, kept(keyUsed), "no expiry, consumed before the cutoff")
	assert.True(t, kept(recent), "expired after the cutoff")
	assert.True(t, kept(revokedLive), "revoked before the cutoff, and not expired")
	assert.True(t, kept(keyRevokedRecently), "no expiry, revoked after the cutoff")
	assert.True(t, kept(apiKey.ID), "no expiry, and usable")

	// A cutoff later than the store's time is taken as the store's time.
	require.NoError(t, st.PurgeExpiredTokens(ctx, now.Add(24*time.Hour)))
	assert.False(t, kept(recent), "expired before now")
	assert.False(t, kept(keyRevokedRecently), "revoked before now")
	assert.True(t, kept(revokedLive), "a revoked token waits for its expiry")
	_, err = tm.Verify(ctx, kindAPI, apiRaw)
	assert.NoError(t, err, "a usable token is never purged")
}

// purgeContract covers Purge: a table with a cutoff is purged up to it and
// left alone without one, and the tables that take no cutoff are purged on
// every run.
func purgeContract(t *testing.T, st *store.Store, db behemoth.Database, resolver behemoth.SchemaResolver) {
	ctx := context.Background()
	now := st.Now()
	twoHoursAgo := now.Add(-2 * time.Hour)

	// One row in each table, past every cutoff used below.
	sm := auth.NewSessionManager(st, nil, testCrypto(t), core.SessionConfig{ExpiresIn: time.Hour}, &passDispatcher{}, nil, nil)
	sess, _, err := sm.Create(ctx, "u-purge-all", core.SessionMeta{State: models.SessionActive})
	require.NoError(t, err)
	setSessionExpiry(t, st, sess.ID, twoHoursAgo)
	tok := &models.Token{Kind: kindReset, Subject: "u-purge-all", LookupHash: "purge-all", TokenHash: "h", KeyVersion: 1, ExpiresAt: twoHoursAgo}
	require.NoError(t, st.CreateToken(ctx, tok))
	require.NoError(t, st.RecordAuditEvent(ctx, telemetry.AuditEvent{
		Type: "contract.purge", Outcome: telemetry.OutcomeSuccess, ActorType: telemetry.ActorUser, ActorID: "u-purge-all",
		RequestID: "req-purge-all", Timestamp: twoHoursAgo,
	}))
	past := store.New(db, store.WithSchema(resolver), store.WithClock(func() time.Time { return twoHoursAgo }))
	_, _, err = past.IncrementRateLimit(ctx, "purge:all", time.Minute)
	require.NoError(t, err)
	const codeKind models.CodeKind = "contract.purge"
	require.NoError(t, st.ReplaceVerificationCode(ctx, &models.VerificationCode{Kind: codeKind, Subject: "u-purge-all", CodeHash: "h", AttemptsLeft: 1, ExpiresAt: twoHoursAgo}))
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "purge-all@example.com", "u-purge-all", twoHoursAgo))

	sessionKept := func() bool {
		_, err := st.FindSessionByID(ctx, sess.ID)
		return err == nil
	}
	tokenKept := func() bool {
		_, err := st.FindTokenByID(ctx, tok.ID)
		return err == nil
	}
	auditKept := func() bool {
		page, err := st.QueryAuditEvents(ctx, telemetry.AuditFilter{RequestID: "req-purge-all"})
		require.NoError(t, err)
		return len(page.Events) == 1
	}

	// Without cutoffs, only the tables that take none are purged.
	require.NoError(t, st.Purge(ctx, store.PurgeOptions{}))
	assert.True(t, sessionKept(), "no session cutoff")
	assert.True(t, tokenKept(), "no token cutoff")
	assert.True(t, auditKept(), "no audit cutoff")
	counters, err := db.Count(ctx, &models.RateLimit{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.RateLimitKey, Operator: clause.OpEqual, Value: "purge:all"},
	}})
	require.NoError(t, err)
	assert.Zero(t, counters, "the expired counter was purged")
	_, err = st.FindVerificationCode(ctx, codeKind, "u-purge-all")
	assert.True(t, errs.IsNotFound(err), "the expired code was purged: %v", err)
	_, err = st.FindReservedValue(ctx, models.UserEmail, "purge-all@example.com")
	assert.True(t, errs.IsNotFound(err), "the expired reservation was purged: %v", err)

	// With cutoffs, the rest go too.
	cutoff := now.Add(-time.Hour)
	require.NoError(t, st.Purge(ctx, store.PurgeOptions{SessionsBefore: cutoff, TokensBefore: cutoff, AuditEventsBefore: cutoff}))
	assert.False(t, sessionKept(), "the session was purged")
	assert.False(t, tokenKept(), "the token was purged")
	assert.False(t, auditKept(), "the audit event was purged")
}

func accountsContract(t *testing.T, st *store.Store, db behemoth.Database) {
	ctx := context.Background()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	google := &models.Account{UserID: "u1", ProviderID: "google", AccountID: "g-123",
		AccessToken: "access-1", RefreshToken: "refresh-1", AccessTokenExpiresAt: &expires, Scope: "openid email"}
	require.NoError(t, st.CreateAccount(ctx, google))
	assert.Equal(t, "access-1", google.AccessToken, "the caller's account keeps the plaintext")

	raw, err := db.FindOne(ctx, &models.Account{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.AccountID, Operator: clause.OpEqual, Value: google.ID}}})
	require.NoError(t, err)
	stored := raw.(*models.Account)
	assert.NotContains(t, stored.AccessToken, "access-1", "tokens are encrypted at rest")
	version, err := store.SealedKeyVersion(stored.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, 1, version, "the key version travels with the ciphertext")
	assert.Empty(t, stored.IDToken, "an absent token is NULL, not a sealed empty string")

	found, err := st.FindAccount(ctx, "google", "g-123")
	require.NoError(t, err)
	assert.Equal(t, google.ID, found.ID)
	assert.Equal(t, "access-1", found.AccessToken)
	assert.Equal(t, "refresh-1", found.RefreshToken)
	assert.Equal(t, "openid email", found.Scope)
	require.NotNil(t, found.AccessTokenExpiresAt)
	assert.True(t, found.AccessTokenExpiresAt.Equal(expires), "expiry round-trips: %v", found.AccessTokenExpiresAt)
	assert.Nil(t, found.RefreshTokenExpiresAt)

	err = st.CreateAccount(ctx, &models.Account{UserID: "u2", ProviderID: "google", AccountID: "g-123"})
	assert.True(t, errs.IsDuplicateKey(err), "one account per (provider, account id): %v", err)

	updated, err := st.UpdateAccount(ctx, google.ID, behemoth.M{models.AccountAccessToken: "access-2", models.AccountIDToken: ""})
	require.NoError(t, err)
	assert.Equal(t, "access-2", updated.AccessToken)
	assert.Equal(t, "refresh-1", updated.RefreshToken, "a token that wasn't updated is still readable")
	cleared, err := st.UpdateAccount(ctx, google.ID, behemoth.M{models.AccountRefreshToken: nil})
	require.NoError(t, err)
	assert.Empty(t, cleared.RefreshToken)

	credential := &models.Account{UserID: "u1", ProviderID: models.ProviderCredential, AccountID: "u1", PasswordHash: "hash"}
	require.NoError(t, st.CreateAccount(ctx, credential))
	listed, err := st.ListAccountsForUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, "access-2", listed[0].AccessToken, "oldest first, tokens opened")
	assert.Equal(t, "hash", listed[1].PasswordHash)

	noCrypto := store.New(db)
	err = noCrypto.CreateAccount(ctx, &models.Account{UserID: "u3", ProviderID: "github", AccountID: "gh-1", AccessToken: "t"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "no encryptor, no plaintext token on disk: %v", err)
	_, err = st.FindAccount(ctx, "github", "gh-1")
	assert.True(t, errs.IsNotFound(err), "%v", err)

	// A token is bound to its account and its column: copied into another
	// account's row, or into another column of its own, it does not open.
	other := &models.Account{UserID: "u4", ProviderID: "github", AccountID: "gh-4", AccessToken: "access-4"}
	require.NoError(t, st.CreateAccount(ctx, other))
	raw, err = db.FindOne(ctx, &models.Account{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.AccountID, Operator: clause.OpEqual, Value: google.ID}}})
	require.NoError(t, err)
	googleAccess := raw.(*models.Account).AccessToken
	byID := func(id string) clause.Expression {
		return clause.Expression{Conditions: []clause.Condition{{Field: models.AccountID, Operator: clause.OpEqual, Value: id}}}
	}
	require.NoError(t, db.UpdateOne(ctx, &models.Account{}, byID(other.ID), behemoth.M{models.AccountAccessToken: googleAccess}))
	_, err = st.FindAccountByID(ctx, other.ID)
	assert.True(t, errs.IsCode(err, "invalid_payload"), "another account's token: %v", err)
	require.NoError(t, db.UpdateOne(ctx, &models.Account{}, byID(other.ID), behemoth.M{models.AccountAccessToken: nil, models.AccountRefreshToken: googleAccess}))
	_, err = st.FindAccountByID(ctx, other.ID)
	assert.True(t, errs.IsCode(err, "invalid_payload"), "an access token as a refresh token: %v", err)

	// A stored value that isn't a valid ciphertext is an error, not a panic.
	require.NoError(t, db.UpdateOne(ctx, &models.Account{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.AccountID, Operator: clause.OpEqual, Value: google.ID}}}, behemoth.M{models.AccountAccessToken: "v1:AAAA"}))
	_, err = st.FindAccountByID(ctx, google.ID)
	assert.True(t, errs.IsCode(err, "short_cipher_length"), "truncated ciphertext: %v", err)

	require.NoError(t, st.DeleteAccount(ctx, google.ID))
	_, err = st.FindAccountByID(ctx, google.ID)
	assert.True(t, errs.IsNotFound(err), "%v", err)
}

func rateLimitsContract(t *testing.T, db behemoth.Database, resolver behemoth.SchemaResolver) {
	ctx := context.Background()
	var clock atomic.Int64 // seconds past t0
	st := store.New(db, store.WithSchema(resolver), store.WithClock(func() time.Time {
		return t0.Add(time.Duration(clock.Load()) * time.Second)
	}))

	const calls = 20
	counts := make(chan int64, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			n, resetAt, err := st.IncrementRateLimit(ctx, "signin:1.2.3.4", time.Minute)
			assert.NoError(t, err)
			assert.True(t, resetAt.Equal(t0.Add(time.Minute)), "every attempt in a window gets the window's end: %v", resetAt)
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	seen := map[int64]bool{}
	for n := range counts {
		seen[n] = true
	}
	assert.Len(t, seen, calls, "concurrent attempts each get their own count")
	assert.True(t, seen[1] && seen[calls], "counts run 1..%d: %v", calls, seen)

	other, _, err := st.IncrementRateLimit(ctx, "signin:5.6.7.8", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 1, other, "keys count independently")

	clock.Store(59)
	n, resetAt, err := st.IncrementRateLimit(ctx, "signin:1.2.3.4", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, calls+1, n, "still inside the window")
	assert.True(t, resetAt.Equal(t0.Add(time.Minute)), "the window's end does not move with later attempts: %v", resetAt)

	clock.Store(61)
	n, resetAt, err = st.IncrementRateLimit(ctx, "signin:1.2.3.4", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "a passed window starts again")
	assert.True(t, resetAt.Equal(t0.Add(121*time.Second)), "and ends a window after the attempt that opened it: %v", resetAt)

	clock.Store(100) // 1.2.3.4's new window runs to 121; 5.6.7.8's ended at 60
	require.NoError(t, st.PurgeExpiredRateLimits(ctx))
	left, err := db.Count(ctx, &models.RateLimit{}, clause.Expression{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, left, "only passed windows are purged")
}

// auditLogContract records events and reads them back: every field
// round-trips, filters select, pages are newest first and don't overlap, and
// a purge removes what is older than its cutoff.
func auditLogContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// An impersonated actor, so that the delegate columns round-trip too.
	full := telemetry.AuditEvent{
		Type: "auth.signIn.failed", Outcome: telemetry.OutcomeFailure,
		ActorType: telemetry.ActorUser, ActorID: "user-9",
		DelegateType: telemetry.ActorUser, DelegateID: "admin-1",
		SubjectType: models.UserTable, SubjectID: "user-1",
		SessionID: "sess-1", RequestID: "req-1", IPAddress: "203.0.113.7", UserAgent: "curl/8",
		Metadata:  behemoth.M{"code": "invalid_credentials", "email": "ada@example.com"},
		Timestamp: base,
	}
	require.NoError(t, st.RecordAuditEvent(ctx, full))
	// Five sign-ins by user-1, one minute apart, then one by user-2.
	for i := 1; i <= 5; i++ {
		require.NoError(t, st.RecordAuditEvent(ctx, telemetry.AuditEvent{
			Type: "auth.signIn.after", Outcome: telemetry.OutcomeSuccess, ActorType: telemetry.ActorUser, ActorID: "user-1",
			SubjectType: models.UserTable, SubjectID: "user-1", Timestamp: base.Add(time.Duration(i) * time.Minute),
		}))
	}
	require.NoError(t, st.RecordAuditEvent(ctx, telemetry.AuditEvent{
		Type: "auth.signIn.after", Outcome: telemetry.OutcomeSuccess, ActorType: telemetry.ActorUser, ActorID: "user-2",
		SubjectType: models.UserTable, SubjectID: "user-2", Timestamp: base.Add(10 * time.Minute),
	}))

	// Every field of the first event round-trips. Optional fields that were
	// empty read back empty.
	page, err := st.QueryAuditEvents(ctx, telemetry.AuditFilter{RequestID: "req-1"})
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	got := page.Events[0]
	assert.NotEmpty(t, got.ID)
	assert.True(t, got.Timestamp.Equal(base), "timestamp: %v", got.Timestamp)
	got.ID, got.Timestamp, full.Timestamp = "", time.Time{}, time.Time{}
	assert.Equal(t, full, got)

	all, err := st.QueryAuditEvents(ctx, telemetry.AuditFilter{})
	require.NoError(t, err)
	require.Len(t, all.Events, 7)
	assert.Empty(t, all.NextCursor)
	assert.Equal(t, "user-2", all.Events[0].ActorID, "newest first")
	assert.Equal(t, "auth.signIn.failed", all.Events[6].Type, "oldest last")
	assert.Empty(t, all.Events[0].RequestID)
	assert.Nil(t, all.Events[0].Metadata)

	count := func(f telemetry.AuditFilter) int {
		p, err := st.QueryAuditEvents(ctx, f)
		require.NoError(t, err)
		return len(p.Events)
	}
	assert.Equal(t, 5, count(telemetry.AuditFilter{ActorID: "user-1"}))
	assert.Equal(t, 6, count(telemetry.AuditFilter{SubjectType: models.UserTable, SubjectID: "user-1"}))
	assert.Equal(t, 1, count(telemetry.AuditFilter{Outcome: telemetry.OutcomeFailure}))
	assert.Equal(t, 6, count(telemetry.AuditFilter{Types: []string{"auth.signIn.after"}}))
	assert.Equal(t, 7, count(telemetry.AuditFilter{Types: []string{"auth.signIn.after", "auth.signIn.failed"}}))
	assert.Equal(t, 0, count(telemetry.AuditFilter{Types: []string{"auth.signOut.after"}}))
	assert.Equal(t, 1, count(telemetry.AuditFilter{SessionID: "sess-1"}))
	// From is inclusive, To exclusive: minutes 2, 3 and 4.
	assert.Equal(t, 3, count(telemetry.AuditFilter{From: base.Add(2 * time.Minute), To: base.Add(5 * time.Minute)}))

	// Paging: 7 events in pages of 3 are 3 + 3 + 1, without overlap, and in
	// the same order as the unpaged query.
	var paged []string
	filter := telemetry.AuditFilter{Limit: 3}
	for pages := 0; ; pages++ {
		require.Less(t, pages, 4, "paging does not end")
		p, err := st.QueryAuditEvents(ctx, filter)
		require.NoError(t, err)
		for _, e := range p.Events {
			paged = append(paged, e.ID)
		}
		if p.NextCursor == "" {
			break
		}
		require.Len(t, p.Events, 3)
		filter.Cursor = p.NextCursor
	}
	var unpaged []string
	for _, e := range all.Events {
		unpaged = append(unpaged, e.ID)
	}
	assert.Equal(t, unpaged, paged)

	// Purge removes what was recorded before the cutoff and keeps the rest.
	require.NoError(t, st.PurgeAuditEvents(ctx, base.Add(3*time.Minute)))
	assert.Equal(t, 4, count(telemetry.AuditFilter{}), "minutes 3, 4, 5 and 10 remain")
}

// reservedValuesContract covers the reservations of user values: one row per
// column and value, a reservation its user extends and never shortens and
// another user replaces once it has expired, the conflict a reserved value
// is refused with, and a user's reservations going with the user.
func reservedValuesContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	soon := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	later := soon.Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	expiresAt := func(value string) time.Time {
		t.Helper()
		found, err := st.FindReservedValue(ctx, models.UserEmail, value)
		require.NoError(t, err)
		return found.ExpiresAt
	}
	owner := &models.User{Email: "reserving@example.com"}
	require.NoError(t, st.CreateUser(ctx, owner))

	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "old@example.com", owner.ID, soon))
	found, err := st.FindReservedValue(ctx, models.UserEmail, "old@example.com")
	require.NoError(t, err)
	assert.Equal(t, owner.ID, found.UserID)
	assert.True(t, found.ExpiresAt.Equal(soon), "expires_at %v, want %v", found.ExpiresAt, soon)
	_, err = st.FindReservedValue(ctx, "username", "old@example.com")
	assert.True(t, errs.IsNotFound(err), "another column: %v", err)

	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "old@example.com", owner.ID, later))
	assert.True(t, expiresAt("old@example.com").Equal(later), "the reservation is extended")
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "old@example.com", owner.ID, soon))
	assert.True(t, expiresAt("old@example.com").Equal(later), "and not shortened")

	// Another user is refused the value with the email's conflict, which
	// holds a duplicate key on the email; its owner is not.
	err = st.CheckReservedValue(ctx, models.UserEmail, "old@example.com", "someone-else")
	de, ok := errors.AsType[*errs.DomainError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, errs.CategoryConflict, de.Category)
	assert.Equal(t, "email_taken", de.Code)
	assert.True(t, errs.IsDuplicateKey(err))
	assert.Equal(t, []string{models.UserEmail}, errs.DuplicateKeyColumns(err))
	assert.NoError(t, st.CheckReservedValue(ctx, models.UserEmail, "old@example.com", owner.ID), "the owner")
	assert.NoError(t, st.CheckReservedValue(ctx, models.UserEmail, "free@example.com", ""), "a value that is not reserved")

	require.NoError(t, st.ReleaseReservedValue(ctx, models.UserEmail, "old@example.com", "someone-else"))
	_, err = st.FindReservedValue(ctx, models.UserEmail, "old@example.com")
	require.NoError(t, err, "another user's release leaves the reservation")
	require.NoError(t, st.ReleaseReservedValue(ctx, models.UserEmail, "old@example.com", owner.ID))
	_, err = st.FindReservedValue(ctx, models.UserEmail, "old@example.com")
	assert.True(t, errs.IsNotFound(err), "the owner's release removes it: %v", err)

	// An expired reservation refuses nothing, another user's reservation
	// replaces it, and the purge removes the expired ones only.
	other := &models.User{Email: "other-reserver@example.com"}
	require.NoError(t, st.CreateUser(ctx, other))
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "expired@example.com", owner.ID, past))
	assert.NoError(t, st.CheckReservedValue(ctx, models.UserEmail, "expired@example.com", "someone-else"))
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "expired@example.com", other.ID, soon))
	found, err = st.FindReservedValue(ctx, models.UserEmail, "expired@example.com")
	require.NoError(t, err)
	assert.Equal(t, other.ID, found.UserID)
	assert.True(t, found.ExpiresAt.Equal(soon))
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "stale@example.com", owner.ID, past))
	require.NoError(t, st.PurgeExpiredReservedValues(ctx))
	_, err = st.FindReservedValue(ctx, models.UserEmail, "stale@example.com")
	assert.True(t, errs.IsNotFound(err), "the expired reservation was purged: %v", err)
	_, err = st.FindReservedValue(ctx, models.UserEmail, "expired@example.com")
	require.NoError(t, err, "the live one was kept")

	// A user's reservations go with the user.
	require.NoError(t, st.ReserveValue(ctx, models.UserEmail, "owned@example.com", owner.ID, soon))
	require.NoError(t, st.DeleteUser(ctx, owner.ID))
	_, err = st.FindReservedValue(ctx, models.UserEmail, "owned@example.com")
	assert.True(t, errs.IsNotFound(err), "the user's reservation went with the user: %v", err)
	require.NoError(t, st.DeleteUser(ctx, other.ID))
}

// verificationCodesContract covers the store's one-time code operations: one
// row per kind and subject, attempts that are taken one at a time, a delete
// that happens once, and a user's codes going with the user.
func verificationCodesContract(t *testing.T, st *store.Store) {
	ctx := context.Background()
	const kind models.CodeKind = "contract.kind"
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	first := &models.VerificationCode{Kind: kind, Subject: "subject", CodeHash: "hash-1", KeyVersion: 1, AttemptsLeft: 2,
		Metadata: behemoth.M{"phone_number": "+14155550101"}, ExpiresAt: expires}
	require.NoError(t, st.ReplaceVerificationCode(ctx, first))
	require.NotEmpty(t, first.ID)

	found, err := st.FindVerificationCode(ctx, kind, "subject")
	require.NoError(t, err)
	assert.Equal(t, "hash-1", found.CodeHash)
	assert.Equal(t, 1, found.KeyVersion)
	assert.Equal(t, 2, found.AttemptsLeft)
	assert.Equal(t, "+14155550101", found.Metadata["phone_number"], "the metadata round-trips")
	assert.True(t, found.ExpiresAt.Equal(expires), "expires_at %v, want %v", found.ExpiresAt, expires)
	_, err = st.FindVerificationCode(ctx, "contract.other", "subject")
	assert.True(t, errs.IsNotFound(err), "another kind: %v", err)

	// A second code for the same kind and subject replaces the first.
	second := &models.VerificationCode{Kind: kind, Subject: "subject", CodeHash: "hash-2", KeyVersion: 1, AttemptsLeft: 2, ExpiresAt: expires}
	require.NoError(t, st.ReplaceVerificationCode(ctx, second))
	found, err = st.FindVerificationCode(ctx, kind, "subject")
	require.NoError(t, err)
	assert.Equal(t, "hash-2", found.CodeHash)
	assert.Nil(t, found.Metadata, "no metadata is stored as none")
	assert.True(t, errs.IsNotFound(st.DeleteVerificationCode(ctx, first.ID)), "the first code is gone")

	// Two attempts, then none.
	for want := 2; want > 0; want-- {
		spent, err := st.SpendVerificationCodeAttempt(ctx, kind, "subject")
		require.NoError(t, err)
		assert.Equal(t, want, spent.AttemptsLeft, "the code as it was before the attempt")
	}
	_, err = st.SpendVerificationCodeAttempt(ctx, kind, "subject")
	assert.True(t, errs.IsNotFound(err), "no attempts left: %v", err)
	_, err = st.SpendVerificationCodeAttempt(ctx, kind, "nobody")
	assert.True(t, errs.IsNotFound(err), "no code: %v", err)

	require.NoError(t, st.DeleteVerificationCode(ctx, second.ID))
	assert.True(t, errs.IsNotFound(st.DeleteVerificationCode(ctx, second.ID)), "a code is deleted once")

	// An expired code is purged, a live one is kept, and a user's code goes
	// with the user.
	u := &models.User{Email: "codes@example.com"}
	require.NoError(t, st.CreateUser(ctx, u))
	require.NoError(t, st.ReplaceVerificationCode(ctx, &models.VerificationCode{Kind: kind, Subject: "expired", CodeHash: "h", AttemptsLeft: 1, ExpiresAt: time.Now().UTC().Add(-time.Hour)}))
	require.NoError(t, st.ReplaceVerificationCode(ctx, &models.VerificationCode{Kind: kind, Subject: u.ID, CodeHash: "h", AttemptsLeft: 1, ExpiresAt: expires}))
	require.NoError(t, st.PurgeExpiredVerificationCodes(ctx))
	_, err = st.FindVerificationCode(ctx, kind, "expired")
	assert.True(t, errs.IsNotFound(err), "the expired code was purged: %v", err)
	_, err = st.FindVerificationCode(ctx, kind, u.ID)
	require.NoError(t, err, "the live code was kept")
	require.NoError(t, st.DeleteUser(ctx, u.ID))
	_, err = st.FindVerificationCode(ctx, kind, u.ID)
	assert.True(t, errs.IsNotFound(err), "the user's code went with the user: %v", err)

	require.NoError(t, st.DeleteVerificationCodeFor(ctx, kind, "nobody"), "removing a code that is not there is not an error")
}
