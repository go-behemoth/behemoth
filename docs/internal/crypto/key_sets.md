# Key sets and JWT verification

A JWK set (RFC 7517) is how an issuer publishes the public keys its tokens are verified with. Behemoth publishes one for its session tokens, and reads the sets of OpenID Connect providers. This document covers the codec in `crypto` that reads and writes a set, the sources that fetch one, the key set that caches a source, and `VerifyJWT`, which checks a token against a key set. [ADR-0003](../decisions/0003-remote-jwt-verification-and-outbound-calls.md) is the decision behind the split (Proposed, 2026-10-14). The API guide is [`../../api/jwt-verification.md`](../../api/jwt-verification.md).

## Where it sits

| Piece | File | Role |
| --- | --- | --- |
| `ParseJWKSet`, `MarshalJWKSet` | `crypto/jwk.go` | the codec: a set into `[]crypto.PublishedKey`, and back. No I/O. |
| `algorithmsOf`, `checkRSA`, `curveNamed`, `curveName` | `crypto/jwk.go` | which algorithms a key can sign with, and which keys the codec accepts |
| `KeySetRoutes` | `httpapi/key_set.go` | the key set route of session tokens, written with `MarshalJWKSet` |
| `keysource.JWKSet` | `keysource/keysource.go` | a source: fetches a set at a URL through an `outbound` client, and reads it with `ParseJWKSet` |
| `oidcProvider.document`, `verifyIDToken` | `plugins/oauth/oidc.go` | the OAuth plugin's key set, a `CachedKeySet` over a `keysource.JWKSet`, and its ID tokens, checked with `VerifyJWT` and then the OpenID Connect rules |
| `KeySource` | `crypto/key_set.go` | what a source is: `Keys(ctx)`, the keys as they are now |
| `CachedKeySet`, `NewCachedKeySet`, `CacheOptions` | `crypto/key_set.go` | a `KeySet` over a `KeySource` |
| `VerifyJWT`, `VerifyOptions`, `VerifiedJWT` | `crypto/verify.go` | the check of a token signed with an asymmetric key of a `KeySet` |
| `candidateKeys`, `fits` | `crypto/verify.go` | which keys of the set may have signed a token |
| `externalJWT.verify` | `crypto/jwt.go` | Behemoth's external tokens, checked with `VerifyJWT` against the derived key set |
| `KeySet`, `ExternalKeySet` | `crypto/contracts.go` | a set of public keys, and the one that also signs Behemoth's external tokens |
| `crypto.PublishedKey` | `crypto/contracts.go` | one key: its kid, its algorithm, and the public key |

`crypto` makes no outbound call. A set at a URL reaches it through a source, so the codec is tested without a server, and a source can be any protocol.

## What `ParseJWKSet` keeps

RFC 7517, section 5, advises ignoring a key that can't be used, and keeping the rest of the set. Each key is decoded on its own from a `json.RawMessage`, so a malformed key (a member of the wrong type, a value that is not base64url) does not fail the set.

| Key | Kept when | Algorithm when the key names none |
| --- | --- | --- |
| `RSA` | the modulus has at least 2048 bits, and the exponent is odd, at least 3, and at most 2^31-1 | empty: an RSA key can sign with RS256, RS384, RS512, PS256, PS384, and PS512 |
| `EC` | the curve is P-256, P-384, or P-521, and the point is on it (`ecdsa.ParseUncompressedPublicKey`) | ES256, ES384, or ES512, by the curve |
| `OKP` | the curve is Ed25519, and `x` has 32 bytes. The point's order is not checked (`docs/ongoing.md`, M37). | EdDSA |

Every key is also left out when:

- its `use` is present and not `sig`, or its `key_ops` are present and lack `verify`
- it names an `alg` it can't sign with, `none` and the HMAC algorithms among them
- it carries a `d` member. A set that publishes a private key can't be trusted with it.
- its type is another, `oct` among them: a symmetric key never verifies a token, so a public key is never used as an HMAC secret

An exponent over 2^31-1 is left out because `crypto/rsa` refuses to verify with one: the key would look usable, and fail every token. The 2048-bit floor was the OAuth plugin's rule before the codec. It is the smallest RSA key NIST SP 800-131A allows for new signatures.

Two forms that some issuers write are read: base64url with `=` padding, and an EC coordinate shorter than the curve's size, which is padded on the left. The point is checked on its curve either way.

Data that is not a JSON object with a `keys` array is an error. `{"keys": []}` is an empty set, not an error.

## What `MarshalJWKSet` writes

It writes what `ParseJWKSet` would keep, and refuses the rest. A private key is refused: `*rsa.PrivateKey`, `*ecdsa.PrivateKey`, and `ed25519.PrivateKey` are other types than the public ones, and the `jwk` struct has no field for a private member, so none can be written. Each key gets `use: "sig"`, its `kid`, and its `alg` when it names one. EC coordinates are written at the curve's full size (RFC 7518, section 6.2.1.2), from `ecdsa.PublicKey.Bytes`.

The key set route (`httpapi.KeySetRoutes`) turns an error of `MarshalJWKSet` into a security error with `CodeJWKSReadFailed`, and publishes nothing. A set with one bad key is not published without it: a verifier would refuse every token that key signed, with nothing to say why.

## The JWK set source

`keysource.NewJWKSet(client, url)` checks `url` with `outbound.CheckURL`: whoever can change the keys can sign tokens, so they come over `https`, or over `http` from a loopback address in development. A nil client is `outbound.NewClient(outbound.Options{}, nil)`.

`Keys` asks for `application/jwk-set+json, application/json`, reads the answer with `outbound.GetJSON` into a `json.RawMessage`, and decodes it with `ParseJWKSet`. It caches nothing: every call fetches the set.

A set with no key `ParseJWKSet` keeps is `keysource.ErrNoUsableKey`. An empty set, or one of encryption keys only, can check no token. As an error of the source, it is a failure of the system, which an operator sees as such. As an empty set, every token would be refused instead, as if each were forged.

Errors name the URL without its query: a key set URL may carry a tenant or a token.

## The cached key set

`CachedKeySet` keeps what its source last returned. One mutex guards it, and a call that has to fetch holds it while the source answers, so one fetch runs at a time and the other callers wait for it.

| When | It |
| --- | --- |
| the first call | fetches |
| keys older than `TTL` | fetches |
| a `kid` the keys lack (`PublicKey`) | fetches, then looks again. Still missing, the token is refused with `CodeJWTUnknownKey`. |
| less than `RefetchInterval` since the last fetch, whatever caused it | does not fetch. After a fetch that worked, the keys decide. After one that failed, a lookup uses stale keys within `MaxStale`, and a `kid` the keys lack gets the failure: it can't be told from a new key. |
| a fetch failed, keys within `TTL + MaxStale` | uses them, and returns no error |
| a fetch failed, keys missing or older | returns the error: a failure of the system |
| a fetch for an unknown `kid` failed | returns the error, whatever the keys' age |
| a fetch failed because the caller's context ended | returns the error, and records nothing: the next call fetches at once |

`PublicKeys` returns a copy, so a caller can't change the set. `lookup` returns the first key with the `kid`. `VerifyJWT` looks for another one in `PublicKeys` when that key does not fit the token.

## VerifyJWT

The checks run in this order, and the first that fails decides the code:

| # | Check | Refused with |
| --- | --- | --- |
| 1 | the options: an issuer, an audience, asymmetric algorithms only, no negative leeway | a configuration error |
| 2 | the token parses, and its header has no `crit` | `CodeJWTMalformed` |
| 3 | `alg` is one of the options' algorithms, by default `asymmetricAlgs` | `CodeJWTAlgorithmMismatch` |
| 4 | `candidateKeys`: the key `kid` names, and it fits `alg`; or, with the same `kid`, another key that fits (RFC 7517, section 4.5). Without a `kid`, with `AllowMissingKID`: every key that fits. | the set's error, `CodeJWTUnknownKey`, or `CodeJWTAlgorithmMismatch` |
| 5 | the signature, with each candidate until one matches | `CodeJWTInvalidSignature` |
| 6 | golang-jwt's validator: `exp` required, `nbf`, `aud` holds one of the audiences, `iat` with `CheckIssuedAt`, all with the leeway and the options' clock | `checkClaims`: the code of the check |
| 7 | `iss` is one of the issuers. golang-jwt checks a single issuer, and Google has two forms. | `CodeJWTIssuerMismatch` |
| 8 | `sub`, `aud`, `jti`, and the dates have their types | `CodeJWTInvalidClaims` |

`fits` is the ADR's rule for the algorithm: a key that names one signs with it alone, and every key signs only with an algorithm of its type (`algorithmsOf`). The second half also catches a `KeySet` that pairs a key with an algorithm of another type.

`externalJWT.verify` calls `VerifyJWT` with the configured issuer and the external audiences, no leeway, and `iat` unchecked, as before the move. It decodes `wireClaims` from `VerifiedJWT.Claims` and builds `crypto.Claims` with `claimsOf`. Internal tokens keep their own path: they are HS256, and `VerifyJWT` refuses every symmetric algorithm. They share `parse` and `checkClaims`.

## Published test vectors

| Test | File | Covers |
| --- | --- | --- |
| `TestJOSEExampleKeys` | `crypto/jose_vectors_test.go` | the JWKs of RFC 7515 (appendix A), RFC 7517 (appendices A and B), RFC 7520 (section 3), and RFC 8037 (appendix A): which ones `ParseJWKSet` keeps, with their kid and algorithm, and that `MarshalJWKSet` writes their members as the RFC prints them |
| `TestJOSEExampleSignatures` | same | the tokens of RFC 7515 (A.2 to A.4), RFC 7520 (4.1 to 4.3), and RFC 8037 (A.5) verify with the key `ParseJWKSet` reads, through `fits` and the method `VerifyJWT` takes for the algorithm, and fail with a byte of the signature changed. A.2 and A.3 also go through `parse`. The other payloads are not claims, which `parse` decodes. |
| `TestWycheproofSignatures` | `tests/crypto/wycheproof_test.go` | Wycheproof's signatures for each algorithm of `asymmetricAlgs`: the group's key through `ParseJWKSet`, and each signature through the method `VerifyJWT` takes |

The examples have no audience, which `VerifyJWT` requires, so they run through its steps rather than through it whole. Their keys and tokens are in `crypto/jose_vectors_data_test.go`, copied from the RFC texts by a script. The Wycheproof files are in `tests/crypto/testdata/wycheproof`, with their commit and license, so that the module applications download does not carry 2.5 MB of test data.

Two kinds of Wycheproof vector pass that a strict verifier refuses:

- A PSS signature with a salt length other than the hash's verifies (`docs/ongoing.md`, M36). The test counts these, and fails on any other invalid signature that verifies.
- An Ed25519 key of small or mixed order is kept, and signatures under it verify (M37). Wycheproof marks these acceptable.

## Invariants

These invariants hold:

- **`crypto` makes no outbound call.** The codec reads bytes. A source fetches them.
- **A token's algorithm comes from the caller's list and from its key**, never from the header alone. `none` and the HMAC algorithms can't be listed.
- **The signature is checked before any claim.** A forged token learns nothing from which claim fails.
- **A token is accepted from a trusted issuer, for this service.** `VerifyJWT` refuses to run without both.
- **A failure to get the keys is never a refused token.** It is returned as it is, so that a route answers with a 5xx and not a 401.
- **A written set holds public keys only.** `MarshalJWKSet` refuses a private key, and `jwk` has no private member.
- **What `MarshalJWKSet` writes, `ParseJWKSet` reads back to the same keys.** For a key that names its algorithm, the kid, the algorithm, and the key are the same (`TestMarshalJWKSet`).
- **A key's algorithm fits its type.** `ParseJWKSet` leaves out a key whose `alg` does not, and `MarshalJWKSet` refuses one. An EC or Ed25519 key without an `alg` gets the one its type allows.
- **A source's error is a failure of the system.** It is never turned into a refused token.

## Design decisions

### Sources live in package `keysource`

**Context:** ADR-0003 puts the codec in `crypto`, and a source that fetches a set over HTTPS "on `outbound`". It leaves open which package holds the source.

**Options considered:**

- *In `outbound`.* No new package. `outbound` would import `crypto` and know a key format, and it is the generic client every call uses.
- *In a package under `crypto`.* Close to the codec. A reader of `crypto/...` would find a package that calls the network, against the ADR's first invariant.
- *A package of sources, `keysource`.* It imports `crypto` and `outbound`, and neither imports it. A source of another kind, such as a file the platform mounts, goes next to `JWKSet`. A KMS SDK still goes in a module of its own.

**Decision:** `keysource`.

**Revisit if:** a second source needs a module of its own anyway, and the package holds one source only.

### A set without a usable key is an error of the source

**Context:** A set can be well formed and have no key `ParseJWKSet` keeps.

**Options considered:**

- *Return an empty set.* The codec's answer, and what the OAuth plugin did before. Every token is then refused as if it were forged. An operator sees refusals, and no cause.
- *Return `ErrNoUsableKey`.* The failure shows as one of the system, with the URL in the error.

**Decision:** the error, in the source. `ParseJWKSet` keeps returning an empty set: an empty set is valid JSON for a JWK set, and only a caller that needs keys knows it can't do without.

**Revisit if:** an issuer publishes an empty set on purpose, such as while it rotates.

### An EC or Ed25519 key without `alg` gets the algorithm its type allows

**Context:** `alg` is optional in a JWK (RFC 7517, section 4.4). ADR-0003 says a token's algorithm comes from its key, and from the caller's allowlist only for a key that names none.

**Options considered:**

- *Keep `Algorithm` empty when the set names none.* The verifier then works out the algorithm for every key type.
- *Fill it in where one algorithm fits.* An EC key on P-256 signs with ES256 only (RFC 7518, section 3.4), and an Ed25519 key with EdDSA. Only RSA keys are left without, and need the allowlist.

**Decision:** fill it in. A round trip then adds an `alg` member that was not in the original set, which changes no verification.

**Revisit if:** the fully-specified algorithms of RFC 9864, such as `Ed25519`, need support next to `EdDSA`.

### Stale keys are used while the source fails

**Context:** A service checks session tokens on every request. When the key set can't be fetched at the end of the TTL, the set can fail closed, or keep the keys it has.

**Options considered:**

- *Fail closed.* Every token fails with a system error until the source is back. A short outage of Behemoth's key set route takes every service that checks its tokens down with it, which defeats the point of a token checked without calling Behemoth.
- *Keep the keys without limit.* A key the issuer withdrew keeps being trusted for as long as the set can't be fetched.
- *Keep the keys for `MaxStale`, one hour by default.* The outage of an issuer does not stop the services, and a withdrawn key is trusted for a bounded time.

**Decision:** `MaxStale`. A negative value fails closed, for a caller that prefers it.

**Revisit if:** an issuer withdraws a key for a compromise, and an hour is too long.

### Fetches are spaced by `RefetchInterval`, failures included

**Context:** The OAuth plugin fetched its keys again for every sign-in while the provider failed, and for every unknown `kid` once a minute.

**Options considered:**

- *Try again at every call.* Each request waits for the source's timeout while it is down, and a burst of requests asks a failing issuer as many times.
- *Space every fetch.* Between two fetches, the set answers from what it has, or with the last error, at once.

**Decision:** space every fetch. A first fetch that fails makes the next minute fail fast. A caller whose context ends is not counted as a failure of the source, so one client that disconnects does not do that.

**Revisit if:** a minute of failures after a failed first fetch is too long at a service's start.

### A token without a `kid` is refused unless the caller allows it

**Context:** RFC 7515 makes `kid` optional. Behemoth's tokens always carry one. Some OpenID Connect providers with one key leave it out.

**Options considered:**

- *Always try every key that fits.* A token with a bad signature costs one signature check per key of the set.
- *Accept only when exactly one key fits.* The OAuth plugin's rule before `VerifyJWT`. A provider with two keys of one type during a rotation fails every token.
- *Refuse by default, and try every key that fits with `AllowMissingKID`.* go-oidc tries every key the same way.

**Decision:** the third. `AllowMissingKID` is for the OAuth plugin. A token without a `kid` causes no fetch: its key is looked for among those the set has.

**Revisit if:** a provider without `kid` rotates often enough for the TTL to matter.

### The issuer and the audience are required

**Context:** golang-jwt checks `iss` and `aud` only when asked. A verifier that forgets the audience accepts a token issued for another service.

**Decision:** `VerifyJWT` refuses to run without both. Every caller in Behemoth has both, and a service that checks session tokens has its own name.

### `iat` is checked only when asked

**Context:** OpenID Connect asks for `iat` to be checked, with some leeway. A service that checks session tokens on another machine has a clock that may be a second behind Behemoth's.

**Decision:** `CheckIssuedAt`, off by default, and no leeway by default. Checked without leeway across machines, `iat` would refuse tokens at random. `exp` is always required, and decides how long a token lasts.

### `NewCachedKeySet` returns `*CachedKeySet` and an error

**Context:** ADR-0003 sketched `NewCachedKeySet(src, opts) crypto.KeySet`.

**Decision:** the concrete type, which can gain methods without changing `crypto.KeySet`, and an error for a nil source or a negative duration, so a wrong value fails where the set is built.

## Deferred

These parts are deferred:

- **The answer's `Cache-Control`.** The source does not read it, and the cached key set uses its own durations. The default TTL is the `max-age` of Behemoth's own route.
- **A refresh in the background.** The call that finds the keys past their TTL fetches them, and the others wait for it. A refresh while the old keys keep serving would need a goroutine the set has to stop.
- **A word about a failing source.** `crypto` has no logger. While stale keys are used, nothing says that the source fails, except the span of the outbound call when the client has telemetry. Tracked in `docs/ongoing.md` (L48).
- **Ed448, secp256k1, and the fully-specified algorithm names of RFC 9864.** `crypto/ed25519` and golang-jwt know neither curve nor those names. Keys that use them are left out.
- **`x5c` and `x5t`.** A certificate chain in a key is ignored. The key's own members are used, and nothing checks them against the chain.
- **Keys from a file or a KMS.** A source of each kind can be added next to `JWKSet`, or in a module of its own for a KMS SDK.
