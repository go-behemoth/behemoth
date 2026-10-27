# Rate limiter

This document explains how rate limiting works: how a rule becomes a check, the fixed-window algorithm, the two places counters are kept (Redis and the `rate_limits` table), how the database counter behaves when many requests hit one key at once, how old counters are removed, and what is not built.

The code lives in:

- `core/ratelimit.go`: `RateLimitAlgorithm`, `Limiter`, `LimitDecision`, `Limit`, `RateLimitRule`, `HookRateLimitRule`, `KeyByValues`, `KeyByClientIP`, `RateLimitConfig`, `AtomicIncrementer`, `RateLimiter`
- `httpapi/ratelimit.go`: `RouteRateLimitRule`, `DeclareRouteRules`, `BestRouteMatch`
- `ratelimit/fixed_window.go`: `FixedWindow`, the one algorithm
- `ratelimit/limiter.go`, `ratelimit/catalog.go`: `DefaultRateLimiter`, `NewDefaultRateLimiter`, `DefaultRateLimitCatalog`, `resolveRateLimitStore`, `dbCounterStore`
- `httpapi/router.go`: `Router.ApplyRateLimiting` and `rateLimitWrap`, which apply route rules
- `engine/init.go`: `CoreDeclareRateLimitRules`
- `store/rate_limits.go`: `IncrementRateLimit`, `PurgeExpiredRateLimits`
- `storage/adapters/redis/redis.go`: `RedisAdapter.Increment`
- `models/rate_limit.go`: the `rate_limits` table

---

## Status

| Piece | State |
|---|---|
| Rule declaration and matching (`RateLimitCatalog`, `BestRouteMatch`) | works |
| `DefaultRateLimiter.evaluate`: refusal, failure mode, one audit event per window | works |
| Fixed-window algorithm (`ratelimit.FixedWindow`) | works |
| Database counter (`Store.IncrementRateLimit`) | works; used when no key-value storage can count |
| Redis counter (`RedisAdapter.Increment`) | works; used when Redis is the configured key-value storage |
| Core rules | three hook rules per client address: sign-in and sign-up, each 10 attempts a minute per method, and session token issues, 30 a minute |
| Changing a rule by name (`RateLimitConfig.Limits`, `Disabled`) | works, for route and hook rules |
| Lockout (`ActionLockout`) | `[Not built]` the catalog rejects a rule that asks for it; see *Lockout is declared and rejected* |
| Sliding window, token bucket | `[Not built]` |
| Removal of old database counters | the method exists, nothing schedules it |

---

## How a check flows

A check passes through these steps:

1. A rule is declared at `Prepare` time: a `HookRateLimitRule` (attached to a before-phase hook point), which a plugin declares in `Declare`, or an `httpapi.RouteRateLimitRule` (matched by method and path), which a plugin returns from `httpapi.RouteRulesPlugin`. `Prepare` declares a route rule's counting part, a `RateLimitRule`, under its name (`httpapi.DeclareRouteRules`; see *A transport counts its own rules*). Each carries a `Limit{Max, Window}`, the name of an algorithm, and an action. The catalog rejects an unknown algorithm name, a limit without a positive `Max` and `Window`, and any action other than `ActionReject` (the empty value means `ActionReject`).
2. At `Boot`, `NewDefaultRateLimiter` checks the names in `RateLimitConfig.Limits` and `Disabled` against the catalog (`HasRule`), picks the counter storage (`resolveRateLimitStore`), and builds one `Limiter` per algorithm over it (`newLimiters`).
3. At request time `DefaultRateLimiter` (`CheckHookLimit`, or `CheckRule` for a route rule) skips a rule `Disabled` names, builds the key as `<rule name>:<KeyFunc result>`, for example `core.signin.address:1.2.3.4|emailpassword`, and `limiterFor` picks the rule's limiter. The limit is the one `Limits` sets for the rule, else the declared one (`limitOf`). A hook rule's `KeyFunc` also returns whether the rule applies; when it does not, the rule is skipped and the steps below don't run for it.
4. `evaluate` calls `limiter.Allow(ctx, key, limit)`, which returns a `core.LimitDecision`: whether the attempt is allowed, the retry-after, and whether it is the window's first refusal.
5. If the attempt is not allowed, it returns a `rate_limited` error with a retry-after: the time left in the window. The router answers `429` and sets `Retry-After` to that time in whole seconds, rounded up. On the window's first refusal (`FirstRefusal`) it also records a `ratelimit.exceeded` audit event (outcome `denied`, best effort) with the rule's name, the counter key, and the client address. The later refusals of the window are counted in `behemoth.ratelimit.checks` and not recorded; see *One audit event per window*. With `AuditConfig.OmitIPAndAgent` the event has neither the address nor the key, which for a rule per address is the address.
6. If `Allow` itself fails, `RateLimitConfig.FailureMode` decides: `FailOpen` (the default) lets the request through and logs a warning; `FailClosed` rejects it.

Route rules and hook rules differ in one way. For a route, the single most specific matching rule applies. For a hook point, every declared rule is evaluated and all must pass.

### A transport counts its own rules

A route rule has two halves. Matching a request to a rule, and computing what the request counts per, need the request: the method, the path, and `KeyFunc(r, ip)`. Counting needs only the rule's name, its limit and strategy, and a key. The router does the first half and the rate limiter the second:

```go
type RateLimitRule struct { // core: what the rate limiter counts with
	Name       string
	Limit      Limit
	Algorithm  RateLimitAlgorithm
	Limiter    Limiter
	Action     RateLimitAction
	LockoutFor time.Duration
	Disabled   bool
	Owner      string
}

DeclareRule(rule RateLimitRule) error                 // RateLimitCatalog
Rule(name string) (RateLimitRule, bool)               // RateLimitCatalog
CheckRule(ctx context.Context, name, key, ip string) error // RateLimiter
```

- **`httpapi.DeclareRouteRules` declares each route rule's `RateLimitRule`** (`RouteRateLimitRule.RateLimitRule`) under the same name, so names stay unique across hook and route rules, and `HasRule` and `RateLimitConfig` know every rule. `Prepare` keeps the route rules on `PreparedApp.RouteRules` for the router.
- **`rateLimitWrap` resolves the client's address, calls `KeyFunc`, and passes the rule's name, the key and the address to `CheckRule`.** `CheckRule` looks the rule up, skips it when it is declared `Disabled` or named in `RateLimitConfig.Disabled`, and evaluates it like a hook rule. An undeclared name is a configuration error.
- **The address is passed for the audit event.** A route rule runs before the authentication stage, so the request has no caller yet to read it from.

`ratelimit` holds no HTTP type, and another transport that limits calls by a rule of its own declares a `RateLimitRule` and calls `CheckRule` the same way.

### A rule names its algorithm

```go
type RouteRateLimitRule struct {
	// ...
	Algorithm RateLimitAlgorithm // "" means AlgorithmFixedWindow
	Limiter   Limiter            // optional: the rule's own, used instead
}
```

A `Limiter` needs somewhere to count, and rules are declared before any connection exists. So a rule holds a name, and `Boot` builds the limiter. `Limiter` on the rule is the way out for a strategy core does not have: it is used as it is, has to bring its own storage, and its `Limit` is not checked.

### The fixed window

`FixedWindow.Allow` is one call and one comparison:

```go
count, resetAt, err := l.counter.Increment(ctx, key, limit.Window)
// allowed while count <= limit.Max; otherwise retry after resetAt - now,
// and the attempt counted limit.Max+1 is the window's first refusal
```

With a limit of 10 a minute, a client's attempts look like this:

| Time in the window | Attempt | Count | Result | Retry-after |
| --- | --- | --- | --- | --- |
| 0s | 1st | 1 | allowed; opens the window | |
| 20s | 10th | 10 | allowed | |
| 50s | 11th | 11 | refused; the first refusal, audited | 10s |
| 55s | 12th | 12 | refused | 5s |
| 61s | 13th | 1 | allowed; opens a new window | |

- **A refused attempt is counted.** The count keeps rising, but the window ends when it was going to. A client that keeps trying is not punished with a longer wait, and does not get through earlier.
- **Retry-after is the time left**, from the `resetAt` the counter returns. It is capped at the window's length and never negative, because the counter's clock may not be this process's.
- **One attempt per key and window is the first refusal.** The database counter never returns the same count twice in a window (see *`IncrementRateLimit(ctx, key, ttl)`*), and Redis's `INCR` is atomic. So only one attempt sees `Max+1`, whichever replica counts it.

### The core rules

`CoreDeclareRateLimitRules` declares three hook rules, keyed by client address with `core.KeyByClientIP`:

| Rule | Constant | Point | Key | Limit | What it bounds |
| --- | --- | --- | --- | --- | --- |
| `core.signin.address` | `RuleSignInAddress` | `auth.signIn.before` | address and method | 10 a minute | password and code guesses from one client, per sign-in method |
| `core.signup.address` | `RuleSignUpAddress` | `auth.signUp.before` | address and method | 10 a minute | accounts created, and passwords hashed, by one client |
| `core.session_token.address` | `RuleSessionTokenAddress` | `auth.sessionToken.beforeIssue` | address | 30 a minute | session tokens issued to one client |

- **Every call of the flow counts**, whatever reached it: the route, another transport's wrapper, a plugin calling in. The address is the caller's (`identity.Peer.IPAddress`), which the authentication stage resolved with `RouterConfig.TrustedProxies`, like a session's. Code in the process has no address, and is not counted.
- **The key includes the rule's name**, so the rules count separately: a client that used up its sign-ins can still sign up.
- **Sign-in counts each method apart.** Every sign-in plugin fires `auth.signIn.before`, and each publishes its name under `hooks.HookValueMethod` first: `emailpassword`, `username`, `magiclink`, `oauth`, `phonenumber`. A client that used up its password attempts can still follow a magic link. Before, each plugin had a route rule on its own sign-in path with the same limit; one rule keyed by method keeps that, and covers a sign-in method added later. Sign-up is keyed the same way, so the sign-up inside an OAuth sign-in counts apart from the email sign-up.
- **The sign-in rule is keyed by address and not by email** on purpose. A per-account limit lets anyone lock a user out by failing on that user's email. The cost is that an attacker with many addresses is not slowed per account.
- **The plugins' limits per address follow the same pattern**, with names that end in `.address`, on their flows' before points. `username.available.route` stays a route rule: `Available` fires no hook point. The email verification send rule counts only the sends a caller asks for (`SendVerification` marks them), not the plugin's own sends for a new user, so that a busy address can't leave a new user without a link.

An application changes any rule by its name, route or hook, with `RateLimitConfig`:

```go
engine.BootConfig{
	RateLimit: core.RateLimitConfig{
		Limits:   map[string]core.Limit{engine.RuleSignInAddress: {Max: 20, Window: time.Minute}},
		Disabled: []string{engine.RuleSignUpAddress},
	},
}
```

`Boot` fails on a name no rule has and on a limit without a positive `Max` and `Window`. A disabled route rule leaves its routes without a limit, like a rule declared with `Disabled`. A route rule declared with `Disabled`, or a more specific one for the same path, still works for route rules; a hook rule can be changed only through the configuration.

### What a hook rule can key on

A route rule's `KeyFunc` gets the request and the client address. A hook rule's gets the `HookContext`, because a hook point also fires without a request: from a CLI, a job, another plugin, or another transport. That is the reason to use one. The caller is on `hctx.Ctx` (`hctx.Caller()`), with the address the transport resolved, which `core.KeyByClientIP` keys on. "Five invite tokens a minute per team" as a route rule covers one path; as a rule on `token.beforeIssue` it covers every caller of `TokenManager.Issue`.

The payload is not passed to `KeyFunc`. The firing site publishes what a rule may count per in `hctx.Values`, before its first dispatch:

| Point | Entry in `Values` | Published by |
| --- | --- | --- |
| `auth.signUp.before` | `HookValueEmail`: the email, normalized; `HookValueMethod` | `Plugin.SignUp` (`publishEmail`); `oauth.Plugin.Complete`, for the sign-up of a new user |
| `auth.signIn.before` | `HookValueEmail`: the email, normalized; `HookValueMethod`: the plugin's name | `Plugin.SignIn` (`publishEmail`), `Plugin.SignInWith` (the method and the identifier), `magiclink.Plugin.Verify` (the email, when the token names one), `oauth.Plugin.Complete`, `phonenumber.Plugin.SignInWithCode` |
| `auth.phoneNumber.beforeSend` | the number, and `purpose` | `phonenumber.Plugin` (`send`) |
| `auth.magicLink.beforeRequest` | `HookValueEmail`: the email, normalized | `magiclink.Plugin.RequestLink` |
| `auth.passwordReset.beforeRequest`, `.before` | `HookValueEmail`: the email, normalized | `emailpassword.Plugin.RequestPasswordReset`; `ResetPassword`, when the token names one |
| `auth.emailVerification.beforeSend`, `.beforeVerify` | `HookValueEmail`: the email, normalized | `emailverification.Plugin` (`sendVerification`, `Verify`) |
| `auth.emailChange.beforeRequest`, `.beforeConfirm`, `.beforeRevert` | `HookValueUserID`: the user whose address changes | `emailverification.Plugin` (`RequestChange`, `readChangeToken`) |
| `auth.oauth.beforeBegin` for a link, `auth.oauth.beforeLink`, `.beforeUnlink` | `HookValueUserID`: the user whose provider account changes; `oauth.HookValueProvider` on the link and unlink points | `oauth.Plugin.BeginLink`, `Complete` for a link (`finishLink`) and for a sign-in that links by a verified email (`linkByVerifiedEmail`), `Unlink` |
| `auth.session.beforeCreate` | `HookValueUserID`: the user's id as a string | `DefaultSessionManager.create` |
| `token.beforeIssue` | `HookValueTokenKind`, `HookValueTokenSubject` | `DefaultTokenManager.issue` |
| `auth.signOut.before` | `HookValueSessionID` | `SignOut` (it predates this, for the audit event) |
| `data.user.beforeUpdate`, `.beforeDelete` | `HookValueUserID`: the row's id | `dataHooks` (it predates this, for handlers) |

```go
ic.RateLimits.DeclareHookRateLimitRule(core.HookRateLimitRule{
	Name:    "myplugin.signin.email",
	Point:   hooks.HookSignInBefore,
	KeyFunc: core.KeyByValues(hooks.HookValueEmail),
	Limit:   core.Limit{Max: 5, Window: time.Minute},
})
```

- **The email is normalized once, at the site** (`store.NormalizeEmail`: trimmed and lowercased). `Ada@Example.com` and `ada@example.com` are one count, and no rule has to repeat the step.
- **It is the value the caller sent.** The check runs before the handlers, and the entry is not updated when a before handler rewrites the payload. A handler that maps an alias to an account does not move the attempt to the account's count.
- **The rule counts attempts.** It runs before the flow, so a sign-in that succeeds is counted like one that fails. A limit on failures only would have to count at the failed point, which is not built.
- **A nested operation inherits the entries.** The session create inside a sign-in sees the sign-in's `HookValueEmail` next to its own `HookValueUserID`, because a nested operation starts from a copy of the outer `Values`. A rule on `auth.session.beforeCreate` can key on the email when the session comes from a sign-in, and gets no email when `SessionManager.Create` is called directly.
- **Handlers see the entries too**, on every point of the operation. Nothing else reads them: the audit event takes its subject from `HookValueUserID` only as a last fallback (see `auditEvent`), and does not copy the email.
- **A per-account limit lets anyone lock a user out** by spending the account's attempts. Core's own sign-in rule stays per client address for that reason. An application that keys on the email is making that trade.

### When a hook rule does not apply

```go
KeyFunc func(hctx *HookContext) (key string, ok bool)
```

A hook rule's `KeyFunc` returns the key and whether the rule applies to the call. `CheckHookLimit` skips a rule that returns `false`: no counter is touched, the call is not limited by that rule, and the point's other rules are still checked.

| `KeyFunc` returns | What happens |
| --- | --- |
| `"ada@example.com", true` | counted under `<rule>:ada@example.com` |
| `"", true` | counted under `<rule>:`, one count for every caller. This is a limit on the operation as a whole |
| anything, `false` | skipped; `behemoth.ratelimit.checks{result="skipped"}` counts it |

Two cases need the second value. A rule per client address has no address when the flow runs from a CLI or a job (`core.KeyByClientIP` returns `false` then). A rule per email has no email when a sign-in arrives without one. Before the second value existed, both returned `""` and every such caller shared one count, so unrelated callers refused each other.

`core.KeyByValues(keys...)` builds the usual `KeyFunc`: the named `Values` entries joined with `|`, and `false` when one is missing or empty. The firing sites cooperate by not publishing an empty value. `publishEmail` removes the entry for a call without an email, and `DefaultTokenManager.issue` removes the subject entry for a token without a subject. They remove it, and don't only skip the write, because the map may hold an entry copied from an enclosing operation that belongs to another call.

A route rule's `KeyFunc` keeps its single return value. It always has a request and a resolved client address, so it has no call it does not apply to.

---

## The database counter

### Table

```go
type RateLimit struct {
	Key       string    // column limit_key, primary key
	Count     int64     // column count
	ExpiresAt time.Time // column expires_at, indexed
}
```

- One row per key. `Count` is the number of attempts in the window that ends at `ExpiresAt`.
- The key column is `limit_key`, not `key`. The SQL adapters emit column names unquoted, and `KEY` is a reserved word in MySQL and SQL Server.
- The table is always declared, even when a key-value store does the counting. See [`../models/models.md`](../models/models.md).

### `IncrementRateLimit(ctx, key, ttl)`

It counts one attempt and returns the count in the current window and the window's end (the row's `expires_at`). This is a fixed window:

- The first attempt for a key creates the row with count 1 and `expires_at = now + ttl`.
- Each later attempt inside the window adds one.
- The first attempt after `expires_at` resets the count to 1 and opens a new window.

`[Decision]` It does not use a transaction, a database-specific upsert, or `count = count + 1`. The `Database` interface has none of those in a portable form. Instead it is a compare-and-swap built on the `UpdateOne` convention, which re-checks its condition in the write itself:

1. Read the row for the key.
2. No row: insert one with count 1. If the insert fails with `DuplicateKey`, another call created it first, so start again.
3. Row found, window still open: update to `count + 1` **where the count is still the value that was read**.
4. Row found, window passed: update to count 1 and a new `expires_at` **where the count is still the value that was read and `expires_at` is still in the past**.
5. If the update matched no row, another call wrote first, so start again.

The guarantee is that no two calls return the same count for one window. The contract test runs 20 concurrent calls on SQLite and Postgres, through the raw SQL, GORM, and Bun adapters, and checks that the counts are exactly 1 to 20.

The extra `expires_at` condition in step 4 is what stops two calls from both resetting an expired window. The second one finds `expires_at` in the future again, fails its guard, re-reads, and increments to 2.

---

## Behavior on contention

Each attempt costs one read and one write, and a call that loses a race repeats both.

Contention behaves as follows:

- **Every lost race is someone else's success.** When several calls read the same count, exactly one write matches. The others retry. The counter always moves forward; no call blocks another.
- **Retries are bounded.** `rateLimitAttempts` is 32. A call gives up only after losing 32 races in a row, which needs at least that many other calls to have incremented the same key in the meantime.
- **Giving up returns an error**, not a count. The limiter treats it as a store failure, so `FailureMode` decides what happens to the request.

`[Important]` This creates a gap under the default `FailOpen`. The situation that exhausts the retries is a flood of requests on one key, which is exactly what a limit is meant to stop. With `FailOpen`, the requests that give up are allowed through. Two mitigations:

- Use `FailClosed` for rules that protect sign-in and similar endpoints.
- Prefer a key-value store with a native atomic increment for high-traffic keys. The database counter is the fallback, and it is the slower path by design: two round trips per attempt at best.

In practice the number of calls contending for one key is limited by the database connection pool, so 32 consecutive losses needs a pool and a burst both well above that size.

Other things to know:

- **Do not call it inside a transaction.** On Postgres a `DuplicateKey` error aborts the enclosing transaction, so the retry in step 2 would fail. `dbCounterStore` uses a store of its own that is never bound to a transaction.
- **The clock is the application's**, not the database's. `expires_at` is computed from the store's clock, in UTC. Application servers with skewed clocks disagree about when a window ends by the amount of the skew.
- **Fixed windows allow a burst across the boundary.** A client can make `Max` attempts at the end of one window and `Max` more at the start of the next.

---

## Removing old counters

A counter row is reused when its key is seen again: the expired window is reset in place. So a key that keeps coming back never needs cleaning up.

Rows only accumulate for keys that never return. With keys built from client IP addresses or emails, that is most of them, and the table grows without bound.

`Store.PurgeExpiredRateLimits(ctx)` deletes every row whose `expires_at` is at or before now. `expires_at` is indexed for this.

The purge follows these rules:

- It is safe to run at any time and from several processes. Deleting an expired row loses nothing: the next attempt for that key starts at 1 either way.
- It can race with an increment that is resetting the same expired row. The increment's update then matches no row, and it retries and inserts.

`Store.Purge` calls it on every run, with the store's other purges. Behemoth schedules neither: `Boot` starts no background job, and the application runs `Purge` from a cron job, a ticker started after `Boot`, or a job such as a Kubernetes CronJob. For the counters alone, an interval about as long as the longest configured window is enough. `[Not built]` A helper that runs `Purge` on an interval and stops with its context.

---

## `AtomicIncrementer`: where counters are kept

```go
type AtomicIncrementer interface {
	Increment(ctx context.Context, key string, ttl time.Duration) (count int64, resetAt time.Time, err error)
}
```

A fixed-window counter needs "add one and return the new count" as one atomic step. `KeyValueStorage` only has `Get`, `Set`, and `Delete`, and get-then-set loses updates under concurrency. `AtomicIncrementer` is that step, and `resolveRateLimitStore` picks the implementation at `Boot`:

| Configured | Counter | Cost per attempt |
| --- | --- | --- |
| a key-value storage that implements `AtomicIncrementer` (the Redis adapter) | Redis | one round trip |
| no key-value storage, or one without it | `dbCounterStore`, which calls `Store.IncrementRateLimit` | a read and a write, repeated on a lost race |

### Why it returns the window's end

A refused client is told how long to wait. With only a count, the limiter knows the window's length and not how much of it has passed, so it could only answer with the whole window: a client refused 50 seconds into a one-minute window would be told to wait 60 seconds instead of 10. Both counters already know the end (the row's `expires_at`, the key's remaining lifetime), so `Increment` returns it.

It is a time and not a duration. A duration starts going stale when it is computed; a time stays right however long the call took.

### The Redis counter

`RedisAdapter.Increment` runs one Lua script:

```lua
local count = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if count == 1 or ttl < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
	ttl = tonumber(ARGV[1])
end
return {count, ttl}
```

- **One script, so the expiry is set with the first increment.** `INCR` followed by a separate `EXPIRE` leaves a counter that never resets if the process stops between the two.
- **A counter without an expiry is given one** (`ttl < 0`). It should not exist, and if it does it would otherwise count forever.
- **`resetAt` is this process's clock plus the key's remaining lifetime.** Redis's clock and the application's don't have to agree.
- **Keys are prefixed `ratelimit:`**, apart from the session cache's `session:` keys.
- **Redis removes the key itself.** There is nothing to purge.

### Limit of the interface

`Increment(key, ttl)` can only express fixed-window counting. A sliding window or a token bucket needs more state than one counter: a script on Redis, a different table shape in the database. Those should be separate optional capabilities with their own fallbacks, not extensions of this one. For throttling sign-in, sign-up, and password reset, a fixed window is usually enough.

---

## Limits to know

These limits apply:

- **There is no lockout.** `ActionLockout` and `LockoutFor` exist on the rule types, and a rule that uses the action fails at `Prepare`. A refused key is let in again when its window ends.
- **A burst across the window boundary.** A client can make `Max` attempts at the end of one window and `Max` at the start of the next.
- **Sign-up's limit equals sign-in's.** Sign-ups are rarer than sign-ins, so a lower number would fit; 10 a minute was chosen to match. The password reset routes have rules of their own, declared by the email/password plugin when reset is on.
- **A deployment without the `rate_limits` table and without Redis is not limited.** The counter fails, and with the default `FailOpen` the request goes through with a Warn line under the `ratelimit` component. `behemoth.ratelimit.checks{result="error"}` counts it.
- **A hook rule keys on what its point publishes.** `HookRateLimitRule.KeyFunc` receives the `HookContext` and not the payload. The firing sites in *What a hook rule can key on* publish values for it in `hctx.Values`. On any other point a rule has the caller and whatever an enclosing operation published.
- **A hook rule runs after the input is decoded.** A route rule refused a request before its body was read; a hook rule on the flow's before point runs once the wrapper has decoded the body and the flow has started. A flood of requests now costs a decode each, and for the magic link and OAuth completions a token read, before it is refused. Malformed input that a flow refuses before its before point, such as a phone number that does not parse, is not counted.
- **A skipped hook rule does not limit the call.** That is the point of skipping, and it means a rule attached to a point that never publishes what it reads limits nothing. `result="skipped"` in `behemoth.ratelimit.checks` shows it.
- **An empty key from a route rule is still one shared count.** Only a hook rule's `KeyFunc` can say it does not apply.
- **A plugin can't add an algorithm by name.** `knownAlgorithms` is fixed. A rule's own `Limiter` is the extension point.
- **Old database counters are not removed** unless the application schedules `Store.Purge` or `PurgeExpiredRateLimits` (above).
- **A refusal still writes the counter.** Only the audit event is limited to one per window. With the counters in the `rate_limits` table, each refused attempt is one update of the key's row, which does not grow the table.
- **A rule's own `Limiter` decides what is audited.** One that never sets `FirstRefusal` gets no `ratelimit.exceeded` event; its refusals are still counted in the metric.
- **A lower limit can skip a window's event.** When `RateLimitConfig.Limits` lowers a rule's `Max` at a restart, a key whose count in the current window is already past the new `Max+1` gets no event until that window ends.

---

## Tests

| Test | File | Covers |
| --- | --- | --- |
| `TestFixedWindow` | `ratelimit/fixed_window_test.go` | allowed up to `Max`, refused after, only the attempt counted `Max+1` is the first refusal, retry-after from the window's end and its caps, a failing counter |
| `TestRateLimitRulesNameTheirAlgorithm` | `ratelimit/limiter_test.go` | what the catalog accepts, and `limiterFor`. `ActionLockout` and an unknown action are rejected on the rules of `DeclareRule` and on hook rules, also when the rule has its own `Limiter` |
| `TestRateLimiterMetrics` | `ratelimit/limiter_test.go` | `evaluate`: the three results, an audit event for the first refusal and none for a later one |
| `TestStoreContract/*/RateLimits` | `tests/store/contract_test.go` | the database counter on every backend: concurrent counts, the window's end, the reset |
| `TestRedisIncrement` | `tests/storage/kv_storage_test.go` | the Redis counter: concurrent counts, the expiry, the reset, a counter without an expiry |
| `TestHookRateLimitRulesKeyOnPublishedValues` | `tests/plugins/emailpassword_test.go` | hook rules under a real `Boot`, without a request: per email on sign-up and sign-in (normalized, and not moved by a handler's rewrite), per user on session create, per kind and subject on token issue. A sign-in without an email and a token without a subject are not counted |
| `TestHookRuleIsSkippedWhenItDoesNotApply` | `ratelimit/limiter_test.go` | `CheckHookLimit`: a rule whose `KeyFunc` returns `false` is skipped and counted as `skipped`, the point's other rules still run, `"", true` is counted |
| `TestKeyByValues` | `core/ratelimit_test.go` | the joined key, and `false` for a missing, nil or empty entry |
| `TestKeyByClientIP` | same | the caller's address with the named entries, and `false` for a caller without an address or a missing entry |
| `TestRateLimitConfigChangesRulesByName` | `ratelimit/limiter_test.go` | `Limits` replaces the limit of a hook rule and of a rule declared with `DeclareRule`, `Disabled` skips both, an unknown name or a limit that can't count is a configuration error |
| `TestCheckRuleCountsWhatTheTransportPasses` | same | `CheckRule`: the key under the rule's name, the transport's address in the refusal's event, a rule declared `Disabled` counts nothing, an undeclared name is a configuration error |
| `TestRateLimitRefusalOmitsTheAddressWhenConfigured` | same | with `AuditConfig.OmitIPAndAgent` the refusal's event has neither the address nor the key |
| `TestSignInRouteIsRateLimited` | `tests/plugins/emailpassword_test.go` | sign-in and sign-up through the router: the 11th attempt is a `429` with `Retry-After`, another address is not affected, the two flows count separately, a refused sign-up creates nothing, three refusals in a window are one audit event and three counts in the metric |
| `TestSignInLimitsHoldForEveryTransport` | same | sign-in called with another transport's caller is refused at the 11th attempt, another method counts apart, a system caller is not counted, `Limits` and `Disabled` through `Boot`, an unknown name fails `Boot` |

---

## Design decisions

### Fixed window first

**Context:** No algorithm counted. The limits core needs are "at most N attempts per window per key" on sign-in and similar routes.

**Options considered:**

- *Fixed window.* One counter per key. The counter existed already, tested on every database. Allows a burst across the boundary.
- *Sliding window counter.* No boundary burst. Two counters per key and a weighted sum, which is a new table shape and a Redis script.
- *Sliding window log.* Exact. One row per attempt, heavy on a database.
- *Token bucket.* A burst, then a steady rate. Fractional state and a timestamp per key, updated atomically: a Redis script and a new compare-and-swap on SQL.

**Decision:** Fixed window. For throttling password guesses, up to twice `Max` in a short span at a boundary is acceptable. `Limiter` stays an interface, so another strategy is a second implementation and a second name.

**Revisit if:** a limit has to hold exactly across a boundary, or an API-style limit needs smooth traffic.

### A rule names its algorithm; `Boot` builds the limiter

**Context:** Rules held a ready `Limiter` value and are declared during `Prepare`, before a store or key-value connection exists. A limiter that counts needs one.

**Options considered:**

- *Pass the counter to `Limiter.Allow`.* The smaller change. Every algorithm is then tied to the fixed-window counter's shape.
- *Rules name a strategy and `Boot` builds it.* `Prepare` stays free of connections, and each algorithm can take the storage it needs. Rules can't name a strategy core does not know.
- *Both a name and an optional `Limiter` on the rule.* The name for what core builds, the instance for what it doesn't.

**Decision:** The third. An empty name means the fixed window, so a rule that only sets a `Limit` works.

### A hook rule keys on `Values`, not on the payload

**Context:** `KeyFunc` receives the `HookContext`. On `auth.signIn.before` the email was in the payload only, so a rule could count per client address and not per account. An address-based key is also empty when the flow runs without a request, which is the case hook rules exist for.

**Options considered:**

- *Pass the payload to `KeyFunc`.* Every key in the payload becomes available with no code at the firing site. It changes the signatures of `KeyFunc` and `RateLimiter.CheckHookLimit`. Each rule reads untyped keys and has to normalize what it reads: two rules on sign-in could disagree on whether `Ada@Example.com` is `ada@example.com`. A renamed payload key turns a rule's key into `""` without an error.
- *The firing site publishes the values in `Values`.* No signature changes. The site normalizes once and decides what is safe to count per. A rule can only key on what a site chose to publish, so each site has a short published list to keep. The entries are visible to handlers and are copied into nested operations.

**Decision:** `Values`. It is what `SignOut` and the data hooks already did with the session id and the row id, and the published list is small: one or two entries per site. The sites covered are the ones that create something a caller can ask for repeatedly: sign-up, sign-in, session create and token issue.

**Revisit if:** rules need payload fields that sites keep having to add one by one, or a plugin's own firing sites find publishing a burden. The payload can be passed as well; the two do not exclude each other.

### A hook rule's `KeyFunc` says whether the rule applies

**Context:** The counter key is `<rule name>:<KeyFunc result>`. A `KeyFunc` that returned `""` counted every caller under one key. A hook rule hit this when it keyed on the client address and the operation ran without a request, when its point did not publish the entry it read, and when a sign-in arrived without an email. Callers with nothing in common then used up each other's attempts.

**Options considered:**

- *Treat an empty key as "skip the rule".* No signature change. It fails open without a sign: a rule meant as a limit on the whole operation returns `""` on purpose, or by mistake, and stops limiting. The two meanings of `""` can't be told apart.
- *Keep the shared count and document it.* Nothing changes. Every author has to remember to build a key that can't be empty, and a rule per address still can't be used on a point that also fires from a job.
- *A second return value.* `KeyFunc` returns `(key, ok)`. Skipping is something the author writes, and `"", true` stays a valid shared count. It changes the signature of `HookRateLimitRule.KeyFunc`; no rule existed outside tests.

**Decision:** The second value, with `KeyByValues` as the helper that returns `false` for a missing or empty entry, and firing sites that don't publish empty values. The helper matters as much as the signature: an author can still write `return email, true`, and the helper makes the correct form the short one. Route rules keep one return value because they have no such case.

**Revisit if:** a route rule needs to say it does not apply (a rule per session on a path that also serves anonymous requests, for example). The two `KeyFunc` signatures would then be made the same.

### The limits per address are hook rules

**Context:** Core's limits per client address, and each plugin's, were route rules: matched by method and path, and applied by the router. A flow called from anywhere else, such as an application's GraphQL resolver or another transport's wrapper, ran without them. A hook rule could not count per address, because it had no resolved address: only the raw request, without the router's proxy settings.

**Options considered:**

- *Keep the route rules and document that an application exposing a flow itself limits it.* No change. Every wrapper has to repeat the limit, against the guideline that a protection belongs to the operation.
- *Hook rules on the flows' before points, keyed by the caller's address.* Every call of a flow counts, however it arrived. The authentication stage puts the address on the caller first. A hook rule runs after the input is decoded, where a route rule ran before.
- *Both.* Two counts for one HTTP request, and the hook rule alone for other transports.

**Decision:** hook rules, with `core.KeyByClientIP`, under names that end in `.address`. The old route rules are removed. A flow without a before point keeps its route rule (`username.available.route`). Sign-in has one core rule keyed by address and method in place of a route rule per plugin, so each method keeps a count of its own. Hook rules are cumulative and can't be overridden by a more specific rule, so `RateLimitConfig.Limits` and `Disabled` change any rule by name.

**Revisit if:** the cost of decoding before the limit matters, such as a flood of large bodies. A route rule with a higher limit in front would bound it.

### The route half counts through `CheckRule`

**Context:** `RateLimiter.CheckRouteLimit` took a `RouteRateLimitRule` and the `*http.Request`, and resolved the client's address itself. The rate limiter's package depended on HTTP, and the router's types could not leave `core` while the rate limiter's contract named them. A second transport that wanted a limit of its own had no way to count against a named rule.

**Options considered:**

- *Keep `CheckRouteLimit`, and move the rate limiter's route half to the HTTP package.* The HTTP package would repeat the counting: the limiter per algorithm, `RateLimitConfig`, the failure mode, the metric, and the audit event.
- *A counting call that takes a rule's name and a key.* The router matches the request and computes the key, and the rate limiter counts as it does for hook rules. The rule's counting part is declared under its name, so names, `HasRule`, and `RateLimitConfig` cover every rule.

**Decision:** the second: `RateLimitRule`, `RateLimitCatalog.DeclareRule` and `Rule`, and `RateLimiter.CheckRule`. Part 2 of [`../plans/package_layout.md`](../plans/package_layout.md) moved the route rules to `httpapi`, which declares their counting part through `DeclareRule`.

**Revisit if:** a transport needs a key that the rate limiter computes, such as one that reads the counter store. Today every transport has its key before it counts.

### Lockout is declared and rejected

**Context:** `ActionLockout` was documented as blocking a key for `LockoutFor`, whatever the window does. `evaluate` only replaced the retry-after it returned with `LockoutFor`. Nothing was stored, so with a 1-minute window and a 15-minute lockout a client that ignored the header was let in after a minute. The lockout held only for clients that obeyed it. No core rule used the action.

**Options considered:**

- *Build it.* A second key per rule key, `lockout:<key>`, that lives for `LockoutFor`. It needs a "set if absent, with expiry" on both counter backends, which `AtomicIncrementer` does not have, and one more read on every attempt. Nothing needs a lockout yet.
- *Remove the action and `LockoutFor`.* Nothing can ask for what it does not get. The rule types change now and again when the lockout is built.
- *Keep the types and reject the rule.* `checkRateLimitRule` refuses `ActionLockout` at `Prepare`, so the mistake is found at startup and not in production. The rule types keep their shape.

**Decision:** Keep and reject. The same check refuses an action it does not know, which before was treated as a rejection without a word. The retry-after swap is removed from `evaluate`; its `action` and `lockoutFor` parameters stay, unread, so the call sites don't change when the lockout is built.

To build it:

1. Give the counter storage a "set if absent, with expiry": `SET key value NX PX` on Redis, and on the database an insert into `rate_limits` that treats a duplicate key as "already locked" and replaces a row that has expired. A plain set would let every refused attempt push the end of the lockout further out.
2. In `evaluate`, before `algo.Allow`: read `lockout:<key>`. If it is there, refuse with the time it has left and don't count.
3. In `evaluate`, on the window's first refusal (`LimitDecision.FirstRefusal`): set `lockout:<key>` for `lockoutFor`, if absent.
4. Remove the `ActionLockout` case from `checkRateLimitRule`, and require a positive `LockoutFor` there.

The lockout sits in `evaluate` and not in a `Limiter`, so it also covers a rule that brings its own `Limiter`. That is why the check rejects the action before it accepts such a rule.

**Revisit if:** a rule needs a penalty longer than its window. Decide the key with it: a lockout per account lets anyone lock a user out for the whole `LockoutFor`, which is worse than the same trade for a plain limit (see *What a hook rule can key on*).

### One audit event per window

**Context:** `evaluate` recorded a `ratelimit.exceeded` event for every refused attempt. With the database recorder, the default, that is one insert into `audit_log` per refused request. A client that kept sending past its limit made behemoth write at the rate it sent, which the limit was meant to prevent, and filled the table with identical rows. `behemoth.ratelimit.checks{result="limited"}` already counted every refusal.

**Options considered:**

- *Drop the event.* The metric counts refusals, but by rule only. The audit log would no longer say which address or account was limited, and when.
- *Sample the refusals.* Fewer rows, but which ones are kept is chance, and a query can't tell a limited client from one that was not sampled.
- *Record the first refusal of each window.* One row per rule, key, and window, however many attempts follow. The counter already tells: its count is `Max+1` exactly once per window, on every replica. `Limiter.Allow` returned only a boolean and a retry-after, so the answer needed a field for it.

**Decision:** Record the first refusal of each window. `Allow` returns a `core.LimitDecision` with `FirstRefusal`, which `FixedWindow` sets when the count is `Max+1`. The signature changed before any release had the interface. The lockout, when it is built, sets its key on the same signal.

**Revisit if:** an application needs the number of refusals per key, not per rule. The event could then carry the count when the window ends, which needs a sweep that nothing schedules today.

### `Increment` returns the end of the window
See *Why it returns the window's end*. The alternative kept the interface and reported the whole window as the retry-after, which tells a client to wait up to a window too long.
