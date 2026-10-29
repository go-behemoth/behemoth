# Keys, hashing, signing, and encryption at rest

Every symmetric key Behemoth uses comes from one master secret per version, through the key manager. This document covers the key manager, the services built on it (the secret hasher, the HMAC signers, and the encryptor), the randomizer, and how a rotation reaches them through a watched secret source. Password hashing takes its pepper from the same key manager; [`passwords.md`](passwords.md) covers the hasher and its bound on hashes in flight. The external JWT keys are derived from it as [ADR-0002](../decisions/0002-deriving-external-jwt-keys.md) describes. Key sets of other issuers are in [`key_sets.md`](key_sets.md).

## Where it sits

| Piece | File | Role |
| --- | --- | --- |
| `KeyManager`, `KeyPurpose`, `SecretSource`, `WatchableSecretSource` | `crypto/contracts.go` | the contracts |
| `DefaultKeyManager`, `NewDefaultKeyManager` | `crypto/key_manager.go` | holds the master secrets by version, and derives a key per purpose and version |
| `StaticSecretSource` | `crypto/secret_source.go` | a fixed set of secrets, without rotation |
| `PollingWatcher` | `crypto/polling_watcher.go` | makes any source watchable by loading it every interval |
| `Argon2idHasher` | `crypto/password_hasher.go` | password hashes, peppered with the `password_hash` key; see [`passwords.md`](passwords.md) |
| `NewSecretHasher` | `crypto/secret_hasher.go` | the hash of a token, a session token, a one-time code, or a recovery code |
| `HMACSigner` | `crypto/signer.go` | signs internal JWTs (`jwt_sign`). `Services.CookieSign` (`cookie_sign`) is built, and no flow uses it yet |
| `AESGCMEncryptor` | `crypto/encryptor.go` | seals OAuth tokens, the OAuth code verifier, and two-factor secrets, through `store.Seal`, each bound to what it is and what it belongs to |
| `DefaultRandomizer` | `crypto/randomizer.go` | random bytes and base64url strings from `crypto/rand` |
| `New` | `crypto/config.go` | builds all of them over one key manager, as `Services` |

## Keys

A master secret is hex in the source, at least 32 bytes once decoded (`KeyManagerConfig.MinSecretBytes`). The key of a purpose for a version is:

```
HKDF-SHA256(secret of the version, no salt, info = the purpose string), 32 bytes
```

The purpose strings are `password_hash`, `token_hash`, `cookie_sign`, `encrypt_at_rest`, `jwt_sign`, and `jwt_external`. Each purpose has its own key, so a signature made for a cookie does not verify as a JWT, and the key that hashes tokens can't open a sealed value. Derived keys are cached per version and purpose.

## What each service computes

| Service | Output | Stored with |
| --- | --- | --- |
| Secret hasher | `hex(HMAC-SHA256(key token_hash, secret))`: 64 lowercase hex characters | the key version, next to the hash |
| Signer | `HMAC-SHA256(key of its purpose, payload)`: 32 bytes | the key version, as the JWT's `kid` |
| Encryptor | `nonce (12 bytes) ‖ AES-256-GCM ciphertext ‖ tag (16 bytes)`, a random nonce from the randomizer. The tag also covers the associated data the caller gives, which is not stored. | the key version, in the sealed form `v<version>:<base64>` (`store/seal.go`). The store's associated data is the value's binding, which the caller names again to open it. |
| Randomizer | `n` bytes from `crypto/rand`; as a string, unpadded base64url | |

Every comparison of a hash or a signature uses `hmac.Equal`. The secret hasher compares the hex strings, and the signer the raw bytes.

`HMACSigner.PrepareSigning` returns the current key fixed, with its version. The JWT provider puts the version in the header and signs the header with that key, so a rotation between the two can't produce a token whose `kid` names a key that did not sign it.

## Rotation

A source that implements `WatchableSecretSource` reports new secrets to the key manager (`applyUpdate`), which then:

1. Validates the update like the first load. An invalid update is logged at Error and ignored; the manager keeps its secrets.
2. Refuses an update that gives a version it holds another secret, whatever else the update holds. It is logged at Error like an invalid update, and nothing of it is applied. See *A version's secret never changes*.
3. Ignores an update that changes nothing: the same current version, and no version it lacks. A polling watcher reports every load, so this is what keeps a poll from counting as a rotation.
4. Adds the new versions, sets the current version, logs `secret rotation applied` at Info, and records a `crypto.secret.rotated` audit event.

An update never removes a version: what was signed or sealed under an old version stays readable. `RemoveVersion` removes one explicitly, and refuses the current one.

`Current` reads the current version once, under the lock, and derives the key of that version. Reading it twice would let a rotation in between return one version's key with the other's number.

`PollingWatcher` loads its source every interval in a goroutine that ends with the context `Watch` was given. A load that fails is skipped. An interval that is not positive is a configuration error, and nothing starts.

## Invariants

These formats are stored, in rows and in clients, so changing any of them breaks what was written before:

- **The derivation**: HKDF-SHA256, no salt, the purpose string as the info, 32 bytes. A change makes every stored hash, signature, and sealed value unverifiable or unreadable, and changes every external public key.
- **The purpose strings.** Each is part of the derivation.
- **The secret hash**: HMAC-SHA256 as lowercase hex. A change makes every token, session, one-time code, and recovery code fail to verify.
- **The signature**: HMAC-SHA256. A change makes every internal JWT fail to verify, and any value signed with the cookie signer.
- **The ciphertext layout**: the nonce first, then the GCM output over the plaintext and the associated data. A change makes every sealed value unreadable.
- **The store's associated data**: `behemoth.seal.v1`, then the binding's purpose and owner, each after its length as four big-endian bytes, and the purposes the callers bind with (`accounts.<column>`, `twofactor.totp_secret`, `oauth.pkce_verifier`). A change makes every sealed value unreadable. `docs/internal/models/models.md` describes the binding.
- **A version's secret never changes.** A key version names one secret; a new secret gets a new version. The key manager refuses an update that breaks this, so the keys it has derived always belong to the secrets it holds.

The tests pin each of them with known answers computed outside the code under test.

## Limits

- **A sealed value is bound to its owner, not to its age.** A value copied into another user's row does not open, but an older value of the same user, written back, does. A password hash is not sealed, and can still be copied between rows.
- **A changed secret is caught only by a process that is running.** Nothing records what a version's secret was, so a replica that starts after the change loads the new secret, and disagrees with the replicas that refused it. Their Error line is the sign. A fingerprint of each version, stored and checked at startup, would catch it.
- **A refused update is logged at every poll** until the source is fixed, as an invalid one is.
- **A polling watcher does not log a failed load.** A source that stays unreachable leaves the key manager on its current secrets without a line.
- **A version removed while the source still lists it comes back** at the next update, which counts as a rotation.

## Design decisions

### A version's secret never changes

**Context:** An update could give a version the key manager holds another secret. The manager replaced the secret and kept the keys it had derived from the old one, so it signed with old keys for some purposes and new ones for others until it restarted, and replicas that started later disagreed with it.

**Options considered:**

- *Apply it, and remove the version's derived keys from the cache.* Every replica that gets the update agrees. But a new secret for a version in use makes everything under it unusable: sealed two-factor secrets and OAuth tokens can't be decrypted, token, session, and code hashes stop verifying, and passwords hashed with the old pepper fail.
- *Refuse it.* A key version names one secret, as in key management systems (Vault's transit engine, Google Tink, AWS KMS), and as a JWK `kid` names one key. Behemoth publishes its external keys under the version, so a changed secret would also change a published key under the same `kid`.

**Decision:** Refuse it, and the whole update with it. A changed secret for a version in use is almost always a mistake: a secret edited in place, or a store that returns its latest value under a fixed version. Refusing keeps the process serving with the keys it has, and the Error line tells the operator.

**Revisit if:** a deployment needs to replace a compromised secret without a new version. Then the data under it is lost anyway, and a forced apply that clears the cache would fit.

## Tests

| Test | File | Covers |
| --- | --- | --- |
| `TestEncryptorKnownAnswer` | `crypto/encryptor_test.go` | the `encrypt_at_rest` key of a known secret, and the ciphertext of a known nonce, plaintext, and associated data |
| `TestEncryptorBindsTheAssociatedData` | same | a ciphertext opens only with its associated data: another, a changed byte, a cut or extended one, or none are refused |
| `TestEncryptorRoundTrip` | same | plaintexts of any length come back, the length is 12 + n + 16, each ciphertext has its own nonce |
| `TestEncryptorRefusesAChangedCiphertext` | same | any byte changed, every cut, a byte added, nothing: `invalid_payload` or `short_cipher_length` |
| `TestEncryptorKeyVersions` | same | the old version opens what it sealed after a rotation, no other version does, an unknown version is refused |
| `TestEncryptorFailures` | same | a failing key manager or randomizer, a key AES can't use |
| `TestSecretHasherKnownAnswer` | `crypto/secret_hasher_test.go` | the hash of a known secret, the same hash twice |
| `TestSecretHasherVerify` | same | only the right secret and the exact hash verify |
| `TestSecretHasherKeyVersions` | same | an old hash verifies with its version after a rotation, with no other |
| `TestSecretHasherKeyFailure` | same | a failing key manager |
| `TestSignerKnownAnswer` | `crypto/signer_test.go` | the signature of a known payload by the cookie and the JWT signers |
| `TestSignerVerify` | same | another payload, any byte of the signature changed, a cut or longer signature, the other purpose's signature |
| `TestSignerKeyVersions` | same | an old signature verifies with its version after a rotation, with no other |
| `TestPrepareSigningFixesTheKey` | same | a prepared key keeps its key and version through a rotation |
| `TestSignerKeyFailure` | same | a failing key manager |
| `TestRandomizerLengths` | `crypto/randomizer_test.go` | the byte count, and the base64url length and alphabet |
| `TestRandomizerRefusesALengthThatIsNotPositive` | same | zero and a negative length are errors, not a panic |
| `TestRandomizerOutputVaries` | same | no repeated value, about half the bits set |
| `TestRandomizerReadFailure` | same | a failed or short read of `crypto/rand` |
| `TestPollingWatcherReportsEachLoad` | `crypto/polling_watcher_test.go` | each load is reported, a change within an interval |
| `TestPollingWatcherSkipsAFailedLoad` | same | a failed load is not reported, the next good one is |
| `TestPollingWatcherStopsWithItsContext` | same | no load after the context is done |
| `TestPollingWatcherRefusesAnIntervalThatIsNotPositive` | same | a configuration error, and nothing polls |
| `TestKeyManagerRotatesThroughAPollingWatcher` | same | a rotation is picked up and audited once, polls that find nothing new are not |
| `TestDefaultKeyManager_CurrentDuringRotation` | `crypto/key_manager_test.go` | the key and the version `Current` returns agree while rotations run; with `-race`, no unlocked read |
| `TestDefaultKeyManager_UpdateThatChangesNothing` | same | an update that changes nothing is neither logged nor audited |
| `TestDefaultKeyManager_RefusesANewSecretForAHeldVersion` | same | an update that changes a held version's secret is refused whole, with an Error line that does not carry the secret, also when it adds a version or moves the current one |
| `TestSealKnownAnswer`, `TestSealIsBoundToItsOwnerAndPurpose` | `tests/store/seal_test.go` | the sealed form of a known value and binding, which pins the encoding; another owner or purpose is refused; a binding without both fields is an error |
| `TestHMACSHA256_RFC4231` | `crypto/vectors_test.go` | the signer's and the secret hasher's HMAC-SHA256 under RFC 4231's keys, test cases 1 to 4, 6, and 7 |
| `TestKeyManagerDerivesWithHKDF_RFC5869` | same | the key manager's HKDF-SHA256 with RFC 5869's test case 3, the one without a salt |
| `TestDerivedKeySetSigns_RFC8032` | same | the derived key set's Ed25519 public keys and signatures for RFC 8032's tests 1 to 3 and RFC 8037's appendix A.4 |
| `TestWycheproofAESGCM` | `tests/crypto/wycheproof_test.go` | the encryptor's sealed form and opening for Wycheproof's AES-256-GCM vectors with a 12-byte nonce and a 16-byte tag; an invalid one is refused with `invalid_payload` |

The known answers of the tests not named after an RFC or Wycheproof were computed with Python's standard library (`hmac`, `hashlib`, and an HKDF checked against RFC 5869 test case 3) and its `cryptography` package for AES-GCM. The others run published vectors through the services. Some published vectors have nothing to check here: RFC 5869's cases 1 and 2 have a salt, which the key manager does not use, RFC 4231's case 5 truncates the output, and Behemoth uses no HMAC-SHA512. The Wycheproof files are in `tests/crypto/testdata/wycheproof`, outside the module applications download.
