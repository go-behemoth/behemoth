package errs

// CodeUnknownKeyVersion is the code of the security error for a key version
// a key manager does not hold. A verifier that took the version from a
// token refuses the token instead (CodeJWTUnknownKey): the version came
// from the client, and the system has not failed.
const CodeUnknownKeyVersion = "unknown_key_version"
