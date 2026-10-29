# Wycheproof test vectors

The JSON files in this directory are copied unchanged from [Project Wycheproof](https://github.com/C2SP/wycheproof), directory `testvectors_v1`, at commit `d17cc89fa5c8f4f4d378b5385ebf75da5d0926c9` (2026-10-23). Wycheproof is licensed under the Apache License 2.0, in [`LICENSE`](LICENSE).

`tests/crypto/wycheproof_test.go` and `tests/crypto/wycheproof_jose_test.go` run them:

| File | Runs through |
| --- | --- |
| `aes_gcm_test.json` | the encryptor: the vectors with a 256-bit key, a 96-bit nonce, and a 128-bit tag |
| `ecdsa_secp256r1_sha256_p1363_test.json` | `ParseJWKSet` and the ES256 method |
| `ecdsa_secp384r1_sha384_p1363_test.json` | `ParseJWKSet` and the ES384 method |
| `ecdsa_secp521r1_sha512_p1363_test.json` | `ParseJWKSet` and the ES512 method |
| `rsa_signature_2048_sha256_test.json` | `ParseJWKSet` and the RS256 method |
| `rsa_signature_3072_sha384_test.json` | `ParseJWKSet` and the RS384 method |
| `rsa_signature_3072_sha512_test.json` | `ParseJWKSet` and the RS512 method |
| `rsa_pss_2048_sha256_mgf1_32_test.json` | `ParseJWKSet` and the PS256 method |
| `rsa_pss_4096_sha384_mgf1_48_test.json` | `ParseJWKSet` and the PS384 method |
| `rsa_pss_4096_sha512_mgf1_64_test.json` | `ParseJWKSet` and the PS512 method |
| `ed25519_test.json` | `ParseJWKSet` and the EdDSA method |
| `json_web_signature_test.json` | `VerifyJWT`, with each group's public key in a `CachedKeySet` |
| `json_web_key_test.json` | `VerifyJWT`, with each group's public keys in a `CachedKeySet` |

The ECDSA files are the IEEE P1363 ones, whose signatures are `r` and `s` side by side, as in a JWS. The other files are the ones whose hash, salt length, and mask generation function are those of the JWS algorithm (RFC 7518, section 3).

The files are in the `tests` module and not in `crypto`, so that the module applications download does not carry them.

To update them, download the same files from a newer commit, check each against the git blob hash GitHub reports for it, and change the commit here.
