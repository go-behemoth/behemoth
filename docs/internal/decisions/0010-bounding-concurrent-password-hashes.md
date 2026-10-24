# 0010. Bound the password hashes that run at once in the hasher

- **Status:** Accepted
- **Date:** 2026-10-24
- **Deciders:** MastewalB
- **Affects:** `crypto` (`PasswordHasher`, `Argon2idHasher`, `Config`), `errs` (a new category), `httpapi` (its status and log level), `telemetry` (two metrics), the email/password plugin, and applications that implement `PasswordHasher` themselves

## Context and problem

An Argon2id hash with `DefaultParams` holds 64 MiB for about 33 ms. Sign-up, sign-in, and the password reset each run one, and sign-in also runs one for an unknown email, so that its answer takes as long as a wrong password's.

Nothing bounded how many ran at once. The core rate limits count per client address, so many clients, or one client with many addresses, start one hash per request in flight. The measurements in [`../plans/performance_testing.md`](../plans/performance_testing.md) found 24 hashes in flight at a peak RSS of 3.3 GiB, and 200 at 12.5 GiB of live hash memory. The process can run out of memory before any limit refuses a request.

`PasswordHasher.Hash` and `Verify` also took no context. A bound that waits needs one to stop waiting for a caller that gave up, and the hasher could not start a span under the caller's trace.

## Decision drivers

In priority order:

1. The memory that hashes hold has a bound, however the requests arrive.
2. The bound holds for every caller of the hasher: a route, a flow called from code, another plugin.
3. A refusal does not tell a caller which emails have an account.
4. The setting is plain data that a configuration file can carry, and it stays right with several replicas.
5. The cost on a sign-in that finds room at once is close to nothing.

## Options considered

### Option A: a global rate-limit rule on the hashing flows

A rule on `auth.signIn.before` and `auth.signUp.before` that counts all requests, not per address.

- **Pros:** Uses the rate limiter as it is.
- **Cons:** It bounds the rate, not the number in flight. Each check is a round trip to the counter store.
- **Fails when:** a burst arrives within one window. Every request under the count starts a hash at once.

### Option B: a limit at the reverse proxy

The deployment limits concurrent requests to the hashing routes.

- **Pros:** No code.
- **Cons:** It sits outside behemoth, and every deployment has to know to set it.
- **Fails when:** a flow is called from code, a job, or another transport. See [`../guidelines/transport_neutral_flows.md`](../guidelines/transport_neutral_flows.md).

### Option C: a semaphore in each flow that hashes

The email/password plugin bounds its own calls.

- **Pros:** The plugin can choose what to answer.
- **Cons:** Each plugin that hashes needs its own, and they don't share one budget for one process's memory.
- **Fails when:** a second plugin sets passwords. Two bounds of N allow 2N hashes.

### Option D: a semaphore shared by the replicas

A count in Redis or the database, like the rate-limit counters.

- **Pros:** One number for the deployment.
- **Cons:** A round trip on every sign-in. A slot held by a replica that dies stays taken until it expires.
- **Fails when:** the replicas differ in memory, or the load is uneven. The resource is each replica's own memory, and a shared count can't tell which replica holds it.

### Option E: a semaphore in the hasher, per process, weighted by memory

`Argon2idHasher` holds a weighted semaphore of `PasswordLimit.MaxConcurrent × Params.Memory` KiB. Each call takes the memory it hashes with and waits up to `PasswordLimit.MaxWait`, or until its context ends. A call that waits the whole `MaxWait` fails with a new error category, `unavailable`, which the router answers with `503` and `Retry-After`. `Hash` and `Verify` take a context.

- **Pros:** One bound per process for every caller. No round trip. Capacity grows with the replicas. The setting is an integer and a duration.
- **Cons:** `PasswordHasher` changes, and an application's own implementation breaks. A new error category and a new dependency (`golang.org/x/sync`). The default bound depends on `GOMAXPROCS`, which is wrong for a container without a CPU limit.
- **Fails when:** an application builds `Services` with a hasher of its own and no bound. It gets none.

### Option E with a plain count

The same, with a semaphore of `MaxConcurrent` slots, one per hash.

- **Pros:** Simpler. A hand-written channel would do.
- **Fails when:** stored hashes were made with other params. Nothing rehashes at sign-in, so after `Params.Memory` goes down, old hashes keep their larger memory, and N slots hold more than N × `Params.Memory`.

## Decision

We chose Option E, weighted by memory. The setting stays a count, `MaxConcurrent`, of hashes made with the hasher's own params. A verify of a stored hash made with more memory takes more than one slot's share, capped at the whole room. `MaxConcurrent` defaults to `GOMAXPROCS` at construction, and `MaxWait` to two seconds.

## Rationale

Only Options C, D and E bound the number in flight, which driver 1 needs, and Option C fails driver 2 as soon as a second plugin hashes. Option D protects nothing that is shared: each replica runs out of its own memory. Option E covers every caller of the hasher in one place, costs one `TryAcquire` on a call that finds room, and its setting is two plain values.

Weighting by memory keeps driver 1 true while old hashes remain, at the cost of the dependency. A count is what an operator reasons in: it maps to CPUs, and throughput gains little beyond them (41 hashes a second with 4 in flight, 68 with 24, on a 24-thread machine).

`MaxWait` is needed besides the context. Nothing in behemoth sets a request deadline, and net/http cancels a request's context only when the client goes away. Without it, requests would queue for as long as the client waits.

For driver 3, the decoy hash of an unknown email returns the hasher's error, as `Verify` does. If the decoy's error were ignored, a busy server would answer `401` for an unknown email and `503` for a known one.

## Consequences

- **Gains:** The memory held by hashes stays within `MaxConcurrent × Params.Memory` in each process. A burst queues, and what doesn't fit in `MaxWait` gets `503` with `Retry-After` before the process runs out of memory. Every caller of the hasher is traced under its own span.
- **Costs and risks we accepted:**
  - `PasswordHasher` takes a context, which breaks implementations outside this repository. `crypto.Config` has no field for another hasher, so only code that builds `Services` itself is affected.
  - A new category, `errs.CategoryUnavailable`, and a code, `server_busy`. A client sees a `503` it did not see before. The router logs it at Warn, and spans don't fail on it.
  - The bound sheds load, and is not fair. Requests from an attacker's many addresses can fill it, and real users then get `503`. The process stays up.
  - A hash that has started can't be stopped: `argon2.IDKey` takes no context.
  - The default bound follows `GOMAXPROCS`. Go 1.25 and later set it from a container's CPU limit. A container without one sees every CPU of its host, and the bound can then exceed the container's memory.
- **Follow-up work:** "The default password hash bound ignores a container's memory" and "A partial Params passes Boot" in `docs/ongoing.md`.

## Invariants

- Every password hash goes through `PasswordHasher` with the operation's context. A plugin that calls `argon2` itself bypasses the bound.
- A refused sign-in that hashes a decoy returns the decoy's error. Dropping it makes the answer under load tell which emails have an account. `TestEmailPasswordAnswersBusyTheSameForEveryEmail` checks it.
- `CategoryUnavailable` is neither a rejection (`errs.IsRejection`) nor a system failure (`telemetry.IsSystemFailure`). A refusal fires no failed point, so a lockout does not count it.

## Revisit when

- Sign-in rehashes with `NeedsRehash`, and stored params converge. A plain count would then be enough.
- The standalone service exists. Its `main` should derive `MaxConcurrent` from the container's memory limit when the file leaves it out.
- A second algorithm (bcrypt, scrypt) is added, whose cost is not mostly memory.
- Measurements show that `503`s under real load hit users before memory is at risk. The default would then be too low.

## References

- `crypto/password_hasher.go`: `PasswordLimit`, `Argon2idHasher.acquire`
- `plugins/emailpassword/email-and-password.go`: `decoyHash`
- [`../crypto/passwords.md`](../crypto/passwords.md)
- [`../plans/performance_testing.md`](../plans/performance_testing.md), *Password hashes in flight*
- [`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md)
