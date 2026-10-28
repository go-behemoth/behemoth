# Core models

This document explains the tables behemoth's core owns: which models exist and where they are defined, how a model, its column constants and its table declaration stay in step, how an `Account` stores credentials and encrypts OAuth tokens, what an existing database needs before `users.password_hash` is dropped, and how data hooks run inside the transaction of the write that fires them.

The code lives in:

- `models/`: one file per model, plus `schema.go` (table declarations) and `extension.go` (contributed columns)
- `store/`: the typed operations on those models; `seal.go` holds the at-rest encryption
- `engine/init.go`: `CoreDeclareSchema`, which declares the tables, and `Boot`, which builds the store
- `crypto/contracts.go`: the crypto contracts the store uses (`Encryptor`)

The rate limiter that writes `rate_limits` is described in [`../ratelimit/rate_limiter.md`](../ratelimit/rate_limiter.md).

---

## What exists

| Table | Model | Definition | Declared by core | Store operations |
|---|---|---|---|---|
| `users` | `models.User` | `models/user.go` | yes | `store/users.go` |
| `accounts` | `models.Account` | `models/account.go` | yes | `store/accounts.go` |
| `sessions` | `models.Session` | `models/session.go` | yes | `store/sessions.go` |
| `tokens` | `models.Token` | `models/token.go` | yes | `store/tokens.go` |
| `rate_limits` | `models.RateLimit` | `models/rate_limit.go` | yes | `store/rate_limits.go` |
| `reserved_values` | `models.ReservedValue` | `models/reserved_value.go` | yes | `store/reserved_values.go` |

What each one is for:

- **`users`**: who signs in: id, unique email, profile fields, `email_verified`. It holds no credential.
- **`accounts`**: how a user signs in. One row per `(provider_id, account_id)`: the `"credential"` provider carries the password hash, an OAuth provider carries its tokens.
- **`sessions`**: one row per issued session, looked up by the hash of its token. `factors` holds what its sign-in proved, separated by spaces, and `Promote` appends the second factor ([ADR-0009](../decisions/0009-recording-the-factors-of-a-session.md)). `version` counts the row's updates: `Store.UpdateSessionAt`, the store's only update of a session, guards each one on the version it read and writes the next, so that the session cache keeps the newest copy of a session ([ADR-0011](../decisions/0011-ordering-session-cache-writes-by-row-version.md)). The session manager takes every session time from the store's clock (`Store.Now`), in UTC. The session limit compares `expires_at` in the database, and SQLite stores a time as text with its zone, so expiries stamped in local time would compare wrongly there. `Store.PurgeExpiredSessions` compares `expires_at` in the database too, through `idx_sessions_expires_at`.
- **`tokens`**: the verification table. It is generic by `Kind` (email verification, password reset, magic link, API key), so there is no separate `verifications` table. The token manager takes its times from `Store.Now`, for the reason given for `sessions`. A kind with no TTL is stored with a NULL `expires_at`: a zero time would match every "expired before" comparison, so the purge would remove a live API key, and MySQL refuses the zero date its driver sends for it. `Store.PurgeExpiredTokens` removes the rows that expired before its cutoff through `idx_tokens_expires_at`, and the rows with no expiry once they are consumed or revoked.
- **`rate_limits`**: fixed-window counters, used only when rate limiting has no key-value store to count in.
- **`reserved_values`**: a value of a unique `users` column kept for the user who gave it up, until an expiry. One row per `(column_name, column_value)`. The email change flow keeps the old address there while its revert link works ([`../plugins/emailverification.md`](../plugins/emailverification.md)). The store does not check it on every write: the plugin that reserves a column's values refuses them from its data handlers with `Store.CheckReservedValue`, which answers the column's taken conflict. The table is generic over the column so that the username plugin can reserve released usernames the same way (`docs/ongoing.md`, L8).

`sessions`, `accounts`, and `reserved_values` each declare a cascading foreign key to `users`, so deleting a user removes them.

---

## How a model is put together

Every model in `models/` follows the same four-part shape. `models/session.go` is the reference.

The four parts are:

1. **Column constants.** The table name and every column name are constants (`SessionTable`, `SessionUserID`, ...). They are canonical names: the `SchemaResolver` maps them to physical ones.
2. **The struct, with `ToMap` and `FromMap`.** `ToMap` is the row that gets written; `FromMap` reads a row back. Both use the constants.
3. **The own-column set.** `sessionColumns`, `accountColumns`, and so on list the columns the model itself has.
4. **The table declaration.** `models/schema.go` has one function per table (`SessionTableSchema()`), built from the same constants, with indexes and foreign keys.

`[Convention, important]` These three must list exactly the same columns: the table declaration, the own-column set, and the keys `ToMap` returns. `TestDeclaredTablesMatchModels` in `models/schema_test.go` fails when they drift. A new model is added to that test's table.

### Contributed columns

A plugin or the application may add a column to a core table (`Registry.ExtendColumn`). A model that embeds `models.Extension` carries those values in `Extra`: `FromMap` puts every column that is not in the own-column set there, and `ToMap` merges them back. Typed access goes through `schema.Field`.

`User`, `Account`, `Session`, and `Token` embed `Extension`. `RateLimit` does not: it is an internal counter, and a column contributed to it would be ignored.

A column declared `Unique`, core's or contributed, gets a conflict code: `<column>_taken`, or its `ConflictCode`. When a write through the store gives it a value another row holds, the store answers `409` with that code instead of the duplicate key error, whichever flow wrote the row. `users.email` is `email_taken`, the username plugin's column `username_taken`. [`../database/adapters/unique_keys.md`](../database/adapters/unique_keys.md) explains how the store learns which column was taken.

### What a client sees: public columns

A model has two serializations, and neither is what a route should send:

| Serialization | Shape | Used for |
| --- | --- | --- |
| `ToMap()` | flat, every column, contributed ones merged in | writing rows, hook payloads |
| `encoding/json` | the struct's fields, with every contributed column under `"extra"` (the tag on `Extension.Extra`) | the session cache, key-value tokens |

Both carry every column. A route answers with a third form, `AuthContext.Public.Of(model)`, which is `ToMap()` reduced to the columns the table declares public:

1. `schema.Column` has a `Public` flag. It is false unless the declarer sets it, for a table's own columns and for a contribution alike.
2. `models.UserTableSchema` marks the eight user columns. No other core table marks any.
3. `Boot` builds `schema.NewPublicView(app.Schemas.All())`: for each merged table, the public columns by name.
4. `Of(m)` calls `m.ToMap()`, keeps the public columns of `m.SchemaName()`, and passes each value through `schema.Normalize`.

`schema.Normalize(col, raw)` returns a value as the type its column declares. Contributed columns reach the model as the driver returned them (`schema.Field` exists for the same reason), so without it a public boolean would be `1` in a response from SQLite and `true` from PostgreSQL. A value it can't convert is passed through.

| Model | `Of` returns |
| --- | --- |
| a user | the eight core columns, plus the contributed columns marked `Public` |
| a session, an account, a token | an empty map: their tables declare nothing public |
| a model whose table was not declared | a configuration error |

The routes that return a user (`WriteSignIn`, sign-up, the two verify routes, the email change confirm) all go through it.

The flag works as follows:

- **`Public` has the tag `json:"-"`.** Schema snapshots and migration files are JSON of `schema.Table`, and the differ's `columnsEqual` compares named fields. The flag appears in neither, so marking a column plans nothing and does not change a stored snapshot.
- **The model's own JSON is unchanged.** `Extension.Extra` still serializes every contributed column. The session cache depends on that: a cached session has to come back with all its columns.

### Declaration

`CoreDeclareSchema` declares core's tables (`users`, `sessions`, `tokens`, `accounts`, `rate_limits`, `audit_log`, `verification_codes`, and `reserved_values`) under owner `"core"` during `Prepare`, before any plugin declares. `rate_limits` is declared whether or not a key-value store is configured, because `Prepare` runs without a live connection and cannot know. With a key-value store that counts natively, the table exists and stays empty.

`[Reminder]` Adding a table to `CoreDeclareSchema`, or removing one, changes the name of the first migration of an application without plugins. `TestLauncherRunsAnApplication` (`cmd/behemoth/main_test.go`) checks that name, `0001_create_accounts_and_7_more.json` for the eight tables above: the first table and the count of the others. Update its `migrationName` and the list above in the same change. `cli/cli_test.go`, `docs/api/cli.md`, `README.md`, and `examples/cli/main.go` carry the name for an application with one table of its own (`0001_create_accounts_and_8_more`), and `docs/api/cli.md` the operation count of `examples/cli`: update them too.

### NULL versus empty

`Account.ToMap` writes an empty string or a nil time as `NULL` (`nullIfEmpty`, `nullIfNil`). `User` and `Session` write empty strings as empty strings. For `Account` the distinction matters: a missing token must be `NULL`, not an encrypted empty string.

---

## Accounts

```go
type Account struct {
	Extension
	ID         string
	UserID     string
	ProviderID string // "credential" | "google" | ...
	AccountID  string // the provider's id for the user; for "credential", the user id

	PasswordHash string // "credential" only

	AccessToken           string
	RefreshToken          string
	IDToken               string
	AccessTokenExpiresAt  *time.Time
	RefreshTokenExpiresAt *time.Time
	Scope                 string

	CreatedAt time.Time
	UpdatedAt time.Time
}
```

- **Uniqueness.** The unique index `uq_accounts_provider_account` covers `(provider_id, account_id)`. Creating a second account with the same pair is a `DuplicateKey` error.
- **The credential account.** `models.ProviderCredential` is `"credential"`. Its `AccountID` is the user's own id, so the email/password plugin finds it with `Store.FindAccount(ctx, models.ProviderCredential, user.ID)`.
- **A user without a credential account** signed up through an OAuth provider only and has no password. Sign-in answers them exactly like a wrong password, runs the same dummy hash for timing, and fires the failed hook with code `no_credential_account`.
- **JSON.** `PasswordHash` and the three tokens are tagged `json:"-"`. Handlers return models as JSON, and these must never be in a response.

### Secret columns

`models.AccountSecretColumns` names the columns encrypted at rest: `access_token`, `refresh_token`, `id_token`. `password_hash` is not in the list: it is already a one-way hash.

The store is the only place that encrypts and decrypts them. The rule is:

| Where | Form of the token |
|---|---|
| An `Account` the caller passes to or gets from the store | plaintext |
| The row in the database | sealed |
| The row or model a data hook receives | sealed |

### Seal and open

A sealed value is a string:

```
v<key version>:<base64 ciphertext>
```

- Every sealed value is bound to a `store.SealBinding`: a `Purpose` that names what the value is, and an `Owner` that names what it belongs to. Both are required.
- `Store.seal` calls `Encryptor.Encrypt` with the binding as the associated data. It returns the ciphertext and the version of the key it used, and `seal` writes both into that string. The base64 is `RawStdEncoding` (no padding).
- `Store.open` splits the string, parses the version, and calls `Encryptor.Decrypt(ciphertext, version, associated data)` with the binding the caller names. A value sealed with another binding fails like a changed one.
- The associated data is `behemoth.seal.v1`, then the purpose and the owner, each after its length as four big-endian bytes. The lengths keep two bindings from encoding to the same bytes.
- The ciphertext itself is the AES-256-GCM output of `crypto.AESGCMEncryptor`, with the nonce prepended. The key comes from the `KeyManager` under purpose `encrypt_at_rest`.
- `store.SealedKeyVersion(sealed)` reads the version without decrypting.
- `Store.Seal` and `Store.Open` are the exported pair, for a secret a plugin keeps outside `accounts`. The OAuth plugin seals the PKCE verifier of a started sign-in with them ([`../plugins/oauth.md`](../plugins/oauth.md)), and the two-factor plugin the TOTP secret ([`../plugins/twofactor.md`](../plugins/twofactor.md)).

An account's token is bound to the account and its column: the purpose is `accounts.<column>` and the owner is the account's id (`accountBinding`). It opens neither in another account's row nor in another column of its own row, so tokens can't be swapped between accounts, or an access token read as a refresh token. `CreateAccount` assigns the id before it seals.

`[Decision]` The key version travels inside the value instead of in a `key_version` column. An account has three secrets written at different times: an access token refreshed after a key rotation sits next to a refresh token sealed under the previous key. One version column would be wrong for one of them; three columns, or re-encrypting every token on every write, would be the alternative. The cost is that key version cannot be indexed. A re-encryption sweep after a rotation selects with a starts-with match on `v<old>:` (`clause.OpStartsWith`).

`[Not built]` No such sweep exists. Old key versions must stay available to the `KeyManager` for as long as rows sealed under them exist.

`[Decision]` A sealed value is bound to its purpose and its owner, not to its table and column. The two-factor plugin moves a sealed TOTP secret from `pending_secret` to `secret` when the user confirms it, which a binding to the column would break. For accounts the purpose names the column, because their tokens never move. A binding to the row's primary key would also do for accounts, but not for the OAuth verifier, which is sealed before its token row exists. Before the binding, the `Encryptor` took no associated data, and a value copied between rows still opened. No release sealed anything without a binding, so nothing needs a migration.

### What each store operation does

Each store operation behaves as follows:

- **`CreateAccount`** assigns the id and timestamps, seals each non-empty token, and inserts. The caller's `Account` holds the plaintext again when the call returns, whether it succeeded or failed.
- **`FindAccountByID`, `FindAccount`, `ListAccountsForUser`** open the tokens on a copy of the stored model and return the copy. The copy matters: the stored model may already have been handed to an after-hook, which must keep seeing the sealed form.
- **`UpdateAccount`** takes token columns in plaintext in the changes map. It seals them before the before-update hook runs. A value of `""` or `nil` clears the column to `NULL`. A non-string value is a validation error. The caller's map is not modified.
- **`DeleteAccount`** deletes by id.

### The encryptor is required for tokens

`store.WithEncryptor(e)` supplies the encryptor. `Boot` passes `Crypto.AtRest`.

`[Convention, important]` A store built without an encryptor returns a configuration error when asked to write or read a token. It never falls back to storing plaintext. Accounts with no tokens, such as credential accounts, work without one.

A stored value that is not a valid sealed string, or whose ciphertext is truncated or tampered with, is a security error from `open`. It is not a panic.

### Limits to know

These limits apply:

- **Hooks do not fire for accounts today.** The store calls its hooks for every table, but `coreDataHookPoints` in `dispatch/datahooks.go` only maps `users` to hook points. The "hooks see the sealed form" rule takes effect when account points are declared.
- **A hook that rewrites a token column on create is not reflected in the caller's model.** `CreateAccount` restores the caller's original plaintext after the insert.
- **A sealed value can be rolled back.** The binding stops a value from moving between owners and purposes, not an older value of the same owner from being written back. Someone with write access to the table could restore an account's earlier token.

---

## Moving `password_hash` from `users` to `accounts`

`users.password_hash` no longer exists in the declared schema. The hash now lives in `accounts.password_hash` on the user's credential account.

`[Important]` On a database created before this change, every existing user's hash is still in `users.password_hash`, and no credential account exists for them. Two things follow:

- Until the hashes are copied, existing users cannot sign in with a password: sign-in looks for a credential account and finds none.
- The migration planner sees `password_hash` as a live column that is no longer declared and raises a plan issue offering to drop it. Choosing "Drop column" before the copy destroys every password.

The planner never drops the column on its own. A drop is a plan issue with two options ("Drop column", "Leave as-is"), and the drop operation must be confirmed.

### Order of operations

Move `password_hash` in this order:

1. Generate and apply the migration that creates `accounts`. Resolve the `password_hash` issue as **Leave as-is**.
2. Copy the hashes into credential accounts (below).
3. Check that the number of credential accounts equals the number of users that had a hash.
4. Deploy the code that reads the hash from accounts.
5. In a later migration, resolve the issue as **Drop column**.

Steps 2 and 4 must not be far apart. A user who signs up through the old code after the copy has a hash in `users` and no account. Either run the copy again just before step 5 (it is idempotent as written below) or stop sign-ups during the switch.

### The copy

`[Not built]` The migration system has no data operation. `SchemaOperation` kinds are all schema changes, and a `CustomMigration` is made of the same kinds, so the copy cannot be expressed as a behemoth migration today. It is run as SQL against the database.

The statement below was checked on SQLite only (it copies the hashes and a second run inserts nothing). Test it on a copy of the real database before running it in production:

```sql
INSERT INTO accounts (id, user_id, provider_id, account_id, password_hash, created_at, updated_at)
SELECT u.id, u.id, 'credential', u.id, u.password_hash, u.created_at, u.updated_at
FROM users u
WHERE u.password_hash IS NOT NULL
  AND u.password_hash <> ''
  AND NOT EXISTS (
    SELECT 1 FROM accounts a
    WHERE a.provider_id = 'credential' AND a.account_id = u.id
  );
```

- The account's `id` reuses the user's id. It only has to be a unique string of at most 36 characters, and this avoids a database-specific UUID function.
- `account_id` is the user id, which is what the plugin looks up.
- The names above are canonical. If the application maps physical table or column names through the resolver, use the physical ones.
- The hash string is copied unchanged. It is a self-describing Argon2id string, so it verifies the same from its new location.

---

## Data hooks and transactions

Behemoth has two tiers of hooks. Tier 1 data hooks (`data.user.beforeCreate`, `data.user.afterCreate`, ...) are fired by the store around a write to one table. Tier 2 flow hooks (`auth.signUp.before`, `auth.signUp.after`, ...) are fired by a plugin around a whole operation. This section covers Tier 1.

`[Convention]` A data hook is part of the write. It runs inside the write's database transaction, before and after phases alike, and it should only do work that the transaction's rollback undoes. Compare the two after hooks on sign-up:

| | `data.user.afterCreate` (Tier 1) | `auth.signUp.after` (Tier 2) |
| --- | --- | --- |
| Runs | after the insert, before commit | after the transaction has committed |
| The user | may still be rolled back | is durable |
| A handler error | fails the write and rolls it back | is logged; sign-up still succeeds |
| A later handler | does not run after a failure | runs anyway |
| Meant for | rows in other tables that belong with the user | welcome emails, webhooks, other systems |

### How a hooked write runs

`Store.create` and `Store.update` wrap three steps in one transaction (`Store.hooked` in `store/create.go`):

1. The before hook runs. It may rewrite the row or return an error.
2. The row is written.
3. The after hook runs. It may return an error.

An error at any step rolls the transaction back and is returned to the caller as-is. A panic in a handler is turned into an internal error and treated the same way.

If the store is already bound to a transaction (`Store.Transaction`), the write joins it instead of opening one. There is no nesting. Email/password sign-up is the example:

```go
err = ac.Store.Transaction(hctx.Ctx, func(ctx context.Context, tx *store.Store) error {
	if err := tx.CreateUser(ctx, user); err != nil {
		return err
	}
	return tx.CreateAccount(ctx, &models.Account{
		UserID: user.ID, ProviderID: models.ProviderCredential,
		AccountID: user.ID, PasswordHash: passwordHash,
	})
})
```

1. `data.user.beforeCreate` runs.
2. The user row is inserted.
3. `data.user.afterCreate` runs. The transaction is open.
4. The account row is inserted.
5. The transaction commits, or rolls back if any step failed.

If step 4 fails, the after-create hook in step 3 has already run for a user that is then rolled back. Whatever the hook wrote through the transaction is rolled back with it.

Only tables that fire hooks get a transaction. `store.Hooks.Fires(table)` answers that, and `dataHooks` answers it from `coreDataHookPoints`, which maps only `users` today. Writes to `sessions`, `tokens`, `accounts`, and `rate_limits` go straight to the database as before.

### The hook's store: `HookContext.Tx`

The store passes itself to every `store.Hooks` method, and `dataHooks` publishes it as `HookContext.Tx`. Inside a hooked write it is the store bound to the transaction.

```go
registry.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, result any) error {
	user := result.(*models.User)
	// Commits or rolls back with the user.
	return hctx.Tx.CreateAccount(hctx.Ctx, &models.Account{UserID: user.ID /* ... */})
}, nil)
```

`[Convention]` `Tx` has two surfaces, and a handler picks by who owns the table:

- **Core tables** (users, accounts, sessions, tokens) go through the store's methods on `Tx`. They are the only place the tables' invariants are applied.
- **Any other table** (a plugin's or the application's) goes through `Tx.DB()`, the `behemoth.Database` the store itself writes through. On a transaction-bound store it is the adapter bound to that transaction.

```go
registry.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, result any) error {
	user := result.(*models.User)
	return hctx.Tx.DB().Create(hctx.Ctx, &Profile{UserID: user.ID})
}, nil)
```

`Store.DB()` returns `s.db` and nothing more. `Store.Transaction` builds the bound store with `tx.db = db`, the adapter the driver handed to the transaction callback, so `DB()` follows the store into and out of transactions without extra state.

What the adapter skips when it is used on a core table:

| Store step | Where it lives | Effect of skipping it |
| --- | --- | --- |
| Data hooks | `Store.create`, `Store.update` | no `data.user.*` handler runs; other plugins' rewrites and vetoes are bypassed |
| Token sealing | `CreateAccount`, `UpdateAccount` (`seal.go`) | OAuth tokens land in plaintext; `openAccount` then fails to open them |
| Ids and timestamps | `CreateUser`, `CreateAccount`, ... | empty primary key, zero timestamps |
| Email normalization | `CreateUser`, `UpdateUser` (`normalizeUserRow`, applied after the before hooks) | the row is not found by `FindUserByEmail` |
| Column check | `checkColumns` | unknown keys are no longer a validation error at the store |
| Session cache | `DefaultSessionManager` | a revoked session stays valid from the KV cache until its TTL |
| Secret hashing | `DefaultSessionManager.Create`, `DefaultTokenManager.Issue` | a session or token row without matching lookup and keyed hashes can't be validated |
| Session and token hook points | the two managers | `auth.session.*` and `token.*` handlers don't run |
| Session limits and config | `DefaultSessionManager` | `MaxConcurrent`, expiry and `CaptureIPAndAgent` are not applied |
| Single-use consume | `Store.ConsumeToken` | two concurrent requests can both use a single-use token |
| Token backend | `DefaultTokenManager.persist` | a kind stored in the key-value storage is missed by a direct read of `tokens` |

Nothing enforces the convention. The adapter accepts any declared model. The per-table list for plugin authors is in [`../../api/core-tables.md`](../../api/core-tables.md); keep it in step with this table when a store or manager step is added.

`hctx.Auth.Store` and `hctx.Auth.DB` are the root store and adapter and use a different connection. From inside a data hook:

- A write through them is not part of the transaction and stays if the transaction rolls back. A handler may want exactly that, for a record that should survive a rejected write.
- A read through them does not see the row being written. A foreign key from a row they write to the new user fails for the same reason.
- On SQLite they can wait on the write lock the transaction holds.

`HookContext.Tx` is nil on Tier 2 hooks. They do not run inside a store transaction.

How each adapter binds a transaction, and what `Transaction` does when called on a bound adapter, is described in [`../database/adapters/transactions.md`](../database/adapters/transactions.md). Three points from there matter to a handler:

- **The context carries the transaction on MongoDB.** A call on `Tx.DB()` with a context other than `hctx.Ctx` runs outside the transaction.
- **The bound adapter ends with the hook.** On the SQL adapters it wraps a `*sql.Tx` and returns `sql.ErrTxDone` afterwards.
- **A failed statement aborts a Postgres transaction.** A handler that swallows an adapter error leaves the transaction unusable, and the commit fails.

### The dispatcher: `RunAfter` and `RunAfterTx`

Both run the same frozen after-chain. They differ in what a handler error means.

| | `RunAfter` (Tier 2) | `RunAfterTx` (Tier 1) |
| --- | --- | --- |
| Handler returns an error | logged, chain continues | chain stops, error returned |
| Handler panics | logged as an error, chain continues | chain stops, internal error returned |
| Return value | nil, unless the point can't be dispatched | the first error |
| Audit event (`HookPointDef.Audit`) | recorded once the operation is over, best effort | written through the write's transaction; a failed write is returned |

All four dispatcher methods start with `DefaultDispatcher.checkPhase`. A point that was never declared, or is declared with another phase, returns a configuration error and runs no handler. For `RunAfter` and `Fail` this is the only error they return.

`Boot` runs the same check once for the points in `coreDataHookPoints` (`CheckDataHookPoints`), so a table mapped to an undeclared point fails `Boot` instead of the first write.

`dataHooks.AfterCreate` and `AfterUpdate` call `RunAfterTx` and return its error to the store. Before hooks need no second entry point: `RunBefore` already stops at the first error.

### After commit: the commit queue

`data.user.created` and `data.user.updated` fire once the write's transaction has committed. The store does not know hook points; it has a queue of callbacks per transaction and two more `store.Hooks` methods.

The commit queue works in these steps:

1. `Store.Transaction` creates a `commitQueue` and puts it on the bound store (`tx.commit`). A `Transaction` call on a bound store returns that same store, so a joined transaction shares the queue of the outermost one.
2. `dataHooks.AfterCreate` and `AfterUpdate` queue the dispatch of the committed point with `tx.AfterCommit` once the in-transaction after chain has succeeded (`dataHooks.onCommit`). The store has no call of its own for this, and a table that fires no hooks queues nothing.
3. When the adapter's `Transaction` returns nil, `Store.Transaction` runs the queue in order, with the context it was called with, and then returns. When it returns an error, the queue is dropped.
4. The queued callback dispatches the point with `RunAfter`: handler errors are logged, an audit event is recorded if the point declares one, and `HookContext.Tx` is nil. `HookContext.Values` are the write's, the same map its before and after handlers saw.

Sign-up shows why the queue belongs to the outermost transaction:

| Step | Queue |
| --- | --- |
| `Store.Transaction` opens | empty |
| `tx.CreateUser`: before hook, insert, after hook | `created(user)` |
| `tx.CreateAccount` fails | dropped with the rollback; nothing fires |
| `tx.CreateAccount` succeeds, commit | runs: `data.user.created` fires |

Details that matter:

- **A queue per attempt.** MongoDB may run the transaction callback more than once. Each attempt gets a fresh queue, and only the queue of the attempt that committed is run, so a retried write is reported once.
- **The context.** Callbacks get the context of the `Store.Transaction` call. The callback's own context carries the MongoDB session, which has ended by then.
- **`Store.AfterCommit(ctx, fn)` is public.** A handler or a flow can queue its own callback. On a store that is not bound to a transaction it runs `fn` at once.
- **Callbacks run synchronously**, before `Transaction` returns, in the calling goroutine.
- **A callback's panic is recovered.** `Store.runCommitted` wraps each callback. A panic is logged at Error under the `store` component (when the store has a `Telemetry`), the callbacks queued behind it still run, and `Transaction` returns nil because the write is committed. The same wrapper runs a callback called at once on an unbound store. Handler panics on the `created` and `updated` points never get this far: the dispatcher recovers them first.
- **Adapter-level transactions are not seen.** Only `Store.Transaction` owns a queue. A transaction opened directly on the adapter around store calls does not delay the notifications of those calls.

### What a rollback does not undo

A rollback does not undo these writes:

- **Key-value writes.** A hook that writes to `KeyValueStorage` (Redis, a KV-backed token) leaves that write behind. Only database writes through `HookContext.Tx` are covered.
- **Writes through `Auth.Store` or `Auth.DB`**, as described above.
- **Adapter calls made with a context other than `hctx.Ctx`** on MongoDB.
- **Outside side effects.** An email or a webhook call sent from a data hook can't be recalled. These belong in Tier 2.
- **Rate-limit counters.** `RunBefore` checks a point's rate limit before the handlers run, through a store that is never bound to a transaction. An attempt that is later rolled back still counts. This is intended: the limit is on attempts.
- **The caller's model.** `Store.create` reads the before hook's rewrite back into the model and the caller's `CreateUser` has already set its id and timestamps. After a failed create the model keeps those values for a row that does not exist.
- **`HookContext.Values`** and any other in-memory state a handler changed.

### Limits to know

These limits apply:

- **After-commit delivery is best effort.** The queue is in memory. A process that stops between the commit and the callbacks never runs them. There is no outbox in core; see the decision below.
- **The in-transaction after points are audited inside the transaction.** `data.user.afterCreate` and `data.user.afterUpdate` are declared with an `AuditSpec`. `RunAfterTx` writes the event through `HookContext.Tx`, so it commits or rolls back with the row, and a failed audit insert fails the write. The after-commit points carry no spec, so a write is recorded once. See `../telemetry/audit.md`.
- **A hook can run more than once on MongoDB.** The Mongo adapter uses `session.WithTransaction`, which runs its callback again on a transient error. A hooked write and its hooks are inside that callback. Writes through `HookContext.Tx` and `Tx.DB()` are rolled back between attempts, so they are unaffected. Anything else a hook does is repeated.
- **Hooked writes need transaction support.** A write to a table that fires hooks always opens a transaction. MongoDB only supports transactions on a replica set or sharded cluster, so the MongoDB adapter requires one. `Boot` fails on a standalone server (`behemoth.TransactionChecker`, see [`../database/adapters/transactions.md`](../database/adapters/transactions.md)).
- **Hooks hold the transaction open.** A slow handler keeps locks for as long as it runs. On SQLite that blocks every other writer.
- **Hooks fire only for `users`.** The transaction rules apply to any table that gains hook points in `coreDataHookPoints`. The `auth.session.*` and `token.*` points are not data points: the session and token managers fire them as Tier 2 points, so `sessions` and `tokens` have no entry there. The managers build a `HookContext` per dispatch (`hookContext` in `auth/session.go`) with the point, the phase, the operation's `Values`, the request and the `AuthContext` that `Boot` passes to their constructors. `Tx` is nil.

### Data hooks run inside the write's transaction

**Context:** `data.user.afterCreate` ran at the point of the insert. Inside sign-up's transaction it could act on a user that the account insert then rolled back, it could not reach the transaction to write related rows, and its error was logged and ignored. The after hook was neither a reliable "the row is committed" signal nor a usable part of the write.

**Options considered:**

- *After hooks fire after commit.* Gives a reliable "committed" signal. A handler can no longer fail the write or add rows atomically, and the store has to queue hooks until the outermost transaction commits.
- *After hooks stay as they were, with a documented warning.* No code change. Handlers still can't write in the transaction or abort, so the only safe handler is one that does nothing durable.
- *Before and after hooks both run inside the transaction, and an after-hook error rolls back.* A handler can add related rows atomically and veto the write. Handlers must avoid work a rollback can't undo, and the dispatcher needs an after-dispatch that returns errors.

**Decision:** The third option. It matches what a data hook is for, plugins adding their own rows and columns to a write, and it gives after hooks a contract that is the same inside and outside a caller's transaction. Irreversible side effects already have a home in Tier 2, which fires after commit. Tier 2 after hooks keep the log-and-continue behaviour.

**Revisit if:** the in-transaction contract proves too strict for common handlers. Work that must not fail the write has its own points now (`data.user.created`, see the next decision).

### After-commit data hooks use an in-memory queue

**Context:** The in-transaction after hook can't mean "this row is committed", and the Tier 2 after points only cover one flow each. With OAuth, users are created by more than one flow, and a plugin that reacts to every new user would have to know all of them. Data points also could not be audited.

**Options considered:**

- *Fire after the single write.* Simple, but wrong inside a caller's transaction: sign-up's user would be reported before the account insert that can still roll it back.
- *An in-memory queue on the outermost transaction, run after commit.* Correct for nested writes and cheap. A process that stops between the commit and the callbacks loses them. This is what better-auth's `queueAfterTransactionHook`, Rails' `after_commit` and Django's `transaction.on_commit` do.
- *A transactional outbox in core.* The event is a row written in the same transaction, and a worker delivers it at least once. Nothing is lost on a crash. Core would own a table, a worker, retries, and cleanup, and every handler would have to be idempotent.

**Decision:** The in-memory queue, documented as best effort. It covers the common uses (emails, stats, cache updates). An application that needs guaranteed delivery writes its own outbox row from `data.user.afterCreate` through `HookContext.Tx`, which is atomic with the user row, and runs its own worker. The API docs describe that recipe.

**Revisit if:** core features need guaranteed delivery themselves (for example an audit log that must not miss a write, or webhooks as a built-in feature). An outbox in core would then be justified.

### After-commit callbacks stay in the caller's goroutine, and their panics are recovered

**Context:** The commit queue runs in the goroutine that called `Transaction`, before it returns. Two things followed. A callback that panicked unwound into the caller after the commit, so a stored sign-up looked like a failure and the callbacks behind it never ran. A slow callback (an email sent inline from `data.user.created`) delays the response.

**Options considered:**

- *Recover each callback's panic in `Store.Transaction`.* About fifteen lines and no API change. It fixes the panic and leaves the latency.
- *Run the queue in a goroutine.* Fixes the latency for every application. The store then needs a wait for shutdown, a bound on concurrent callbacks and a timeout, since the request's cancellation no longer applies. Handlers lose two things they can rely on today: `hctx.Request` may be read after the response is written, and `data.user.created` no longer runs before `auth.signUp.after`. The audit recorders that are not written in the transaction share the queue and would become asynchronous too.
- *An executor supplied by the application.* `store.WithCommitRunner(func(run func()))`, passed through `BootConfig`, with the synchronous loop as the default. The application owns the pool, the bound and the shutdown, and core gains no background state. The stale request and the lost ordering apply to the applications that opt in.
- *A transactional outbox in core.* The only option where queued work survives a crash. See the decision above for its cost.

**Decision:** Recover the panic and stay synchronous. A handler with slow work can start its own goroutine or queue a job, which solves the latency where it occurs, while the hazards of an asynchronous queue would reach every handler. The executor option and the outbox are deferred, not rejected: [`../../ongoing.md`](../../ongoing.md) tracks both.

**Revisit if:** an application needs every committed callback off the request path (the executor), or core needs delivery that survives a crash (the outbox).

### An undeclared hook point is a configuration error, not a panic

**Context:** `checkPhase` panicked when a point was dispatched without being declared, or in the wrong phase. Core fired the `auth.*`, `auth.session.*`, and `token.*` points without declaring them, so sign-in, session creation and token issue panicked under a real `Boot`. Every other setup mistake (a duplicate declaration, a handler on an unknown point, a reserved owner name) is returned as a `ConfigurationError`.

**Options considered:**

- *Keep the panic.* Loud, and the signatures of `RunAfter` and `Fail` stay without a return value. A missing declaration in one flow takes the whole process down at request time, unless the HTTP layer recovers.
- *Return the error from `RunBefore` and `RunAfterTx`, log it in `RunAfter` and `Fail`.* No interface change. A flow with an undeclared after or failed point then looks healthy while its handlers and audit events never run.
- *Return a `ConfigurationError` from all four methods.* The mistake reaches the caller as an error on every path. `RunAfter` and `Fail` gain an error return that every caller has to check, and a flow can fail after its work is done (the session row exists, the after point is undeclared).

**Decision:** The third option. `RunAfter` and `Fail` return only this error; handler errors are still logged. Core now declares every point it fires (`CoreDeclareHookPoints`), with the session points split into `beforeCreate`/`afterCreate` and `beforeRevoke`/`afterRevoke` because a point has one phase.

**Revisit if:** the set of dispatched points can be known at boot (for example flows registering the points they fire). The check could then move to `Boot` entirely and `RunAfter` and `Fail` could drop the return value.

### A client sees public columns, and a column is private unless declared public

**Context:** The routes encoded `models.User` directly, and its `Extension` serialized every contributed column under `"extra"`. A plugin that added a column no client should see (a risk note, a billing id) had it returned by sign-up, sign-in, and the verify routes.

**Options considered:**

- *Filter in `Extension`'s JSON.* No route changes. The session cache and key-value tokens use the same encoding, so a cached session would lose its private columns, and a model can't see the schema while it is being marshalled.
- *Return core columns only; plugins add fields to responses themselves.* Nothing to leak. Every plugin with a public column would need a way into every response that carries a user.
- *A flag on the column and a view built at the response.* The plugin decides where it already declares the column. Routes have to answer with the view and not with the model.
- *Public unless marked private.* Less to write. A forgotten flag leaks.

**Decision:** The flag, with private as the default, and `PublicView` on the `AuthContext`. The view is flat, like `ToMap`, so a response names a column the way a request does and the `extra` nesting is gone from responses.

**Revisit if:** a column has to be visible to some viewers and not others (an admin listing). `PublicView` is an interface so that a per-request decision can wrap the schema's; nothing does yet.

### Core tables through the store is a convention, not an enforced rule

**Context:** `Store.DB()` and `AuthContext.DB` give plugins the raw adapter. A write to `users`, `accounts`, `sessions`, or `tokens` through it skips everything the store and the managers add. The most serious case is an OAuth token stored in plaintext, which nothing reports at write time.

**Options considered:**

- *A wrapper `Database` that rejects writes to core-owned tables.* The schema registry knows each table's owner, so the wrapper is cheap to build and turns a silent mistake into an error. It also blocks legitimate direct writes (a backfill, an import, a maintenance job that must not fire hooks), and it fixes the set of protected steps in code. Sealing may become optional per configuration later, and a plugin can already avoid the sensitive tables by using the managers.
- *Keep it a convention and document exactly what a direct write skips.* No code, and the plugin author decides per write. A plugin that ignores the documentation can still store a token in plaintext.

**Decision:** The convention. Plugin authors are expected to use the store for core tables and the managers for sessions and tokens, and `docs/api/core-tables.md` lists per table what a direct write skips so that the choice is an informed one. `Store.DB` and `AuthContext.DB` carry the same warning in their doc comments.

**Revisit if:** direct writes to core tables become a recurring source of bugs or of plaintext secrets in third-party plugins. The wrapper could then be offered as an opt-in (`BootConfig`), or limited to the columns that are sealed.

### Plugins write their own tables through the adapter

**Context:** A data hook runs inside the write's transaction, but `Store` only had typed operations for core tables. A plugin that owns a table could not write it inside the transaction, so the convention above could not be followed for the tables plugins care most about. Plugins such as a dashboard or an audit log also need to read every table.

**Options considered:**

- *Generic `Create`/`Update`/`Find` on `Store` for any declared model.* Keeps one door to the database. It re-implements the adapter interface one level up, and the generic methods would have to decide per table whether hooks, sealing, and normalization apply.
- *Expose the adapter: `Store.DB()`.* One accessor, no new API to maintain, and plugins get the full `behemoth.Database` they already have as `AuthContext.DB`. The store is no longer the only path to core tables, so their invariants rest on a convention.

**Decision:** Expose the adapter. `AuthContext.DB` was already public, so the store was never an enforced boundary; what was missing was the transaction-bound adapter. The store stays the owner of core tables' rules, and the convention (core tables through the store, own tables through the adapter) is documented in the package doc, on `Store.DB` and in the API docs.

**Revisit if:** plugins writing core tables through the adapter becomes a recurring source of bugs. A wrapper adapter that rejects writes to core tables would then enforce the convention.
