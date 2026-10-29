package errs

// Codes of the errors for a JWT that is refused. They are CategoryToken,
// which a route answers with 401, and share one public message, so that a
// client is not told which check failed. An expired JWT has
// CodeTokenExpired, the code of every expired token.
const (
	// CodeJWTMalformed: the input is not a JWT, or a segment can't be
	// decoded.
	CodeJWTMalformed = "malformed_token"
	// CodeJWTAlgorithmMismatch: the header names an algorithm the
	// verifier does not allow, or one the key does not sign with.
	CodeJWTAlgorithmMismatch = "algorithm_mismatch"
	// CodeJWTInvalidSignature: the signature does not match the key the
	// header names.
	CodeJWTInvalidSignature = "invalid_signature"
	// CodeJWTNotYetValid: nbf is in the future.
	CodeJWTNotYetValid = "token_not_yet_valid"
	// CodeJWTIssuerMismatch: iss is not an issuer the verifier trusts.
	CodeJWTIssuerMismatch = "issuer_mismatch"
	// CodeJWTAudienceMismatch: aud names none of the verifier's
	// audiences.
	CodeJWTAudienceMismatch = "audience_mismatch"
	// CodeJWTMissingClaim: a required claim, exp or aud, is absent.
	CodeJWTMissingClaim = "missing_claim"
	// CodeJWTInvalidClaims: a registered claim fails a check that has
	// no code of its own.
	CodeJWTInvalidClaims = "invalid_claims"
	// CodeJWTUnknownKey: the header names a key the key set does not
	// have, or names none where a kid is required.
	CodeJWTUnknownKey = "unknown_signing_key"
	// CodeJWTTooLarge: the token, or its header, is longer than a
	// verifier reads. It is refused before it is decoded.
	CodeJWTTooLarge = "token_too_large"
)

// Codes of the errors for a JWT that could not be issued or checked because
// of the system, not the token. They are not CategoryToken.
const (
	// CodeJWTUnknownKind: the kind is neither internal nor external.
	CodeJWTUnknownKind = "unknown_jwt_kind"
	// CodeJWTExternalNotConfigured: an external JWT was asked for, and
	// the provider has no key set.
	CodeJWTExternalNotConfigured = "external_jwt_not_configured"
	// CodeJWTEncodingFailed: the claims could not be encoded.
	CodeJWTEncodingFailed = "claims_marshal_failed"
	// CodeJWTSigningFailed: the signature could not be made.
	CodeJWTSigningFailed = "signing_failed"
	// CodeJWKSNoActiveKey: the key set has no current key to sign with.
	CodeJWKSNoActiveKey = "no_active_signing_key"
	// CodeJWKSReadFailed: the key set could not be read.
	CodeJWKSReadFailed = "error_retrieving_keys"
	// CodeJWKSKeygenFailed: a new key pair could not be generated.
	CodeJWKSKeygenFailed = "keygen_failed"
	// CodeJWKSRotateFailed: a new key could not be stored as the
	// current one.
	CodeJWKSRotateFailed = "key_update_failed"
)

// NewJWTError returns the error for a JWT that is refused, with one of the
// refusal codes above.
func NewJWTError(op, code string, original error) *DomainError {
	return &DomainError{
		Category:        CategoryToken,
		Op:              op,
		Entity:          "jwt",
		Code:            code,
		PublicMessage:   "invalid or expired token",
		InternalMessage: op + ": " + code,
		Original:        original,
	}
}

// NewConfigurationErrorCode is NewConfigurationError with a code of its own,
// for a misconfiguration a caller tells apart, such as
// CodeJWTExternalNotConfigured.
func NewConfigurationErrorCode(op, code, detail string) *DomainError {
	e := NewConfigurationError(op, detail, nil)
	e.Code = code
	return e
}
