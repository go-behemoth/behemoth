# 0011. Order the session cache's writes by a version on the session row

- **Status:** Accepted
- **Date:** 2026-10-28
- **Deciders:** MastewalB
- **Affects:** `models` (`Session`, the `sessions` table), `store` (session updates), `auth` (`DefaultSessionManager`), `core` (a new optional capability of `KeyValueStorage`), `storage/adapters/redis`

## Context and problem

### How the session cache works today

When an application gives the session manager a `KeyValueStorage`, the manager keeps a copy of each session row in it:

- The key is `session:` followed by the token's lookup hash. The value is the row as JSON. The entry's TTL is the session's remaining lifetime (`cacheSet` in `auth/session.go`).
- `Get` reads the cache first. On a hit, it validates the token against the cached copy and doesn't touch the database. That is what makes an authenticated request free of database statements (`TestRequireSessionReadsNoMoreThanItNeeds`).
- On a miss, `Get` reads the row and writes it to the cache.
- `Create`, `Touch`, and `Promote` write the row they produced with a plain `Set`.
- `revokeModel` updates the row and then deletes the entry. `Promote` deletes the entry under the pending token. `Evict` and `Discard` delete the entries of sessions removed with their user.
- The cache fails open. When the KV is down, every lookup falls through to the database.

A write to the row and the matching write to the cache are two steps in two stores. A cache write carries a copy of the row read some time earlier. Nothing orders cache writes against each other, or against the deletes. A copy older than the row's latest change can reach the cache last, and stay there. Once it is there, a hit trusts it until its TTL runs out.

### Race 1: a revoke during a cache miss

```text
Request A: Get(token)                       Request B: Revoke(session)
------------------------------------        ------------------------------------
cache miss
FindSessionByLookupHash -> active row
verify the HMAC, check state and expiry
(a key rotation adds an UpdateSession)
                                            UpdateSession -> state = revoked
                                            Delete(cache entry)
Set(cache entry, active copy)
```

The active copy goes back in after the delete. Every later `Get` hits it and accepts the token, although the row says revoked. The entry's TTL is the session's remaining lifetime, so a revoked session can keep working for days.

The race needs a cache miss. Misses happen when Redis evicts the entry under memory pressure, after a Redis restart or failover, after a failed cache write (the manager logs it and goes on), and for every session that existed before an application added the cache. A key rotation widens the window: every session's first `Get` after a rotation writes the new hash to the row before it writes to the cache, so the window includes a database round trip.

### Race 2: a revoke during `Touch`

```text
Request A: Touch(session)                   Request B: Revoke(session)
------------------------------------        ------------------------------------
FindSessionByID -> active row
UpdateSession -> expires_at extended
                                            UpdateSession -> state = revoked
                                            Delete(cache entry)
Set(cache entry, extended active copy)
```

This race needs no cache miss. It can happen on any request where an extension is due. The stale copy has just been extended, so it validates for a full `SessionConfig.ExpiresIn`.

Both races are reproduced in `tests/store/managers_test.go`. `TestSessionRevokedDuringACacheMissIsRefused` and `TestSessionRevokedDuringATouchIsRefused` put the revoke between the manager's database step and its cache write, with a KV that runs the revoke inside the write. Both tests failed against the code before this decision was built: the row is revoked, and the next `Get` still returns the session.

### The same pattern elsewhere

The same order of events (read a copy, the row changes, the entry is deleted, the old copy is written) occurs in three more places:

- **A user is deleted.** `Store.DeleteUser` removes the user's sessions. The data hooks call `Evict` inside the transaction, and `Discard` after the commit, and both delete the entries (`dispatch/datahooks.go`). A `Get` that read the row before the delete committed, and writes to the cache after `Discard`, leaves a session in the cache whose row no longer exists.
- **A session is promoted.** `Promote` deletes the entry under the pending token. A `Get` of the pending token that read the row before the promotion can put the pending copy back. `Promote`'s guarded write keeps that copy from being promoted again, so this is minor, but the pending token validates as pending for longer than [ADR-0008](0008-renewing-the-session-token-on-promotion.md) intends.
- **A session is extended.** A `Get` that read the row before a `Touch` can overwrite the extended copy with the earlier expiry. That is not a security problem. The cache then expires a session that the row says is still valid.

### Constraints

- Behemoth runs as several replicas, each with its own clock, any of which can be killed at any moment ([`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md)).
- The store stamps `updated_at` with the clock of the process that writes, `time.Now().UTC()` by default (`store/store.go`).
- The database adapters can't express column arithmetic such as `version = version + 1` in an update. Every adapter supports a guard: a condition that `UpdateOne` checks in the write itself. `Store.UpdateUserIfEmail` relies on it, and `Store.UpdateSessionIn` did before this decision replaced it.
- `Store.update` reads the row back after the write and returns that copy.
- The KV is optional, and `KeyValueStorage` has three methods: `Get`, `Set`, and `Delete`. Applications may implement it themselves. The Redis adapter is the one Behemoth ships.
- An optional capability of a KV has a precedent: `core.AtomicIncrementer`, which the Redis adapter implements with a Lua script, and which Boot finds by type assertion.

## Decision drivers

In priority order:

1. A session that was revoked, or deleted with its user, never validates from the cache after the revoke or the delete has returned.
2. The guarantee holds with several replicas, whatever their clocks say.
3. An authenticated request whose session is cached and not due for an extension still costs no database statement.
4. Every database adapter supports the change as it is.
5. A KV that implements only `KeyValueStorage` keeps working, with a weaker guarantee that has a known bound.
6. Starts no background work, and keeps no state in one replica's memory that the guarantee depends on.

## Options considered

### Option A: leave it

Accept the races as too narrow to matter. An application that can't accept them runs without a session cache.

- **Pros:** No change.
- **Cons:** Driver 1 fails. Running without a cache fails driver 3.
- **Fails when:** an administrator revokes a stolen session while Redis is recovering from a failover. The theft goes on until the session expires, and nothing tells the administrator.

### Option B: a short lifetime for cache entries

Give each entry a TTL of its own, such as one minute, capped at the session's remaining lifetime.

- **Pros:** One-line change. It bounds every form of the race, including the ones that this record doesn't know about yet. It needs nothing from the KV.
- **Cons:** It doesn't close the races, it bounds them. A revoked session can validate for up to the TTL after the revoke. Every session costs one database read per TTL. Driver 1 fails.
- **Fails when:** the use of a stolen token within the TTL is already the harm, for example an attacker who exports data the moment the session is revoked.

### Option C: re-read the row before each cache write

`cacheSet` reads the row again and skips the write when the row is revoked or has changed.

- **Pros:** No new KV capability. No schema change.
- **Cons:** It is a check in Go followed by a separate write, so it narrows the window and doesn't close it. Every cache write costs one more read.
- **Fails when:** the revoke lands between the re-read and the `Set`. That window is one Redis round trip, smaller than before, and still open.

### Option D: a revoke marker and a set-if-absent

A revoke writes a short-lived "revoked" marker in place of the entry. `Get` on a miss writes with a set-if-absent (Redis `SET NX`), so the marker wins over a stale copy. This needs a new optional capability on the KV.

- **Pros:** It closes race 1. The capability is a single Redis command.
- **Cons:** It doesn't fit `Touch`. The key normally holds the active copy, so a set-if-absent never writes there. `Touch` either keeps its plain `Set`, which leaves race 2 open, or deletes the entry and lets the next `Get` refill it, which costs one read per extension. A delete can also remove a marker. The race on the expiry (a stale `Get` over `Touch`) stays.
- **Fails when:** with `Touch` deleting, three operations interleave. A `Get` reads the active row, a revoke writes its marker, and `Touch` deletes the entry and the marker with it. The `Get`'s set-if-absent then finds the key empty, and the active copy goes in.

### Option E: delete again after a delay

A revoke deletes the entry a second time, a few seconds later, to remove whatever a writer in flight put back.

- **Pros:** No new KV capability. No schema change.
- **Cons:** Each revoke starts a timer, which is background work that can't be stopped, and which disappears with a replica that is killed (driver 6). The delay is a guess.
- **Fails when:** a writer is slower than the delay, after a garbage collection pause or a slow Redis call, or the replica that holds the timer stops before it fires.

### Option F: a lock per session

Take a lock in Redis around each sequence of a row read and a cache write, and around each revoke.

- **Pros:** It closes every form of the race while the lock holds.
- **Cons:** Each cache miss and each `Touch` costs a lock and an unlock. A lock in Redis needs a TTL, so that a killed replica doesn't hold it forever. A lock that expires while its holder is paused allows the race again. A KV without locks gets nothing.
- **Fails when:** the holder pauses longer than the lock's TTL, which is the case that locks with a TTL are known for.

### Option G: order the writes by `updated_at`

Each cache write carries the row's `updated_at`, and the KV keeps the newest. This needs the same KV capability as option H.

- **Pros:** No schema change. The column exists and the store stamps it on every update.
- **Cons:** Each replica stamps `updated_at` with its own clock (driver 2). Two writes can carry the same timestamp, depending on the column's precision in each database, and a tie has no winner.
- **Fails when:** replica B's clock is two seconds behind replica A's. `Touch` on A writes at real time *t*. A revoke on B at *t* + 1 s gets a timestamp of *t* - 1 s. The KV keeps the extended active copy, because it looks newer.

### Option H: order the writes by a version column

Add an integer `version` to the session row. Every update of the row increments it, in the guarded write itself. Each cache write carries the version of the copy it writes, and the KV writes only a version newer than the one it holds. Ending a session writes a newer value instead of deleting the entry.

- **Pros:** It closes both races and the three related cases. The database orders the versions, so clocks don't matter. Every adapter already supports the guard. A hit costs no statement, as before.
- **Cons:** A new column and a migration. A new optional KV capability, with a Lua script in the Redis adapter. Every session update becomes a guarded write that can lose to a concurrent one, so each writer needs a rule for a conflict. A KV without the capability gets only the bound of option B.
- **Fails when:** a session write doesn't go through the versioned update. Its change then shares a version with the copy before it, and the KV can keep the older copy. The [Invariants](#invariants) section records this.

## Decision

We chose option H. A KV without the new capability falls back to option B, a short cap on the lifetime of each entry.

### The version

The `sessions` table gains `version BIGINT NOT NULL DEFAULT 0`. The store's session updates become one guarded update, which replaces `UpdateSession` and `UpdateSessionIn`, so that no update of a session can leave the version out:

```go
// UpdateSessionAt applies changes to the session with id if the session is
// still at version when the change is written. It writes version+1. A
// session at another version gets a NotFound error, as a missing one does.
func (s *Store) UpdateSessionAt(ctx context.Context, id string, version int64, changes behemoth.M) (*models.Session, error)
```

The guard is the condition `version = n`, checked by `UpdateOne` in the write, so of two writers that read version *n*, one writes *n* + 1 and the other gets NotFound. The store sets the new version after the `BeforeUpdate` hooks, and refuses changes that name the column, so neither a caller nor a hook can write another version.

A row stored before the column existed is at version 0. An SQL migration gives it the column's default. A MongoDB document has no such field, and the model reads it as 0, so the guard for version 0 is `version = 0 OR version IS NULL`, which matches the missing field too. Without it, a session stored before the upgrade could never be extended, promoted, or revoked on MongoDB.

Every session update in `auth` goes through `UpdateSessionAt`, and each writer has a rule for the conflict:

| Writer | Guard | On a conflict |
| --- | --- | --- |
| `Touch` | version | Gives up. Another request has extended or revoked the session. The next request decides again. |
| Key rotation in `get` | version | Gives up. The next `Get` rotates. |
| `Promote` | version, which pins the pending state the session was read in | Re-reads. Fails with `CodeInvalidSessionState` when the session is no longer pending, and retries otherwise. |
| `revokeModel` | version | Re-reads and retries. When the re-read finds the session revoked, another revoke won: this one returns without firing `auth.session.afterRevoke`, which the other one fires. |

A promotion or a revoke that loses five times in a row returns a retryable internal error. Only a session under constant concurrent updates loses that often.

The version is read together with the rest of the row, so a copy's version always belongs to its contents. This holds whatever the re-read in `Store.update` returns: if a later write has landed, the copy is newer, and its version says so.

### The capability

Comparing versions in Go and then calling `Set` is a check followed by a separate write, which is option C again. The comparison has to happen inside the KV. A new optional capability does it, found by type assertion as `core.AtomicIncrementer` is:

```go
// VersionedCache is an optional capability of a KeyValueStorage: values
// that a write of an older version never replaces.
type VersionedCache interface {
	// SetIfNewer writes value under key with version, as one atomic step,
	// unless key holds a value of the same or a later version. It reports
	// whether it wrote.
	SetIfNewer(ctx context.Context, key, value string, version int64, ttl time.Duration) (bool, error)

	// GetVersioned returns the value under key and its version.
	GetVersioned(ctx context.Context, key string) (value string, version int64, err error)
}
```

The Redis adapter keeps each value in a hash, next to its version, and `SetIfNewer` is a Lua script that compares the versions, writes, and sets the expiry in one step. A write of the same version is refused, because it carries the same contents. A key that holds a value the plain `Set` wrote has no version, and is replaced: during a rolling deploy, an entry an old replica wrote gives way to the next versioned write.

### No deletes

A delete forgets the version, and after it any older copy counts as newer than nothing. So the paths that end a session write a newer value instead of deleting the entry. One delete stays, in `Evict`, for the reason given after the table:

| Path | Today | With versions |
| --- | --- | --- |
| `get` on a miss | `Set(row)` | `SetIfNewer(row, row.Version)` |
| `Touch` | `Set(touched)` | `SetIfNewer(touched, touched.Version)` |
| Key rotation in `get` | caches the row it read | `SetIfNewer` of the row its update returned |
| `Create` | `Set(row)` | `SetIfNewer(row, row.Version)` |
| `Promote`, new token | `Set(promoted)` | `SetIfNewer(promoted, promoted.Version)` |
| `Promote`, pending token | `Delete` | a "gone" value at the promoted row's version |
| `revokeModel` | `Delete` | `SetIfNewer(revoked, revoked.Version)` |
| `Evict`, in the delete's transaction | `Delete` | `Delete`, unchanged |
| `Discard`, after the commit | `Delete` | a "gone" value at the largest version |

A cached revoked row is safe: `get` already refuses a revoked session, from the cache as from the database. It also saves the database read for a revoked token that clients keep sending. The comment in `revokeModel` that calls a delete the one correct choice reverses: a delete is now the wrong one.

A "gone" value is a fixed value that `fetchByLookupHash` reads as NotFound. Deletion is final, so the gone value of a deleted session takes the largest version, which nothing can replace. A promotion moves the session to a new token, so the gone value under the pending token takes the promoted row's version.

`Evict` runs inside `Store.DeleteUser`'s transaction, before the commit. A gone value written there would refuse the sessions of a delete that rolls back, for as long as the gone value lives, although their rows are still there. A delete is harmless in that case: the next `Get` reads the row and caches it again. `Discard` runs after the commit and writes the gone values, which replace whatever a request wrote after `Evict`'s delete. Without `Evict`, a process that stops between the commit and `Discard` would leave every cached copy of the deleted sessions valid for its TTL.

The lifetime of an entry that ends a session must outlast every older copy that could still be valid:

- A revoked row keeps the session's remaining lifetime as its TTL, as every copy does today. No older copy expires later: only `Touch` moves `expires_at`, and only forward, so a revoked row's expiry is at least that of every copy before it.
- The gone value of a deleted session lives for the longer of `SessionConfig.ExpiresIn` and `PendingExpiresIn`. The sessions that `Discard` receives were read before the delete, and a `Touch` in between can have extended them. No copy expires later than a full lifetime, active or pending, after it was written. The two settings give the same value unless a configuration makes a pending session outlive an active one.
- The gone value under a promoted session's pending token lives for the pending session's remaining life, as the promotion read it. No older copy of the pending session expires later, for the reason given for a revoked row.

### How this closes the races

Race 1, with versions:

```text
Request A: Get(token)                       Request B: Revoke(session)
------------------------------------        ------------------------------------
cache miss
read the active row, version n
                                            guarded update -> revoked, version n+1
                                            SetIfNewer(revoked copy, n+1)  -> written
SetIfNewer(active copy, n)  -> refused
```

Race 2, with versions:

```text
Request A: Touch(session)                   Request B: Revoke(session)
------------------------------------        ------------------------------------
read the active row, version n
guarded update -> extended, version n+1
                                            read the extended row, version n+1
                                            guarded update -> revoked, version n+2
                                            SetIfNewer(revoked copy, n+2)  -> written
SetIfNewer(extended copy, n+1)  -> refused
```

The writes can reach the KV in either order: the copy with the later version stays. If the revoke reads the row before `Touch` writes it, both guards name version *n*, and one of the two updates gets NotFound. A `Touch` that loses gives up. A revoke that loses re-reads and retries.

The related cases close the same way. After a user is deleted, a stale `Get` meets the gone value `Discard` wrote, of the largest version. After a promotion, a stale `Get` of the pending token meets a gone value of a later version. A stale `Get` that read the row before a `Touch` carries an older version than the extended copy and is refused.

### A KV without the capability

When the KV doesn't implement `VersionedCache`, the manager keeps today's `Set` and `Delete`. It caps every entry's TTL at `SessionConfig.UnversionedCacheTTL`, one minute by default, which bounds the races as option B does. The cap is a setting, so that an application can choose it.

## Rationale

Option H is the only one that meets drivers 1 and 2 together. The others lose for these reasons:

- **Option A** accepts a gap that driver 1 rules out, and the gap appears when an administrator relies on a revoke most, during an incident.
- **Option B** bounds the races and doesn't close them. It stays as the fallback for a KV without the capability, because it needs nothing from the KV.
- **Option C** moves the window and doesn't close it.
- **Option D** closes race 1 and leaves race 2 open, or trades it for a three-way race. It also doesn't fix the expiry race. It needs a new capability as option H does, and gives less for it.
- **Option E** depends on timing and starts work that can't be stopped, which driver 6 rules out.
- **Option F** costs two more round trips on every miss and every extension, and it reopens the race when a lock expires during a pause.
- **Option G** is option H with a clock in place of a counter, and the clocks of several replicas don't agree (driver 2).

Option H puts the order where it already exists. The database serializes the writes to a row, and the guard records that order in the row. The KV keeps the order with one comparison. A hit still costs no database statement (driver 3), and the guard is a feature that every adapter has (driver 4).

## Consequences

- **Gains:**
  - A revoked or deleted session never comes back in the cache, whatever the timing, the clocks, or the number of replicas.
  - The expiry in the cache never moves backward.
  - Concurrent updates of one session row are detected. Today a `Touch` can extend a row that a revoke has just ended.
  - A revoked token that clients keep sending is refused from the cache, without a database read.
- **Costs and risks we accepted:**
  - The `sessions` table gains a column, and applications run a migration. The new code needs the column, so the migration runs before the deploy.
  - `Store.UpdateSession` and `Store.UpdateSessionIn` are removed, which breaks code that calls them. `UpdateSessionAt` takes the version the caller read.
  - During a rolling deploy, old replicas update rows without the guard and delete entries. Until the last old replica stops, the guarantees are today's.
  - Every session update can lose to a concurrent one. Each writer has a conflict rule, and the revoke has a retry loop.
  - The Redis adapter gains a Lua script, and an application with its own KV gets only the bound of the fallback until it implements the capability.
  - Revoked sessions and gone values stay in the cache until their TTL runs out, where today they leave at once. That is one entry per ended session, for at most the longer of `ExpiresIn` and `PendingExpiresIn`.
  - A new setting, `SessionConfig.UnversionedCacheTTL`, caps the TTL of entries in a KV without the capability.
- **Follow-up work:** the decision was built in four steps, all done:
  1. The `version` column, its migration, and `Store.UpdateSessionAt`, with its contract tests on every database adapter.
  2. `VersionedCache` in `core`, its Lua implementation in the Redis adapter with a test against Redis, and an implementation in the test `memoryKV`.
  3. The session manager changes in the tables above, and the fallback cap. The two tests named in [Race 2](#race-2-a-revoke-during-touch) pass since this step.
  4. Tests for the related cases: a stale `Get` after a user is deleted, after a promotion, and after a `Touch`.

## Invariants

- **Every update of a session row increments its version, in the guarded write itself.** A write that skips it leaves its change at the version of the copy before it. The KV then refuses to cache the change, or keeps an older copy at the same version.
- **A cached copy's version is the version of the contents it holds.** The two are read from the row together. A copy that pairs new contents with an old version, or the reverse, makes the KV keep the wrong one.
- **While the capability is in use, only `Evict` deletes a session's cache entry, and `Discard` follows it with a gone value after the commit.** Ending a session writes a newer value. A delete forgets the version, and an older copy written after it wins, so a new delete anywhere else reopens the race. After `Evict`'s delete, an older copy can win only until `Discard` writes the gone value.
- **An entry that ends a session outlives every older copy that could still be valid.** If it expires first, an older copy written later finds the key empty and goes in.
- **`expires_at` only moves forward between versions under one lookup hash.** The TTLs of a revoked row and of the gone value under a pending token depend on it. A future write that shortens a session's life must give those entries a TTL of their own.

## Revisit when

- Session rows are updated outside `auth/session.go`, for example a bulk revoke in the store with one statement for many rows. Each row still needs its own increment.
- A database adapter can't check a guard in the write itself.
- Applications commonly bring a KV without the capability, so that most deployments get only the fallback.
- Conflicts on session updates show up as measurable retries, for example from many requests extending one session at once.
- The cache moves into each replica's memory, where every replica has its own copy and a write in one doesn't reach the others.

## References

- `auth/session.go`: `DefaultSessionManager.get`, `Touch`, `Promote`, `revokeModel`, `writeRevoked`, `Evict`, `Discard`, `fetchByLookupHash`, `cacheSet`, `cacheRevoked`, `cacheGone`, and `cacheDelete`
- `store/sessions.go`: `UpdateSessionAt` and its guard, `sessionAt`
- `store/create.go`: `Store.update`, which checks the guard in the write and reads the row back
- `core/session.go`: `VersionedCache`
- `core/auth-options.go`: `SessionConfig.UnversionedCacheTTL`
- `storage/adapters/redis/redis.go`: `SetIfNewer` and `GetVersioned`
- `dispatch/datahooks.go`: `Evict` and `Discard` around the deletion of a user
- `core/ratelimit.go`: `AtomicIncrementer`, the precedent for an optional KV capability
- `tests/store/managers_test.go`: `TestSessionRevokedDuringACacheMissIsRefused`, `TestSessionRevokedDuringATouchIsRefused`, `TestSessionPromotedDuringACacheMissStaysGone`, `TestSessionExtendedDuringACacheMissKeepsItsExpiry`, `TestSessionUpdatesAreVersioned`, `TestSessionRevokeRetriesAfterAConcurrentUpdate`, `TestSessionPromotionRetriesAfterAConcurrentUpdate`, `TestSessionCacheWithoutVersions`, and `TestSessionCacheOnRedis`
- `tests/store/store_test.go`: `TestDeletedUsersSessionStaysOutOfTheCache`
- `tests/store/contract_test.go`: `TestSessionWithoutAVersionOnMongo`
- `tests/storage/kv_storage_test.go`: `TestRedisVersionedCache`
- [ADR-0008](0008-renewing-the-session-token-on-promotion.md): the promotion that moves a session to a new token
- [`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md): several replicas, and no background work that can't be stopped
