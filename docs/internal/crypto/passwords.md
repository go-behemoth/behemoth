# Password hashing

This document covers the password hasher: how a hash is made and checked, and how the hasher bounds the hashes that run at once. The pepper's key comes from the key manager, which [`keys.md`](keys.md) covers. [ADR-0010](../decisions/0010-bounding-concurrent-password-hashes.md) records why the bound is built this way.

## Where it sits

| Piece | File | Role |
| --- | --- | --- |
| `PasswordHasher` | `crypto/contracts.go` | the contract: `Hash(ctx, password)`, `Verify(ctx, hash, password)`, `NeedsRehash(hash)` |
| `Argon2idHasher`, `Params`, `DefaultParams`, `PasswordLimit` | `crypto/password_hasher.go` | Argon2id with a pepper, and the bound |
| `New` | `crypto/config.go` | builds the hasher from `Config.Password` and `Config.PasswordLimit`, as `Services.Passwords` |
| `hashPassword`, `decoyHash`, `signInBody` | `plugins/emailpassword/email-and-password.go` | the one caller: sign-up, sign-in, and the reset |

## The hash

`Hash` takes the current version's `password_hash` key, computes HMAC-SHA256 of the password with it (the pepper), and runs Argon2id on the result with `Params` and a random salt. The result is a PHC string:

```
$argon2id$v=19$m=65536,t=3,p=4,kv=1$<salt>$<key>
```

`kv` is the pepper's version, so `Verify` takes the key of the version the hash was made with. A hash without `kv` is read as version 0. `Verify` runs Argon2id with the params in the string, not the hasher's, and compares in constant time.

### Params

`Config.Password` is a `*Params`. `Params.withDefaults` fills each zero field from `DefaultParams`, so a configuration that sets only `Memory` is complete, as [`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md) asks of every option. The hasher keeps that copy, not the caller's struct. `New` and `NewArgon2idHasher` refuse these values with a configuration error:

| Value | Why |
| --- | --- |
| `SaltLength` under 16 bytes | RFC 9106 recommends a 128-bit salt for passwords. Until 2026-10-24 a zero salt length hashed with an empty salt, so equal passwords had equal hashes under one pepper version. |
| `KeyLength` under 16 bytes | A key under 128 bits. A zero key length panicked inside `argon2` at the first hash. |
| `Memory` under 8 KiB times `Parallelism` | The least Argon2id allows. `argon2` raises a smaller value silently, so the hash string would record less memory than ran, and the bound would count less. |

A zero `Iterations` also panicked at the first hash, and now takes the default.

`Memory` and `Iterations` have no floor above Argon2id's own. They trade memory for passes (OWASP lists several equivalent pairs), and the tests run with 1 MiB and one pass.

The floors apply to the hasher's own params. `Verify` runs with the params written in the stored hash, so a hash made before a change still verifies.

### Rehashing

`NeedsRehash` reports a hash made with weaker params or an older pepper. `[Not built]` Sign-in does not call it, so a stored hash keeps its params until the user sets a new password.

## The bound on hashes in flight

Each running hash holds `Params.Memory`. `Argon2idHasher` holds a weighted semaphore (`golang.org/x/sync/semaphore`) of `MaxConcurrent × Params.Memory` KiB, the room. Every call takes the memory it hashes with before it starts Argon2id, and gives it back when Argon2id returns:

| Call | Takes |
| --- | --- |
| `Hash` | `Params.Memory` |
| `Verify` | the `m` of the stored hash, so a hash made with older, larger params counts for more |
| either, when that is more than the room | the whole room, so it runs alone and doesn't wait forever |

`acquire` tries `TryAcquire` first. A call that finds room pays no timer and no context. Otherwise it waits under a context that ends at `MaxWait` or with the caller's context, whichever comes first:

| How the wait ends | Result |
| --- | --- |
| room is given back | the hash runs |
| `MaxWait` passes | `errs.NewUnavailable` with `server_busy` and `RetryAfter = MaxWait`; one `behemoth.password.busy` |
| the caller's context ends | the context's error, wrapped. Nobody is left to read a `503`. |

The semaphore serves waiters in order, so a verify of a large stored hash is not overtaken by small hashes indefinitely. Every call records its wait in `behemoth.password.wait`, zero when it found room at once.

`deriveKey` gives the room back with a `defer`. `argon2.IDKey` panics on some params, such as zero iterations. The hasher's own params can't have them, but a stored hash edited in the database can, and a panic does not keep the room.

`[Limit]` A hash that has started runs to the end. `argon2.IDKey` takes no context.

### Defaults

`MaxConcurrent` is `runtime.GOMAXPROCS(0)` when the hasher is built. Throughput gains little past the CPUs: on a 24-thread machine, 41 hashes a second with 4 in flight and 68 with 24. Since Go 1.25, `GOMAXPROCS` follows a container's CPU limit.

`MaxWait` is two seconds. Nothing in behemoth sets a request deadline, and net/http cancels a request's context only when the client goes away, so the context alone would let requests queue for as long as clients wait.

`[Limit]` A container without a CPU limit sees every CPU of its host, and the default can then need more memory than the container has. See "The default password hash bound ignores a container's memory" in `docs/ongoing.md`.

A negative field is a configuration error. `New` checks it before the key manager starts its watch, which would otherwise outlive a failed `New` until its context ends.

### The error

`errs.CategoryUnavailable` is for work the system has no room for now. It maps to `503`, and `Retry-After` comes from `RetryAfter` as for a rate limit.

It is neither a rejection nor a system failure:

- `errs.IsRejection` is false, so a flow does not fire a failed point for it, and a lockout built on `auth.signIn.failed` does not count it.
- `telemetry.IsSystemFailure` is false, so a span sets `error_code` and `error_category` and is not failed.
- The router logs it at Warn as `request refused: no capacity`, not at Error. Under a burst there is one line per refused request.

### Spans

`Hash` and `Verify` start `behemoth.password.hash` and `.verify` from the caller's context, so every caller is traced under its own span. A span covers the wait and the hash. `crypto.New` passes the hasher the `Telemetry` that `Boot` gives it.

### Decoy hashes

Sign-in hashes the attempted password and throws the hash away when it refuses before checking one: an unknown email, or a user without a credential account. Sign-up does the same for a taken email. The decoy's error is returned like `Verify`'s; see *The password hashes* in [`../plugins/emailpassword.md`](../plugins/emailpassword.md).

## Invariants

- Every password hash goes through `PasswordHasher` with the operation's context. Code that calls `argon2` itself is not counted in the room.
- A decoy hash's error is returned. Dropped, a busy server answers `401` for an unknown email and `503` for a known one.
- The room is per process. Replicas don't share it, and each one's memory is bounded on its own.
