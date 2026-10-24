package crypto

import (
	"context"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
)

// Config is the security configuration an application hands to Boot. Boot
// builds the Services bundle from it; applications never assemble the
// individual hashers, signers and encryptors themselves.
type Config struct {
	// Secrets supplies the master secret(s) every purpose-specific key is
	// derived from. Required.
	Secrets    SecretSource
	KeyManager KeyManagerConfig

	// Password tunes Argon2id password hashing. nil, or a zero field,
	// means the value of DefaultParams. See [Params] for the values that
	// are refused.
	Password *Params

	// PasswordLimit bounds the password hashes that run at once. The zero
	// value means the defaults described on [PasswordLimit].
	PasswordLimit PasswordLimit
}

// New builds every cryptographic component from cfg, all sharing one
// KeyManager. ctx bounds the KeyManager's secret watch, if the source
// supports hot rotation, so it should live as long as the application.
func New(ctx context.Context, cfg Config, tel *telemetry.Telemetry) (Services, error) {
	if cfg.Secrets == nil {
		return Services{}, errs.NewConfigurationError("crypto.New", "a SecretSource is required", nil)
	}
	// Checked before the key manager starts its watch, which would outlive
	// a failed New until ctx ends.
	if _, err := cfg.Password.withDefaults(); err != nil {
		return Services{}, err
	}
	if _, err := cfg.PasswordLimit.withDefaults(); err != nil {
		return Services{}, err
	}
	tel = telemetry.OrDefault(tel)

	km, err := NewDefaultKeyManager(ctx, cfg.KeyManager, cfg.Secrets, tel)
	if err != nil {
		return Services{}, err
	}
	rnd := NewRandomizer()
	passwords, err := NewArgon2idHasher(cfg.Password, cfg.PasswordLimit, km, tel)
	if err != nil {
		return Services{}, err
	}

	return Services{
		Keys:       km,
		Random:     rnd,
		Secrets:    NewSecretHasher(km),
		Passwords:  passwords,
		CookieSign: NewSigner(km, KeyPurposeCookieSign),
		JWTSign:    NewSigner(km, KeyPurposeJWTSign),
		AtRest:     NewEncryptor(km, rnd),
	}, nil
}
