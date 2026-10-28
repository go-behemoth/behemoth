# Write Behemoth's tables from a plugin

A plugin can reach the database in two ways. This page explains what each one does, so you can choose per write.

| Access | How you get it | What it is |
| --- | --- | --- |
| The store | `ac.Store`, or `hctx.Tx` inside a data hook | Typed operations for Behemoth's own tables, such as `CreateUser` and `UpdateAccount` |
| The adapter | `ac.DB`, `ac.Store.DB()`, or `hctx.Tx.DB()` inside a data hook | The plain `behemoth.Database`, with `Create`, `FindOne`, `UpdateOne`, and so on, for any model |

Sessions and tokens also have managers, which sit above the store: `ac.SessionManager` and `ac.TokenManager`.

## The convention

- Your own tables: use the adapter. The store has no operations for them.
- Behemoth's tables (`users`, `accounts`, `sessions`, `tokens`, `verification_codes`, `reserved_values`, `rate_limits`): use the store, and for sessions, tokens, and one-time codes the managers (`ac.Codes` for codes; see [SMS and one-time codes](sms.md)).

Nothing enforces this. The adapter accepts any declared model, and a write to a core table through it succeeds. The adapter writes the row you give it and nothing else, so every step listed below becomes your job. The rest of this page lists those steps per table. If you write directly, do it knowing which ones you are taking over.

## What a direct write skips, on every core table

| Step | Done by | If you skip it |
| --- | --- | --- |
| Id | the store, on create | The row is inserted with an empty primary key unless you set one. |
| `created_at`, `updated_at` | the store, on create and update | Zero timestamps on create. `updated_at` does not move on update. |
| Column check | the store, on create and update | A misspelled or unknown column is not reported as a validation error. What happens next depends on the database: an SQL error, or on MongoDB a stray field. |
| Model returned after an update | the store reads the row back | You get nothing back and read the row yourself. |

## Users

| Step | Done by | If you skip it |
| --- | --- | --- |
| `data.user.beforeCreate`, `data.user.beforeUpdate` | the store | Other plugins' handlers don't run. A plugin that fills its own column on `users`, or rejects certain emails, is bypassed. A contributed `NOT NULL` column without a default then fails the insert. |
| `data.user.afterCreate`, `data.user.afterUpdate` | the store | Rows that other plugins add next to a user (a profile, an audit entry) are not written. |
| `data.user.created`, `data.user.updated` (after commit) | the store | Plugins that react to new or changed users (a welcome email, a stats counter) are not told. |
| Deleting a user's sessions, accounts, and tokens with the user, clearing the session cache, and the `data.user.*Delete` and `auth.session.afterRevoke` points | `Store.DeleteUser` | A direct delete of the user row leaves all of it to the database. Where foreign keys cascade, the rows go without any hook, and cached sessions keep working until they expire. On MongoDB, and on SQLite without `_foreign_keys=on`, the sessions and accounts stay. See [Delete a user](hooks.md#delete-a-user). |
| The transaction around the write and its hooks | the store | Your write is a single statement. Wrap it yourself if it belongs with other writes. |
| Email normalization (trim, lowercase) | the store | A user stored as `Ada@Example.com` is not found by `FindUserByEmail`, and can sign up a second time as `ada@example.com`. `store.NormalizeEmail` is exported if you need the same form. |
| Answering a taken value of a unique column with the column's code (`email_taken`) | the store | You get the duplicate key error, with the table's code (`users_already_exists`). `errs.DuplicateKeyColumns(err)` names the column. |

### What a client sees of a user

Every route that returns a user answers with the user's **public columns** and nothing else. Core's own eight columns are public. A column you add to `users` is private until you mark it public where you declare it:

```go
var Plan = schema.Field[string]{Table: models.UserTable, Name: "plan"}
var RiskNote = schema.Field[string]{Table: models.UserTable, Name: "risk_note"}

func (p *Plugin) Declare(ic *core.PluginInitContext) error {
	// Clients may see the plan.
	if err := ic.Schemas.ExtendColumn(Plan.Contribution(schema.Column{Type: schema.ColTypeString, Length: 32, Public: true})); err != nil {
		return err
	}
	// Nothing to mark: a column is private by default.
	return ic.Schemas.ExtendColumn(RiskNote.Contribution(schema.Column{Type: schema.ColTypeText, Nullable: true}))
}
```

A sign-in then answers with:

```json
{"user": {"id": "2ff9...", "email": "ada@example.com", "email_verified": true, "plan": "pro"}}
```

The flag follows these rules:

- **Private is the default**, so a column reaches a client only because its plugin said so. Forgetting the flag hides a column and never exposes one.
- **The flag is about responses only.** Your plugin, every hook handler, and the store see all the columns. Read yours with the field: `note, ok, err := RiskNote.Get(user)`.
- **A public column sits next to core's**, under its column name. There is no nesting.
- **Values have their declared type.** A boolean column is `true` or `false` in the JSON on every database, although SQLite stores it as `1` or `0`.
- **It is not a database property.** Marking a column plans no migration.
- **It is the same for every viewer.** The flag can't make a column visible to one client and hidden from another.

In a route of your own, answer with the view and not with the model:

```go
view, err := rctx.Auth.Public.Of(user)
if err != nil {
	return err
}
return rctx.Response.JSON(http.StatusOK, behemoth.M{"user": view})
```

Don't pass a `*models.User` to `Response.JSON`. Encoded directly, a user carries every contributed column under `"extra"`, the private ones included. That encoding exists for Behemoth's own storage.

The other core tables declare no public column. `Public.Of` on a session, an account or a token returns an empty map.

### A unique column you add

A column you declare `Unique` gets a conflict code of its own. When a write through the store gives it a value another user holds, the store answers `409` with the code `<column>_taken`, whichever flow wrote the row. Set `ConflictCode` to use another code:

```go
var Handle = schema.Field[string]{Table: models.UserTable, Name: "handle"}

func (p *Plugin) Declare(ic *core.PluginInitContext) error {
	return ic.Schemas.ExtendColumn(Handle.Contribution(schema.Column{
		Type: schema.ColTypeString, Length: 64, Nullable: true, Unique: true,
		ConflictCode: "handle_in_use", // the default would be "handle_taken"
	}))
}
```

The answer follows these rules:

- **It covers a race.** Check the value in your flow or in a `data.user.beforeCreate` handler, to answer with a message of your own, as the username plugin does. Two writes can both pass that check. The database then refuses the second, and the store answers it with the same code.
- **The message is generic:** "handle is already taken". The code is what a client branches on.
- **It is a conflict that holds the duplicate key.** `errs.IsCategory(err, errs.CategoryConflict)` and `errs.IsDuplicateKey(err)` are both true, and `errs.DuplicateKeyColumns(err)` names the column.
- **It is for one column.** A unique index of several columns keeps the duplicate key error, with the table's code (`users_already_exists`).
- **It is not a database property.** Setting `ConflictCode` plans no migration.

## Accounts

| Step | Done by | If you skip it |
| --- | --- | --- |
| Sealing `access_token`, `refresh_token`, and `id_token` on write | the store | The tokens are stored in plaintext, in the table and in its backups. Nothing reports it at write time. |
| Opening them on read | the store | A direct read returns the sealed string (`v1:<base64>`), which is not a usable token. |
| Consistency between the two | the store | A row you wrote in plaintext can't be read through the store afterwards: `FindAccount` returns a security error (`invalid_sealed_value`) for it. |

Each token is sealed for its account and its column. A sealed token copied into another account's row, or into another column, can't be read through the store: `FindAccount` returns a security error (`invalid_payload`) for it.

These points also apply:

- `password_hash` is not sealed. It is a one-way hash and is stored as given in both paths. Hash it with `ac.Crypto.Passwords` before you store it.
- If you need to write tokens directly, use `UpdateAccount` for the token columns and the adapter for the rest.
- `Store.Seal(value, binding)` returns the sealed form of a value, and `Store.Open(sealed, binding)` reads it back. Use them for a secret your plugin keeps in a table of its own or in a token's metadata. The `store.SealBinding` names what the value is (`Purpose`, starting with your plugin's name) and what it belongs to (`Owner`, such as the user's id). `Open` reads the value only with the binding it was sealed with, so a value copied into another user's row can't be read there. Pick an owner that never changes, and use the same purpose and owner to open the value.

## Sessions

Use `ac.SessionManager`. It does more than the store, and a direct write skips what the store does and what the manager adds.

| Step | Done by | If you skip it |
| --- | --- | --- |
| Generating the raw token, storing only its lookup hash and keyed hash | the session manager | You must produce both hashes the same way, or the session can't be validated. Storing a raw token makes every session in the table usable by whoever reads it. |
| Expiry (`ExpiresIn`, `PendingExpiresIn`), `last_active_at`, `fresh_at` | the session manager | The session has whatever you wrote, not what `SessionConfig` says. |
| `MaxConcurrent` and eviction of the oldest session | the session manager | The limit is not applied to your session. |
| `CaptureIPAndAgent` | the session manager | IP address and user agent are stored even when the application turned capture off. |
| The key-value cache | the session manager | A session you revoke directly stays valid from the cache until its entry expires. A session you create directly is not cached, which only costs a lookup. |
| `version`, raised by one in each update, which is guarded on the version read (`Store.UpdateSessionAt`) | the store, called by the session manager | A direct update that leaves `version` as it is may not reach the session cache: the cache keeps the copy it holds of the same version until the session's next update through the manager. Two direct updates at once can overwrite each other. See [The session cache](sessions.md#the-session-cache). |
| `auth.session.beforeCreate`, `afterCreate`, `beforeRevoke`, `afterRevoke` | the session manager | Other plugins are not told. A handler that vetoes sessions for a suspended user is bypassed. |
| Terminal state rule (a revoked session stays revoked) | the session manager | A direct update can set a revoked session back to active. |

## Tokens

Use `ac.TokenManager`.

| Step | Done by | If you skip it |
| --- | --- | --- |
| Generating the raw token, storing only its lookup hash and keyed hash | the token manager | As for sessions: the token can't be verified, or is stored in a usable form. |
| The declared kind: TTL, single use, backend | the token manager | The row ignores the kind's `TokenKindDef`. A kind whose backend is the key-value storage is not in the table at all, so a direct read misses it. |
| Consuming exactly once | the store (`ConsumeToken`), called by the manager | Two concurrent requests can both use a single-use token. The store's consume is a guarded update: of several concurrent calls one succeeds. |
| `token.beforeIssue`, `afterIssue`, `consumed`, `failed` | the token manager | Other plugins are not told. |

## Rate limits

`rate_limits` holds the rate limiter's counters. `Store.IncrementRateLimit` increments a counter atomically, starts a new window when the old one has passed, and returns the count and the window's end. With Redis configured as the key-value storage, the counters are kept there and this table stays empty. A direct write can lose increments under concurrency or leave a window that never resets.

## Audit log

`audit_log` holds the audit events. `Store.RecordAuditEvent` assigns the id that events are ordered and paged by. A row written directly with another kind of id appears out of order in `Store.QueryAuditEvents`. To record an event of your own, use `ac.Telemetry.RecordAudit`, which also fills in the request ID and redacts the metadata. See [Telemetry](telemetry.md#audit).

The table is created by the migration engine like the other core tables. While Behemoth records to it, which is the default, writes to `users` fail if it is missing.

## Reserved values

`reserved_values` keeps a value of a unique column of `users` for the user who gave it up, until an expiry. Until then no other user is given the value. The email change flow of the [email verification plugin](emailverification.md) keeps an account's old address there while the link at it can put it back. A plugin that frees values of a unique column of its own can use the table the same way.

Use the store:

| Operation | What it does |
| --- | --- |
| `Store.ReserveValue(ctx, column, value, userID, until)` | Keeps `value` of `column` for `userID` until `until`. A longer reservation of the same user is kept, and another user's is replaced. |
| `Store.CheckReservedValue(ctx, column, value, userID)` | Returns the error of a taken value (`409`, the column's code: see [A unique column you add](#a-unique-column-you-add)) when `value` is reserved for a user other than `userID` and the reservation has not expired. Pass an empty `userID` for a user being created. |
| `Store.ReleaseReservedValue(ctx, column, value, userID)` | Removes the reservation of `value` if it is `userID`'s. |
| `Store.FindReservedValue(ctx, column, value)` | Returns the reservation, expired or not. |
| `Store.PurgeExpiredReservedValues(ctx)` | Removes the expired reservations. |

A reservation behaves as follows:

- **It refuses only where it is checked.** The store does not check reservations on every write. Call `CheckReservedValue` on `hctx.Tx` from your handlers on `data.user.beforeCreate` and `data.user.beforeUpdate`, for the column whose values you reserve. A reserved value then fails the write as a taken one does, and sign-up answers it as it answers a taken email.
- **`column` is the column's canonical name**, and `value` is in the form the column stores.
- **It goes with its user.** `Store.DeleteUser` removes the user's reservations.
- **An expired reservation refuses nothing**, and its row stays until a purge removes it. See [Remove old rows](#remove-old-rows).
- **The table is new in core.** Generate and apply a migration before you deploy a version that has it, whether or not you use the email change flow, because deleting a user clears the user's reservations from it.

## Remove old rows

Some rows stay after they stop being of use: sessions that expired or were revoked, tokens that expired or were used, rate-limit windows that passed, expired one-time codes and reservations, and old audit events. Behemoth removes none of them by itself. Call `Store.Purge` from a scheduled job:

```go
now := time.Now()
err := ac.Store.Purge(ctx, store.PurgeOptions{
	SessionsBefore:    now.AddDate(0, 0, -30), // keep ended sessions for 30 days
	TokensBefore:      now.AddDate(0, 0, -7),  // keep ended tokens for 7 days
	AuditEventsBefore: now.AddDate(-1, 0, 0),  // keep audit events for one year
})
```

Each run removes the following:

| Table | What `Purge` removes | The purge alone |
| --- | --- | --- |
| `sessions` | The sessions that expired before `SessionsBefore`, revoked ones included | `PurgeExpiredSessions(ctx, before)` |
| `tokens` | The tokens that expired before `TokensBefore`, used and revoked ones included, and the tokens with no expiry that were consumed or revoked before it | `PurgeExpiredTokens(ctx, before)` |
| `audit_log` | The events recorded before `AuditEventsBefore` | `PurgeAuditEvents(ctx, before)` |
| `rate_limits` | The counters whose window has passed | `PurgeExpiredRateLimits(ctx)` |
| `verification_codes` | The expired codes | `PurgeExpiredVerificationCodes(ctx)` |
| `reserved_values` | The expired reservations | `PurgeExpiredReservedValues(ctx)` |

`Purge` behaves as follows:

- **A zero cutoff leaves its table alone.** Without `AuditEventsBefore`, no audit event is removed. The last three tables take no cutoff, because an expired row there already acts as a missing one, and every run purges them.
- **A cutoff later than now is taken as now.** A session or token that can still be used is never removed, and neither is a token with no expiry that was neither used nor revoked, such as an API key.
- **The cutoff sets how long a client can tell why its token stopped working.** While its row exists, a revoked session's token is refused with `session_revoked` and an expired one's with `session_expired`. Once the row is removed, the token is refused with `session_invalid`. Tokens are reported as expired, revoked, or already used until their rows go, and as not found after. A revoked session or token keeps its expiry and is removed when that expiry is before the cutoff.
- **It is safe to run from several processes at once.** Each purge deletes rows and nothing else, so a second run finds nothing to remove. Run it on a timer in every replica, or once from a job, such as a Kubernetes CronJob.
- **A purge that fails does not stop the others.** The error joins the failures, and each one names its table.
- **Rows kept in the key-value storage are not touched.** A token kind kept there, and rate-limit counters in Redis, expire in the storage.

Run it often enough that each run has few rows to remove, for example every hour. The first run on a table that has grown for a long time removes all of its old rows in one statement.

The purges of `sessions` and `tokens` read an index on `expires_at`. On a database created before these indexes existed, generate and apply a migration to add them, or call `EnsureIndexes` on MongoDB. Without them the purges work, but each one reads the whole table.

## Read core tables directly

Reading a core table through the adapter is supported, and it is the intended way to list or search: a dashboard that lists users can call `FindMany`. Two things differ from a read through the store:

- Account tokens come back sealed.
- You get a `behemoth.Model` to type-assert, and no email normalization on the value you search for. Search for `ada@example.com`, not `Ada@Example.com`.

For sessions and tokens, prefer the managers even for reads. They know about the key-value backend and cache, and the table alone may not hold the answer.

## Use the store inside a data hook

The same choices apply, with one addition: use `hctx.Tx` and `hctx.Tx.DB()`, not `hctx.Auth.Store` and `hctx.Auth.DB`, so that your writes commit or roll back with the write that fired the hook. See [Data hooks and transactions](hooks.md#data-hooks-and-transactions).

## Choose between the store and a direct write

Going through the store is the default because every step above is one that some other part of the system relies on. Write directly only when you know which steps matter for your case and handle them yourself. Examples are a migration script that backfills a column, an import that sets its own ids and timestamps, and a maintenance job that must not trigger other plugins' hooks. The steps that are hard to redo by hand are sealing account tokens and hashing session and token secrets. For those, use the store and the managers.
