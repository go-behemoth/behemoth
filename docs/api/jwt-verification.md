# JWT verification

Package `crypto` checks a JWT that an issuer signed with an asymmetric key: a session token that Behemoth issued, checked in another Go service, or a token of another issuer. You give it the issuer's keys, and the issuer and audience a token has to name.

## Verify tokens from an issuer's key set

An issuer publishes its public keys as a JWK set at a URL, such as Behemoth's `/.well-known/jwks.json`. Build the key set once, when your service starts, and check every token with it:

```go
source, err := keysource.NewJWKSet(nil, "https://auth.example.com/.well-known/jwks.json")
if err != nil {
	return err
}
keys, err := crypto.NewCachedKeySet(source, crypto.CacheOptions{})
if err != nil {
	return err
}

// For each request:
verified, err := crypto.VerifyJWT(ctx, token, keys, crypto.VerifyOptions{
	Issuers:   []string{"https://auth.example.com"},
	Audiences: []string{"https://api.example.com"},
})
if err != nil {
	return err
}
userID := verified.Subject
```

`keysource.NewJWKSet` takes the URL of the set and an `*http.Client`. A nil client is a client with the defaults of [Outbound calls](outbound.md). The URL has to be `https`, or `http` on a loopback address in development.

## What a token has to be

`VerifyJWT` accepts a token only when every one of these checks passes:

- **The size.** The token is at most 64 KiB, and its header at most 8 KiB. A longer one is refused before any of it is decoded.
- **The encoding.** The token is three segments of base64url without padding, in their one encoding: a line break, a space, or a last character with its unused bits set is refused. A token can't be changed into another string that verifies.
- **The algorithm.** The header's `alg` is one of `VerifyOptions.Algorithms`, and the key's own algorithm. For a key that names none, such as an RSA key without `alg`, it is one the key's type signs with. `none` and the HMAC algorithms are always refused.
- **The key.** The header's `kid` names a key of the set. A token without a `kid` is refused, unless `AllowMissingKID` is set. A key the header carries itself (`jwk`, `jku`, `x5u`, `x5c`) is never used.
- **The signature.** It is checked before the claims are decoded.
- **The time.** `exp` is required and has not passed. `nbf`, when present, has passed. `iat` is checked with `CheckIssuedAt`.
- **The issuer and the audience.** `iss` is one of `Issuers`, and `aud` holds one of `Audiences`.

### VerifyOptions

| Field | Default | Meaning |
| --- | --- | --- |
| `Issuers` | required | The values `iss` may have. |
| `Audiences` | required | Your service's names. `aud` has to hold one of them, so a token issued for another service is refused. |
| `Algorithms` | every asymmetric algorithm | The algorithms a token may be signed with: `EdDSA`, `ES256`, `ES384`, `ES512`, `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`. |
| `Leeway` | none | The clock skew allowed on `exp`, `nbf`, and `iat`. At most five minutes. |
| `CheckIssuedAt` | `false` | Refuses a token whose `iat` is later than now, plus `Leeway`. |
| `AllowMissingKID` | `false` | Checks a token without a `kid` against every key of the set that fits its algorithm. |
| `Now` | `time.Now` | The clock, for tests. |

Options without an issuer or an audience, a symmetric algorithm in `Algorithms`, and a `Leeway` that is negative or over five minutes are a configuration error. Servers whose clocks NTP keeps are within a second of each other: a longer leeway only lets a token be used for that much longer after it expires.

## Read the claims

A `VerifiedJWT` has the header's `KID` and `Algorithm`, and the registered claims: `Issuer`, `Subject`, `Audience`, `ID`, `ExpiresAt`, `IssuedAt`, and `NotBefore`. `Claims` decodes the others:

```go
var claims struct {
	SessionID string         `json:"sid"`
	Custom    map[string]any `json:"custom"`
}
if err := verified.Claims(&claims); err != nil {
	return err
}
```

A `map[string]any` receives every claim.

## Keep the keys

A `CachedKeySet` fetches the keys at the first token it checks, and keeps them:

| Field of `CacheOptions` | Default | Meaning |
| --- | --- | --- |
| `TTL` | 5 minutes | How long the keys are used before they are fetched again. |
| `RefetchInterval` | 1 minute | The shortest time between two fetches. A token whose `kid` the keys lack makes the set fetch them again, at most this often. |
| `MaxStale` | 1 hour | How long past `TTL` the keys are still used while the key set can't be fetched. A negative value means not at all. |
| `Now` | `time.Now` | The clock, for tests. |

What this means for your service:

- **A new key is found at once.** When the issuer rotates its keys, the first token signed with the new one makes the set fetch them again.
- **A token with a made-up `kid` costs nothing.** It is refused, and it causes one fetch per `RefetchInterval` at most.
- **An issuer that is down does not stop your service for an hour.** The keys of the last fetch keep checking tokens for `MaxStale`, and the set asks again once per `RefetchInterval`.
- **One set serves the whole service.** It is safe for concurrent use. Build it once, not per request.

## Errors

| Error | Means | Answer the client with |
| --- | --- | --- |
| a `CategoryToken` error | the token is refused. Its code is one of the table below. | `401` |
| any other error | the keys could not be fetched, or the options can't work | `500`, or `503` while the issuer is down |

Check with `errs.IsCategory(err, errs.CategoryToken)`. The codes of a refused token:

| Code | The token |
| --- | --- |
| `token_too_large` | is longer than 64 KiB, or has a header longer than 8 KiB |
| `malformed_token` | is not three segments of base64url in their one encoding, has a header without an `alg`, a `kid` that is not a string, or a `crit` member, or has claims that are not a JSON object |
| `algorithm_mismatch` | names an algorithm that is not allowed, or that its key does not sign with |
| `unknown_signing_key` | names a `kid` the set lacks, even after a fetch, or names none |
| `invalid_signature` | has a signature that does not match |
| `token_expired` | has an `exp` that has passed |
| `token_not_yet_valid` | has an `nbf` in the future |
| `issuer_mismatch` | has an `iss` that is not one of `Issuers` |
| `audience_mismatch` | has an `aud` that holds none of `Audiences` |
| `missing_claim` | has no `exp`, or no `aud` |
| `invalid_claims` | has a registered claim of the wrong type, or an `iat` in the future with `CheckIssuedAt` |

All of them have the same public message, so a client is not told which check failed.

## Keys from elsewhere

`NewCachedKeySet` takes any `crypto.KeySource`: a type with `Keys(ctx) ([]crypto.PublishedKey, error)`, which returns the keys as they are each time it is called. `keysource.JWKSet` is one. Write your own for keys from a file or a key management service.

To read a JWK set you already have, use `crypto.ParseJWKSet`. It returns the keys that can verify a signature, and leaves out the others: symmetric keys, keys for encryption, RSA keys under 2048 bits, keys that carry a private part, and keys whose `alg` does not fit them. `crypto.MarshalJWKSet` writes keys as a JWK set.

`VerifyJWT` takes any `crypto.KeySet`. Behemoth's own key set is one, and `ac.JWT.Verify` checks Behemoth's tokens with it in the process that issues them.
