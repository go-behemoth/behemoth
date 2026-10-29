# Hooks

Hooks let your code run at fixed points in Behemoth's work: before a user row is inserted, after a sign-up has finished, when a sign-in is rejected. A hook handler can inspect what is happening, change the data on its way in, stop the operation, or react once it is done.

This page covers the whole hook system: the two tiers of hooks, the three phases, how handlers are registered and ordered, what each kind of handler may do, and which hook points exist.

## Concepts

| Term | Meaning |
| --- | --- |
| Hook point | A named place where handlers run, such as `data.user.beforeCreate`. The type is `core.HookPoint`. The constants live in `core/hooks`. |
| Phase | When the point fires relative to its operation: `before`, `after`, or `failed`. A point has exactly one phase. |
| Handler | Your function. Its signature depends on the phase. |
| Chain | All handlers registered on one point, in their resolved order. |
| Tier | Which layer fires the point. Tier 1 points are fired by the data layer, Tier 2 points by a flow such as sign-up. |

## The two tiers

The tier decides what a handler may safely do, so pick the point by the tier first.

| | Tier 1: data hooks | Tier 2: flow hooks |
| --- | --- | --- |
| Example | `data.user.afterCreate` | `auth.signUp.after` |
| Fired by | the store, around a write to one table | a flow, around a whole operation |
| Fires when | every time the row is written, whatever caused it | only when that flow runs |
| Runs | inside the write's database transaction | outside any store transaction |
| After-handler error | fails the write and rolls it back | is logged; the operation still succeeds |
| Use it for | data that belongs with the row: extra columns, rows in other tables | everything else: emails, webhooks, calls to other systems |

A short way to choose: if a rollback of the database transaction would undo everything your handler did, it fits Tier 1. If not, it belongs in Tier 2.

Tier 1 also has after-commit points (`data.user.created`), which fire for every committed write and behave like Tier 2 after handlers. See [After-commit data points](#after-commit-data-points).

For example, creating a profile row for each new user fits `data.user.afterCreate`, because the profile is rolled back if the user is. Sending a welcome email does not fit there, because the user can still be rolled back after your handler returns. Send it from `auth.signUp.after`, which fires once the user is committed.

## Phases and handler signatures

```go
// before: validate or rewrite the payload, or stop the operation.
type BeforeHookFunc func(hctx *HookContext, payload behemoth.M) (behemoth.M, error)

// after: react to the result.
type AfterHookFunc func(hctx *HookContext, result any) error

// failed: be told that the operation was rejected.
type FailedHookFunc func(hctx *HookContext, reason FailureReason) error
```

### Before handlers

A before handler receives the payload as a `behemoth.M` and returns the payload to continue with. Its return value works as follows:

- Return `(payload, nil)` to continue with that payload.
- Return `(nil, nil)` to continue without changing it. A handler that only validates can end this way. To continue with an empty payload, return an empty `behemoth.M`.
- Whether a rewrite has an effect depends on the point. The [hook point reference](#hook-point-reference) lists what each before point reads back.
- Return `(nil, err)` to stop the operation. No later handler runs, and the operation does not happen. The caller receives your error unchanged, so return one of the `errs` package's types (a validation error, for example) to get a sensible HTTP status.
- Each handler receives what the previous one returned.
- A panic is caught, logged, and treated as an error that stops the operation.

```go
func requireCompanyEmail(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
	email, _ := payload[models.UserEmail].(string)
	if !strings.HasSuffix(email, "@example.com") {
		return nil, errs.NewValidationError("requireCompanyEmail", "users",
			errors.New("only example.com addresses may register"))
	}
	return payload, nil
}
```

### After handlers

An after handler receives the result of the operation. What its error does depends on the tier.

| | Tier 1 after handler | Tier 2 after handler |
| --- | --- | --- |
| The result | written, not yet committed | committed |
| Returning an error | fails the write, rolls the transaction back, is returned to the caller | is logged; the caller still gets a success |
| Later handlers after an error | do not run | still run |
| A panic | same as an error | logged; later handlers still run |

### Failed handlers

A failed handler is a notification. It fires when a flow rejects an operation for a business reason: wrong password, email already registered, a before handler said no.

```go
type FailureReason struct {
	Code  string // "invalid_credentials", "user_exists", "rejected_by_hook", ...
	Cause error  // the underlying error, or nil
}
```

A failed handler follows these rules:

- `Code` is snake case, like the error codes a client receives. A flow of your own picks its codes the same way.
- It can't change the outcome. Its error is logged and later handlers still run.
- It does not fire for system errors such as a database timeout. Those are returned to the caller as ordinary errors.
- When a before handler refuses a flow with a typed error, the flow's failed point fires with code `rejected_by_hook` (`core.FailureRejectedByHook`) and your error as `Cause`. The code is the same whichever handler refused, at any point of the flow: a refusal on `auth.signIn.credentialsVerified` is reported this way too. Read `Cause` for the reason. The audit event has your error's code as `causeCode` in its metadata.
- A handler's untyped error, or a typed one that is not a rejection (`errs.IsRejection`), counts as a system error. It stops the flow and fires no failed point.
- A flow of your own that runs a before chain reports the chain's error with `core.FailIfRejected`, which applies both rules.
- Tier 1 has no failed points.

## HookContext

Every handler receives a `*core.HookContext`.

| Field | Contents |
| --- | --- |
| `Ctx` | The `context.Context` of the operation. Pass it to anything that does I/O. |
| `Point`, `Phase` | The point being dispatched and its phase. |
| `Auth` | The `*core.AuthContext`: store, session manager, token manager, crypto, telemetry. |
| `Tx` | Tier 1 only: the store bound to the write's transaction. `Tx.DB()` is the database adapter bound to the same transaction, for your own tables. Nil on Tier 2 hooks. See [Data hooks and transactions](#data-hooks-and-transactions). |
| `Values` | A scratch map for handlers, shared by all the points of one operation. A handler can leave a note for a later handler. It is not the payload. See [Pass data between handlers](#pass-data-between-handlers). |
| `Request` | The HTTP request being handled, or nil when the operation was started outside a request (a CLI, a background job). Most handlers don't need it. |

`hctx.Caller()` returns the caller of the operation: who it runs for, and the client's address and user agent, however the operation was reached. Read the caller rather than `Request` when a handler needs to know who is calling. See [Callers and permissions](callers.md).

Some points publish well-known entries in `Values` or in the payload. The keys are constants in `core/hooks` (for example `HookValueUserID`, `HookValueSessionID`, and `HookValueIPAddress`). Use the constants so that handlers from different authors agree on the names.

### Pass data between handlers

An after handler receives the result of an operation, not its input. When a handler on a before point works something out from the input that a later handler needs, it leaves it in `hctx.Values`.

`Values` lasts for one operation: the before, after, and failed points fired for one call share the map. For a sign-up those are `auth.signUp.before`, `auth.signUp.failed`, and `auth.signUp.after`. For a write to `users` they are `data.user.beforeCreate`, `data.user.afterCreate`, and `data.user.created`.

An invite plugin accepts an `inviteCode` at sign-up and attaches the new user to the invite:

```go
func (p *InvitePlugin) Register(reg core.HookRegistry) error {
	// The code is input: it arrives in the payload.
	err := reg.OnBefore(hooks.HookSignUpBefore, func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
		code, _ := payload["inviteCode"].(string)
		invite, err := p.find(hctx.Ctx, code)
		if err != nil {
			return nil, errs.NewInvalidInputError("invite", "invite", "this invite code is not valid", err)
		}
		hctx.Values["invite.id"] = invite.ID // a note for the handlers below
		return payload, nil
	}, nil)
	if err != nil {
		return err
	}

	// The user is being written: link it to the invite in the same transaction.
	return reg.OnAfter(hooks.HookUserAfterCreate, func(hctx *core.HookContext, result any) error {
		inviteID, ok := hctx.Values["invite.id"].(string)
		if !ok {
			return nil // a user created by another path
		}
		return hctx.Tx.DB().Create(hctx.Ctx, &InviteUse{InviteID: inviteID, UserID: result.(*models.User).ID})
	}, nil)
}
```

The second handler is on another operation: the write to `users` that the sign-up makes. An operation started inside another one begins with a copy of the outer operation's `Values`:

- Its handlers see what the outer handlers left (`invite.id`).
- What its handlers write stays in the copy. A handler on `auth.signUp.after` does not see a note left by a handler on `data.user.afterCreate`.
- The copy is shallow, so a pointer stored as a value is shared.

A runnable version of this is in `examples/init`: the application registers the invite handlers in `hooks.go`, a plugin adds its own in `plugin.go`, and every handler logs the point, the phase and the keys it sees in `Values`. Run it through the HTTP route or with `go run . signup`, which calls the flows without a request.

Things to know:

- The caller of a flow does not set `Values`. It passes input, and a handler decides what to keep. This works the same for an HTTP request and for a flow called from a CLI or a job.
- Some operations start with entries of their own, such as the email of a sign-in. [What a rule can key on](#what-a-rule-can-key-on) lists them. Don't overwrite them.
- Prefix your keys with your plugin's name. All handlers of an operation share the map, and nested operations inherit it.
- A value that the operation itself should act on goes in the payload, not in `Values`. The code that fires the point never reads `Values` back.
- The `Values` of the HTTP request, `httpapi.RequestFrom(hctx.Ctx).Values`, last for the whole request and exist only when there is one. `httpapi.RequestFrom` returns nil outside a request, so check for nil before using it.
- The outer values reach a nested operation through the `context.Context`. Code that calls the store or a manager with a new context (`context.Background()`) instead of the one it was given cuts the link, and the nested handlers see an empty map.

## Register handlers

Handlers are registered on a `core.HookRegistry`:

```go
type HookRegistry interface {
	OnBefore(point HookPoint, fn BeforeHookFunc, opts *HookOptions) error
	OnAfter(point HookPoint, fn AfterHookFunc, opts *HookOptions) error
	OnFailed(point HookPoint, fn FailedHookFunc, opts *HookOptions) error
}
```

There are two places to get one, and both run during `Boot`.

An application registers its handlers with `BootConfig.Hooks`. No plugin is needed:

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	Hooks: func(reg core.HookRegistry) error {
		return reg.OnBefore(hooks.HookUserBeforeCreate, requireCompanyEmail, nil)
	},
})
```

A plugin registers its handlers in its `Register` method:

```go
func (p *LegacyPlugin) Register(reg core.HookRegistry) error {
	return reg.OnAfter(hooks.HookUserAfterCreate, p.linkLegacyAccount, nil)
}
```

Both use the same registry and the same points, and their handlers share one chain per point.

Each handler is recorded under an owner name. For a plugin it is the plugin's name. For the application it is `PrepareConfig.AppName`, which defaults to `app`:

```go
app, err := engine.Prepare(plugins, engine.PrepareConfig{AppName: "shop"})
```

The owner name is what ordering constraints and error messages use. `Prepare` rejects an `AppName` of `core`, and a plugin named `core` or named like the application.

Registration is checked, and a failed check is a boot error:

- The point must have been declared. Registering on an unknown point fails.
- The method must match the point's phase. `OnAfter` on a before point fails.
- An owner may register one handler per point. A second registration on the same point by the same plugin, or by the application, fails, whatever its priority. Put both steps in one function.
- Registration closes when `Boot` freezes the chains. Handlers can't be added or removed at runtime.

## Handler order

Handlers on one point run in an order resolved once, at boot.

```go
type HookOptions struct {
	Priority HookPriority // default: PriorityNormal
	Before   []string     // owner names this handler runs before, on this point
	After    []string     // owner names this handler runs after, on this point
}
```

1. **Priority first.** Lower values run earlier.

   | Constant | Value | Meant for |
   | --- | --- | --- |
   | `PriorityHighest` | -100 | gatekeepers that should see the raw input: validation, security checks |
   | `PriorityHigh` | -50 | |
   | `PriorityNormal` | 0 | the default |
   | `PriorityLow` | 50 | |
   | `PriorityLowest` | 100 | observers that should see the final state: logging, analytics |

2. **Then `Before` and `After`**, among handlers of the same priority. They name owners, not functions: `After: []string{"geoip"}` means "after the `geoip` plugin's handler on this point", and the application's name (`"app"` by default) names the application's handler.
3. **Then boot order**, for handlers with no constraint between them. Plugins boot in dependency order and the application comes last, so its handlers run after the plugins' unless a priority or constraint says otherwise.

Rules to know:

- A constraint that names an owner with no handler on this point is ignored.
- A constraint that contradicts priority is a boot error. A `PriorityLow` handler can't ask to run before a `PriorityHigh` one.
- A cycle (`a` before `b`, `b` before `a`) is a boot error.
- One handler per owner per point is what makes these rules work: a constraint names an owner, so it always means exactly one handler.

```go
// Run the domain check before every other handler on the point.
reg.OnBefore(hooks.HookUserBeforeCreate, requireCompanyEmail,
	&core.HookOptions{Priority: core.PriorityHighest})
```

## Data hooks and transactions

A Tier 1 hook is part of the write. The store runs these steps in one database transaction:

1. The before chain runs. It may rewrite the row or stop the write.
2. The row is written.
3. The after chain runs. It may fail the write.

An error at any step rolls the transaction back and is returned to the caller unchanged. If the caller already opened a transaction with `Store.Transaction`, the write and its hooks join that transaction.

### Write through `hctx.Tx`

`hctx.Tx` is the store bound to the transaction. Everything a data hook reads or writes should go through it. Which part of it you use depends on whose table you are touching.

| Table | Use | Why |
| --- | --- | --- |
| Behemoth's own: users, accounts, sessions, tokens | the store's methods, such as `hctx.Tx.CreateAccount` and `hctx.Tx.UpdateUser` | they apply Behemoth's rules for those tables |
| Your plugin's or application's own | the adapter: `hctx.Tx.DB()` | the store has no operations for tables it doesn't own |

A core table, through the store:

```go
func (p *LegacyPlugin) linkLegacyAccount(hctx *core.HookContext, result any) error {
	user := result.(*models.User)
	// Committed or rolled back together with the user.
	return hctx.Tx.CreateAccount(hctx.Ctx, &models.Account{
		UserID: user.ID, ProviderID: "legacy", AccountID: p.legacyID(user.Email),
	})
}
```

Your own table, through the adapter:

```go
func (p *ProfilePlugin) createProfile(hctx *core.HookContext, result any) error {
	user := result.(*models.User)
	// Profile is the plugin's own model, declared in Declare.
	return hctx.Tx.DB().Create(hctx.Ctx, &Profile{UserID: user.ID, Theme: "light"})
}
```

In both cases, return the error. It is what rolls the user back when your row can't be written.

`hctx.Tx.DB()` is a `behemoth.Database`, the same interface as `hctx.Auth.DB`, bound to the open transaction. It has the full adapter API: `Create`, `FindOne`, `FindMany`, `UpdateOne`, `DeleteOne`, and `Count`.

### Rules for the adapter

**By convention, Behemoth's tables go through the store.** Nothing enforces it, so consider what the adapter skips. The adapter writes the row and nothing else. The store's methods add all of this, and the adapter skips it:

| Skipped | What goes wrong |
| --- | --- |
| Data hooks | Updating a user through the adapter runs no `data.user.*` handler, so other plugins' checks and columns are bypassed. |
| Sealing of account tokens | OAuth tokens are stored in plaintext. Reads through the adapter return the sealed form. |
| Ids and timestamps | The row has none unless you set them. |
| Email normalization | A user stored with `Ada@Example.com` is not found by `FindUserByEmail`. |
| The column check | A misspelled column in an update is not caught by the store. |
| Session cache | A session revoked through the adapter stays valid from the key-value cache until its entry expires. Revoke through the session manager. |

Reading Behemoth's tables through the adapter is supported. A dashboard that lists users can use `FindMany` directly.

[Write Behemoth's tables from a plugin](core-tables.md) lists every skipped step per table, including what the session and token managers add.

**Pass `hctx.Ctx` to every call.** On MongoDB the transaction travels in the context, not in the adapter: a call made with another context is written outside the transaction and stays after a rollback. On the SQL adapters it is the adapter that carries the transaction. Using `hctx.Tx.DB()` together with `hctx.Ctx` is correct on every database.

**Don't keep the adapter.** It is valid until your handler returns. After that its transaction is committed or rolled back. Later calls on it fail on the SQL adapters and run outside any transaction on MongoDB. Don't store it on the plugin, and don't use it from a goroutine that outlives the handler.

**`Transaction` on it joins the open transaction.** You may call `hctx.Tx.DB().Transaction(...)`, for example from a helper that also runs outside hooks. Inside a hook it starts nothing new: your function runs in the transaction that is already open, and nothing is committed until the outer write commits. If your function returns an error, return that error from the handler too. Catching it and carrying on has different results per adapter: on the plain SQL adapters and MongoDB the inner writes stay in the transaction, while GORM and bun undo them through a savepoint.

**On Postgres, a failed statement ends the transaction.** After any statement fails, Postgres rejects every later statement in the same transaction. A handler can't try an insert, catch a duplicate-key error, and continue: the user's write is lost as well. Check first with a `FindOne`, or let the error fail the write.

**Writing outside the transaction is a choice you can make.** `hctx.Auth.DB` and `hctx.Auth.Store` use a different connection. What you write through them stays when the transaction rolls back, which is what a record of a rejected attempt needs. From inside a data hook they have limits:

- They can't see the row being written, so a foreign key from your row to the new user fails.
- On SQLite they can block on the lock the transaction holds.

### The row may still be rolled back

An after handler runs before commit. The row it receives can be rolled back after the handler returns, by a later handler's error or by a later write in the caller's transaction. Sign-up is the example: it creates the user and then the user's credential account in one transaction. `data.user.afterCreate` fires between the two, and if the account insert fails the user is rolled back.

A data hook should only do work that the rollback undoes. A rollback does not undo:

- **Key-value writes.** Anything written to `KeyValueStorage`, such as Redis, stays.
- **Writes through `hctx.Auth.Store` or `hctx.Auth.DB`.**
- **Adapter calls made with a context other than `hctx.Ctx`**, on MongoDB.
- **Outside side effects.** Emails, webhooks, and calls to other systems can't be recalled.
- **Rate-limit counters.** A rate limit attached to a before point counts the attempt even when the write is rolled back.
- **In-memory state.** `hctx.Values`, and the model the caller passed in, keep what was set on them.

### After-commit data points

`data.user.created` and `data.user.updated` fire once the transaction that wrote the row has committed. They fire for every committed write to the table, whatever caused it, and never for a write that was rolled back.

| | `data.user.afterCreate` | `data.user.created` |
| --- | --- | --- |
| Runs | inside the transaction, before commit | after the commit |
| The row | may still be rolled back | is committed |
| Handler error | fails the write and rolls it back | is logged; the write stays |
| `hctx.Tx` | the transaction's store | nil; use `hctx.Auth.Store` |
| Use it for | rows that belong with the user | reacting to a new user: emails, stats, cache updates, calls to other systems |

```go
reg.OnAfter(hooks.HookUserCreated, func(hctx *core.HookContext, result any) error {
	user := result.(*models.User)
	return mailer.SendWelcome(hctx.Ctx, user.Email)
}, nil)
```

- **They wait for the outermost transaction.** Sign-up creates the user and its credential account in one transaction. `data.user.created` fires after both are committed, and not at all if the account insert fails.
- **They run before the write returns to its caller**, in the same goroutine, in the order the writes happened. A slow handler delays the response. Start a goroutine or queue a job for slow work.
- **A handler can't fail or undo the write.** Its error and its panic are logged.
- **Delivery is best effort.** The notifications are held in memory until the commit. If the process stops between the commit and the handler, the handler never runs for that row. Most uses (a welcome email, a counter, a cache entry) can accept that.
- **For work that must not be lost, write an outbox row.** In `data.user.afterCreate`, insert a row describing the work into a table of your own through `hctx.Tx.DB()`. It commits with the user or not at all. A worker of yours reads the table, does the work, and marks the row done. Make the work safe to repeat, because a worker that stops halfway does it again.
- **Your own after-commit work.** Inside a data hook or a `Store.Transaction`, `tx.AfterCommit(ctx, fn)` queues `fn` the same way, with the same best-effort limit. `fn` runs before `Transaction` returns. If it panics, the panic is logged and the remaining callbacks still run. `Transaction` returns nil because the write is committed.

### Delete a user

`ac.Store.DeleteUser(ctx, id)` removes the user together with everything that lets someone act as that user. It runs in one transaction, in this order:

| Step | What happens | A handler can |
| --- | --- | --- |
| 1 | `data.user.beforeDelete` fires | refuse the delete by returning an error. Nothing has changed yet. |
| 2 | the user's sessions are deleted, revoked ones included | |
| 3 | the user's accounts are deleted | |
| 4 | the tokens issued to the user's id are deleted | |
| 5 | the user is deleted | |
| 6 | `data.user.afterDelete` fires, and its audit event is written | fail the delete by returning an error. Everything is rolled back. |
| 7 | the transaction commits | |
| 8 | `auth.session.afterRevoke` fires for each session that was live, with reason `user_deleted` | react. Errors are logged. |
| 9 | `data.user.deleted` fires | react. Errors are logged. |

- **The result does not depend on foreign keys.** The store deletes the rows itself. It works the same on MongoDB, which has no foreign keys, and on SQLite, which ignores them unless the connection turns them on.
- **Cached sessions are removed.** With a key-value storage configured, a session is served from the cache without a read of the database. The delete clears the user's entries, so the user's tokens stop working at once.
- **Delete your own rows in `data.user.beforeDelete`.** A plugin with a table that references users removes its rows there, through `hctx.Tx`. They go in the same transaction, before the user row, so a foreign key of yours without a cascade does not block the delete.

```go
reg.OnBefore(hooks.HookUserBeforeDelete, func(hctx *core.HookContext, row behemoth.M) (behemoth.M, error) {
	userID := hctx.Values[hooks.HookValueUserID]
	where := clause.Expression{Conditions: []clause.Condition{
		{Field: "user_id", Operator: clause.OpEqual, Value: userID},
	}}
	return nil, hctx.Tx.DB().DeleteMany(hctx.Ctx, &Membership{}, where)
}, nil)
```

- **`auth.session.beforeRevoke` does not fire.** A session can't outlive its user, so there is nothing for it to refuse. The point that can stop a delete is `data.user.beforeDelete`.
- **The session a handler gets in step 8 is not in the database.** It is the deleted row, marked revoked with the time and the reason. Sessions that were already revoked are not reported again.
- **Steps 8 and 9 are best effort**, like the other after-commit points.

What it does not remove:

- **Tokens kept in a key-value storage.** They can't be found by their subject, and expire on their own.
- **Tokens issued to something other than the user's id**, for example an email address. The plugin that issues them deletes them in `data.user.beforeDelete`.
- **Audit events.** They record what happened and keep the user's id.
- **Sessions in which the user is the impersonator.**

### Other things to know

- **A handler can run more than once on MongoDB.** The MongoDB driver retries a transaction on a transient error, and the hooks are inside it. Writes through `hctx.Tx` and `hctx.Tx.DB()` are rolled back between attempts. Anything else the handler does is repeated.
- **MongoDB needs a replica set.** A hooked write always opens a transaction, and a standalone MongoDB server has none. `Boot` returns a configuration error on a standalone server. A single-node replica set is enough: start `mongod` with `--replSet rs0` and run `rs.initiate()` once. A sharded cluster also works. [MongoDB](mongodb.md) lists what else it needs.
- **A slow handler holds the transaction open.** It keeps its locks for as long as it runs. On SQLite that blocks every other writer.
- **To react to a committed row, use the after-commit points.** See [After-commit data points](#after-commit-data-points).
- **`afterCreate` and `afterUpdate` write their audit event in the same transaction.** See [Audit](#audit).

## What each data point carries

Data points exist for the `users` table.

| Point | Phase | Payload or result | `Values` |
| --- | --- | --- | --- |
| `data.user.beforeCreate` | before | the full row, keyed by column name (for example `models.UserEmail`) | |
| `data.user.afterCreate` | after | the stored `*models.User` | |
| `data.user.beforeUpdate` | before | only the columns being changed | `HookValueUserID`: the row's id |
| `data.user.afterUpdate` | after | the stored row, read back after the update | `HookValueUserID`: the row's id |
| `data.user.created` | after, once committed | the stored `*models.User` | |
| `data.user.updated` | after, once committed | the stored row, read back after the update | `HookValueUserID`: the row's id |
| `data.user.beforeDelete` | before | the user's row. Veto only: a change to it has no effect | `HookValueUserID`: the row's id |
| `data.user.afterDelete` | after | the `*models.User` as it was before the delete | `HookValueUserID`: the row's id |
| `data.user.deleted` | after, once committed | the same | `HookValueUserID`: the row's id |

Rules for rewriting the payload:

- **Setting a column.** A before handler may set any column the table has, including columns contributed by other plugins. This is how a plugin fills its own column on create.
- **Unknown columns.** A key that is not a column of the table is a validation error and stops the write. It is not silently ignored.
- **Primary key.** `beforeUpdate` can't change the primary key, and the id is not in the payload.
- **Email normalization.** On update, the store lowercases and trims an email after the handlers run, so a handler's value is normalized too. On create it normalizes before the handlers run, so an email set by a `beforeCreate` handler is stored as given.
- **Timestamps.** On update, `updated_at` is set by the store after the handlers and can't be overridden.

## Rate limits on hook points

A rate-limit rule can be attached to a before point with `HookRateLimitRule`. The dispatcher checks it before the first handler runs.

```go
ic.RateLimits.DeclareHookRateLimitRule(core.HookRateLimitRule{
	Name:  "myplugin.profile.updates",
	Point: hooks.HookUserBeforeUpdate,
	KeyFunc: core.KeyByValues(hooks.HookValueUserID),
	Limit:   core.Limit{Max: 20, Window: time.Hour},
	Owner: "myplugin",
})
```

A hook rate-limit rule works as follows:

- `KeyFunc` gets the `HookContext`, not the payload. It keys on the entries the operation publishes in `hctx.Values` (next section) and on the caller, `hctx.Caller()`, whose address the transport resolved.
- `KeyFunc` returns the key and whether the rule applies to this call. When it returns `false`, the rule is skipped: nothing is counted and the call is not limited by it.
- `core.KeyByValues(keys...)` builds a `KeyFunc` from `Values` entries. It joins them with `|`, and returns `false` when one is missing or empty.
- `core.KeyByClientIP(keys...)` builds a `KeyFunc` from the caller's address, followed by the named `Values` entries. It returns `false` for a caller without an address, such as a CLI or a job, and when an entry is missing.
- `Limit` allows `Max` attempts per key in each `Window`. The window opens with the first attempt, and the count starts again when it has passed (a fixed window, the only algorithm).
- Every rule declared on the point is checked, and all must pass.
- A rejected attempt returns a `rate_limited` error with a retry-after. No handler runs, and the caller can't tell it apart from a before handler stopping the operation.
- Rules can only be attached to before points.
- `Action` can be left empty or set to `core.ActionReject`. Both refuse the attempt until the window ends. `core.ActionLockout` is not supported, and `Prepare` returns an error for a rule that uses it.
- The attempt is counted before the operation runs, so one that succeeds counts like one that fails.

A route rule limits an HTTP path. A hook rule limits the operation, whoever calls it: "5 invite tokens a minute per team" on `token.beforeIssue` holds for a route, a CLI command and a background job alike.

### Limit a path

A route that fires no hook point, such as a check that a username is free, is limited by a route rule. The plugin returns its route rules from `RouteRateLimitRules`, the method of `httpapi.RouteRulesPlugin`, and `Prepare` declares them under the plugin's name:

```go
func (p *Plugin) RouteRateLimitRules() []httpapi.RouteRateLimitRule {
	return []httpapi.RouteRateLimitRule{{
		Name: "myplugin.lookup.route", Method: http.MethodPost, Path: "/myplugin/lookup",
		KeyFunc: func(_ *http.Request, ip string) string { return ip },
		Limit:   core.Limit{Max: 30, Window: time.Minute},
	}}
}
```

- **`Path` is relative to `RouterConfig.BasePath`** and includes the plugin's `MountPath`. A segment `*` matches one segment, and a last segment `**` any number.
- **Of the rules that match a route, the most specific applies**, where every hook rule of a point applies. A rule declared with `Disabled` exempts its paths from a broader rule.
- **`KeyFunc` gets the request and the client's address**, resolved with `RouterConfig.TrustedProxies`.
- **The rule holds for the route only.** An operation with a before point gets a hook rule instead.

### What a rule can key on

These operations put what they were called with in `hctx.Values` before the first handler and the rate limit run:

| Point | Entry | Value |
| --- | --- | --- |
| `auth.signUp.before`, `auth.signIn.before` | `hooks.HookValueMethod` | the name of the plugin whose flow runs it: `emailpassword`, `username`, `magiclink`, `oauth`, `phonenumber` |
| `auth.signUp.before` | `hooks.HookValueEmail` | the email being registered, trimmed and lowercased |
| `auth.signIn.before` | `hooks.HookValueEmail` | the email being tried, trimmed and lowercased. For a magic link, the email the link was sent to; absent when the token is not valid |
| `auth.signIn.before`, for a sign-in by username | `username.HookValueUsername` | the username being tried, trimmed and lowercased. No email is published for these sign-ins |
| `auth.phoneNumber.beforeSend`, `.beforeVerify`, `.beforePasswordReset`, and `auth.signIn.before` for a sign-in by phone number | `phonenumber.HookValuePhoneNumber` | the phone number, in E.164 form. No email is published |
| `auth.phoneNumber.beforeSend` | `"purpose"` | what the code is for: `verify`, `sign_in`, or `password_reset` |
| `auth.signIn.before`, for a sign-in through an OAuth provider | `oauth.HookValueProvider` | the id of the provider the request names. No email is published: the provider has not answered yet. A sign-up inside such a sign-in publishes `hooks.HookValueEmail` for `auth.signUp.before`. |
| `auth.oauth.beforeBegin`, for a link | `hooks.HookValueUserID` | the user the link is for |
| `auth.oauth.beforeLink`, `auth.oauth.beforeUnlink` | `hooks.HookValueUserID`, `oauth.HookValueProvider` | the user whose provider account is linked or unlinked, and the provider's id |
| `auth.magicLink.beforeRequest` | `hooks.HookValueEmail` | the email a link is requested for, trimmed and lowercased |
| `auth.passwordReset.beforeRequest`, `.before` | `hooks.HookValueEmail` | the email a reset link is requested for, or the one the link being used was sent to; absent at the reset when the token is not valid |
| `auth.emailVerification.beforeSend`, `.beforeVerify` | `hooks.HookValueEmail` | the address being verified, trimmed and lowercased; absent at verify when the token is not valid |
| `auth.emailChange.beforeRequest`, `.beforeConfirm`, `.beforeRevert` | `hooks.HookValueUserID` | the user whose address changes; absent at confirm and revert when the token is not valid |
| `auth.signOut.before` | `hooks.HookValueSessionID` | the session being ended |
| `auth.session.beforeCreate` | `hooks.HookValueUserID` | the user the session is for, as a string |
| `token.beforeIssue` | `hooks.HookValueTokenKind`, `hooks.HookValueTokenSubject` | the kind and the subject, as strings |
| `data.user.beforeUpdate`, `data.user.beforeDelete` | `hooks.HookValueUserID` | the row's id |

A limit of five sign-in attempts a minute per account:

```go
ic.RateLimits.DeclareHookRateLimitRule(core.HookRateLimitRule{
	Name:    "myplugin.signin.email",
	Point:   hooks.HookSignInBefore,
	KeyFunc: core.KeyByValues(hooks.HookValueEmail),
	Limit:   core.Limit{Max: 5, Window: time.Minute},
})
```

A limit per client address, which leaves calls from a CLI or a job alone:

```go
KeyFunc: core.KeyByClientIP(),
```

A `KeyFunc` of your own says when the rule does not apply. This one counts per client address only the sign-ins of one method:

```go
byAddress := core.KeyByClientIP()
KeyFunc: func(hctx *core.HookContext) (string, bool) {
	if hctx.Values[hooks.HookValueMethod] != "magiclink" {
		return "", false // another method: this rule does not apply
	}
	return byAddress(hctx)
},
```

Published entries follow these rules:

- **The entry is the value the caller sent.** A before handler that rewrites the payload's email does not change it, so the attempt stays on the count of the email that was typed.
- **An operation inside another one also sees the outer entries.** A session created by a sign-in has the sign-in's email next to its own user id. A session created by a direct call to `SessionManager.Create` has no email.
- **The client address is the caller's.** A transport puts it on the context when it authenticates the call: over HTTP, the address resolved with `RouterConfig.TrustedProxies`. A call from a CLI or a job has none. Key on a published entry when the limit has to hold there too.
- **An entry without a value is not published.** A sign-in without an email has no `HookValueEmail`, and a token without a subject has no `HookValueTokenSubject`. A rule built with `KeyByValues` does not apply to those calls.
- **A key returned with `true` is counted as it is.** `return "", true` is one count shared by every caller, which is how to write a limit on the operation as a whole. Return `false` when there is nothing to count per.
- **A skipped rule is counted in the metrics**, as `behemoth.ratelimit.checks` with `result="skipped"`. A rule that is skipped on every call is attached to a point that does not publish what it reads.
- **A limit per account lets anyone use up an account's attempts** and keep its owner out for the window. The built-in sign-in limit is per client address for that reason. Weigh the two before keying on the email.

### Change a rule by its name

Every rule, behemoth's and the plugins', route and hook, has a name. `BootConfig.RateLimit` changes a rule's limit, or turns the rule off, by that name:

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	RateLimit: core.RateLimitConfig{
		Limits: map[string]core.Limit{
			engine.RuleSignInAddress: {Max: 20, Window: time.Minute},
		},
		Disabled: []string{magiclink.RuleRequestAddress},
	},
	// ...
})
```

- **The key, the point, and the algorithm stay.** Only the limit changes.
- **`Boot` fails on a name no rule has**, and on a limit without a positive `Max` and `Window`. A misspelled name is found at startup.
- **A disabled route rule leaves its routes without a limit.**

Behemoth's own rules are `engine.RuleSignInAddress` and `engine.RuleSignUpAddress`, 10 attempts a minute per client address and method, and `engine.RuleSessionTokenAddress`, 30 a minute per client address. Each plugin's page lists its rules.

### Choose what happens when counting fails

A rule counts attempts in the key-value storage of `BootConfig.KV` when it can count atomically, as the Redis adapter does, and otherwise in the `rate_limits` table. When that storage can't count an attempt, `RateLimitConfig.FailureMode` decides what happens to the request:

- **`core.FailOpen`** is the default. Behemoth logs a warning, counts the check as `result="error"` in `behemoth.ratelimit.checks`, and lets the request proceed. An outage of the storage turns every rate limit off while it lasts.
- **`core.FailClosed`** refuses the request with the same `rate_limited` error as an attempt over the limit, but with no `Retry-After` time. An outage of the storage then refuses every request that a rule covers.

```go
RateLimit: core.RateLimitConfig{FailureMode: core.FailClosed},
```

Choose `core.FailClosed` where an unprotected sign-in is worse than a refused one.

## Audit

A point can be declared with an `AuditSpec`. The dispatcher then records one audit event each time the point is dispatched, after its handlers have run. The event type defaults to the point's name. Recording differs by kind of point:

- Tier 2 after points, failed points and the after-commit data points record best effort: the operation is over, and a failed audit write is logged.
- The in-transaction after points (`data.user.afterCreate`, `data.user.afterUpdate`, `data.user.afterDelete`) write the event in the write's transaction. The event exists exactly when the row does. A failed audit write fails the write.
- Before points never record.

Core audits the after and failed points of sign-up, sign-in, sign-out, sessions and tokens, and user creation and updates. [Telemetry](telemetry.md#audit) lists the events and explains how to read them.

Two things let your own flow describe its events:

- A failed point's `FailureReason` can name what was targeted (`SubjectType`, `SubjectID`) and add detail (`Metadata`).
- An after point's result can implement `core.AuditSubject` when it is not a model.

## Declare and fire your own points

A plugin can add points of its own for other code to hook into. Declare the point in `Declare`:

```go
const HookInviteAccepted core.HookPoint = "invite.accepted"

func (p *InvitePlugin) Declare(ic *core.PluginInitContext) error {
	return ic.Hooks.Declare(core.HookPointDef{
		Point: HookInviteAccepted,
		Phase: core.AfterHookPhase,
	})
}
```

Declaration follows these rules:

- The owner is filled in with your plugin's name.
- A point can be declared once. Declaring a name that already exists is an error naming both owners.
- A point has one phase. An operation with a before, an after and a failed side declares three points.
- Declaration closes when `Prepare` returns.

Fire it through the dispatcher on the `AuthContext`:

```go
ctx = core.BeginOperation(ctx) // once per call, before the first dispatch
hctx := &core.HookContext{Ctx: ctx, Auth: ac, Values: core.HookValuesFrom(ctx)}
if err := ac.Dispatcher.RunAfter(hctx, HookInviteAccepted, invite); err != nil {
	return err // the point is not declared, or not as an after point
}
```

- `BeginOperation` gives the call its own `Values`, copied from the operation it runs inside, if any. Use the returned context for every dispatch of the call and for the store and manager calls it makes, so that all its points share `Values` and nested operations inherit them.
- The dispatcher sets `Point` and `Phase` for the handlers. You don't have to.

| Method | Use |
| --- | --- |
| `RunBefore(hctx, point, payload)` | runs the before chain; returns the rewritten payload or the first error |
| `RunAfter(hctx, point, result)` | runs a Tier 2 after chain; handler errors are logged, not returned |
| `Fail(hctx, point, reason)` | runs the failed chain; handler errors are logged, not returned. Does nothing when `point` is empty |
| `RunAfterTx(hctx, point, result)` | runs a Tier 1 after chain and returns the first error; used by the store |

Dispatching a point that was never declared, or with the wrong phase, returns a configuration error from all four methods and runs no handler. That is a mistake in the code that fires the point, not something a handler can cause, so return the error to your caller. For `RunAfter` and `Fail` it is the only error they return.

`core.WithLifecycle` wraps a function with all three points of a flow:

```go
signUp := core.WithLifecycle(ac.Dispatcher,
	hooks.HookSignUpBefore, hooks.HookSignUpAfter, hooks.HookSignUpFailed,
	signUpBody)
```

It runs the before chain on the input, calls the function with the rewritten input, and runs the after chain on the output. The whole call is one operation. Pass a `HookContext` with `Ctx`, `Auth`, and `Request`. `WithLifecycle` sets up `Values` and puts them on `hctx.Ctx` for the function to pass on. If a before handler refuses the call with a typed error, `WithLifecycle` fires the failed point with `rejected_by_hook`. A handler's system error stops the call without firing one. If the function itself returns an error, no after chain runs, and the function is expected to have fired the failed point for its own business rejections.

## Hook point reference

### Data points (Tier 1)

Fired by the store. See [What each data point carries](#what-each-data-point-carries).

| Point | Phase |
| --- | --- |
| `data.user.beforeCreate` | before |
| `data.user.afterCreate` | after |
| `data.user.beforeUpdate` | before |
| `data.user.afterUpdate` | after |
| `data.user.created` | after, once committed |
| `data.user.updated` | after, once committed |
| `data.user.beforeDelete` | before |
| `data.user.afterDelete` | after |
| `data.user.deleted` | after, once committed |

### Flow points (Tier 2)

Core declares these, so a handler can be registered on any of them.

| Point | Phase | Fired by | Payload, result or reason |
| --- | --- | --- | --- |
| `auth.signUp.before` | before | email/password sign-up | the sign-up fields from the request, including `email` and the plaintext `password`. The returned payload becomes the sign-up's input. |
| `auth.signUp.after` | after | email/password sign-up | the created `*models.User` |
| `auth.signUp.failed` | failed | email/password sign-up | codes `user_exists`, `rejected_by_hook` (a handler on `auth.signUp.before` or on a `data.user.*` create point rejected the sign-up) |
| `auth.signIn.before` | before | email/password sign-in | `email`, the plaintext `password`, and any other field of the request (the keys of `EmailAndPasswordCredentials.Extra` for a call from code). The returned payload becomes the sign-in's input. An `email` or `password` that is not a string is a validation error. A sign-in by username has `username` in place of `email`. |
| `auth.signIn.credentialsVerified` | before | sign-in, after the password check | `HookValueUserID`, and `HookValueFactor`, the factor the sign-in checked: `password`, `email`, `sms`, or `oauth` (see [What a session proved](sessions.md#what-a-session-proved)). A second-factor plugin stops the sign-in here, or sets `HookValueRequireStepUp` (`requireStepUp`) to `true` to make the session pending. No other key is read back. |
| `auth.signIn.after` | after | email/password sign-in, magic link verify | a `*core.SignInResult`: the user, the session, the raw session token, and `Method`, the name of the plugin that signed the user in (`"emailpassword"`, `"magiclink"`, `"username"`, `"phonenumber"`, `"oauth"`) |
| `auth.signIn.failed` | failed | email/password sign-in | codes `user_not_found`, `no_credential_account`, `invalid_credentials`, `rejected_by_hook` |
| `auth.signOut.before` | before | sign-out | `sessionID`. Veto only. |
| `auth.signOut.after` | after | sign-out | nil |
| `auth.sessionToken.beforeIssue` | before | session token flow | `userID`, `sessionID`, `audience`, and `claims`, an empty object. Only `claims` is read back; its keys go into the token under `custom`. See [Session tokens](session-tokens.md#hooks). |
| `auth.sessionToken.afterIssue` | after | session token flow | a `*core.SessionToken` without the token |
| `auth.sessionToken.issueFailed` | failed | session token flow | code `rejected_by_hook` |
| `auth.session.beforeCreate` | before | session manager | user id, state, IP address, user agent. The last two come from the request being handled unless the caller of `SessionManager.Create` set them, and are empty when `CaptureIPAndAgent` is off or there is no request. A handler can rewrite the IP address and the user agent (`HookValueIPAddress`, `HookValueUserAgent`), for example to mask the address. The rewrite is stored only when `CaptureIPAndAgent` is on. The user id and state are not read back. |
| `auth.session.afterCreate` | after | session manager | the created `*models.Session` |
| `auth.session.beforeRevoke` | before | session manager | session id, reason. Veto only. |
| `auth.session.afterRevoke` | after | session manager | the revoked `*models.Session`. Also fires, without `beforeRevoke`, for each live session of a deleted user, with the reason `user_deleted`. See [Delete a user](#delete-a-user). |
| `token.beforeIssue` | before | token manager | token kind and subject (`HookValueTokenKind`, `HookValueTokenSubject`). Veto only. |
| `token.afterIssue` | after | token manager | the issued token |
| `token.consumed` | after | token manager | the consumed token |
| `token.failed` | failed | token manager | the classified error code, with the error as `Cause` |

"Veto only" means a handler can stop the operation by returning an error, and a change to the payload has no effect. These payloads name what the operation acts on (a session, a token's subject), which a handler is not allowed to swap.

The session and token points are Tier 2 even though they sit next to a table write: the managers fire them outside any store transaction, `hctx.Tx` is nil, and an after handler's error is logged.

The email/password plugin (`plugins/emailpassword`) fires the `auth.signUp.*`, `auth.signIn.*`, and `auth.signOut.*` points, so they only fire when that plugin is passed to `Prepare`. Other plugins relate to them as follows:

- The magic link plugin (`plugins/magiclink`) fires the `auth.signIn.*` points too, when a link is verified, with a different `auth.signIn.before` payload: `method` and `email`, and no password. It also declares three points of its own around a link request. See [Magic link](magiclink.md#hooks).
- The username plugin (`plugins/username`) signs in through the email/password plugin, so its sign-ins fire the same points. See [Username](username.md#hooks).
- The OAuth plugin (`plugins/oauth`) fires the `auth.signIn.*` points when `Complete` finishes a sign-in, with the `auth.signIn.before` payload `method` and `provider`, and the failed codes `invalid_oauth_state`, `oauth_provider_mismatch`, `oauth_binding_mismatch`, `oauth_exchange_refused`, `user_not_found`, `email_not_provided`, `account_not_linked`, and `rejected_by_hook`. For a user it creates, it fires the `auth.signUp.*` points with the payload `method`, `provider`, `email`, `email_verified`, `firstname`, `lastname`, and `image_url`. Both payloads are veto only. It also declares nine points of its own: `auth.oauth.beforeBegin`, `.afterBegin`, and `.beginFailed` around starting a sign-in or a link, `auth.oauth.beforeLink`, `.afterLink`, and `.linkFailed` around linking a provider account, and `auth.oauth.beforeUnlink`, `.afterUnlink`, and `.unlinkFailed` around unlinking one. A sign-in that links by a verified email fires the link points inside the sign-in. See [OAuth](oauth.md#hooks).
- The phone number plugin (`plugins/phonenumber`) fires the `auth.signIn.*` points for a sign-in by code and by password, and declares nine points of its own. See [Phone number](phonenumber.md#hooks).
- With password reset on, the email/password plugin declares six `auth.passwordReset.*` points. See [Email and password](emailpassword.md#hooks).
- The email verification plugin declares six points around sending and verifying a link, and nine more for its email change flow. See [Email verification](emailverification.md#hooks).
