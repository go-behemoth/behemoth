package crypto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
	"golang.org/x/crypto/hkdf"
)

type KeyManagerConfig struct {
	MinSecretBytes int
	Environment    behemoth.Environment
}

func (c KeyManagerConfig) withDefaults() KeyManagerConfig {
	if c.MinSecretBytes == 0 {
		c.MinSecretBytes = 32
	}

	return c
}

const devDefaultSecret = "dev-insecure-default-secret-do-not-use-in-production"

type derivedKeyCacheKey struct {
	version int
	purpose KeyPurpose
}

type DefaultKeyManager struct {
	mu      sync.RWMutex
	secrets map[int][]byte
	current int
	cache   map[derivedKeyCacheKey][]byte
	cfg     KeyManagerConfig
	tel     *telemetry.Telemetry
}

func NewDefaultKeyManager(ctx context.Context, cfg KeyManagerConfig, source SecretSource, tel *telemetry.Telemetry) (*DefaultKeyManager, error) {
	tel = telemetry.OrDefault(tel).Named("crypto")
	raw, current, err := source.Load(ctx)
	if err != nil {
		return nil, errs.NewConfigurationError("KeyManager.Load", "initial secret load failed", err)
	}

	decoded, err := decodeAndValidate(raw, current, cfg)
	if err != nil {
		return nil, err
	}

	km, err := buildKeyManager(decoded, current, cfg, tel)
	if err != nil {
		return nil, err
	}

	if watchable, ok := source.(WatchableSecretSource); ok {
		if err := watchable.Watch(ctx, km.applyUpdate); err != nil {
			tel.Logger.Warn(ctx, "failed to start secret watch; rotation will require a restart", telemetry.ErrorFields(err))
		} else {
			tel.Logger.Info(ctx, "secret hot-rotation enabled", nil)
		}
	} else {
		tel.Logger.Info(ctx, "secret source does not support hot rotation; rotation requires a restart", nil)
	}

	return km, nil
}

// Current reads the current version once, under the lock, so that a
// rotation can't pair the key of one version with the number of another.
func (km *DefaultKeyManager) Current(purpose KeyPurpose) (key []byte, version int, err error) {
	version = km.CurrentVersion()
	key, err = km.deriveKey(purpose, version)
	return key, version, err
}

func (km *DefaultKeyManager) GetByVersion(purpose KeyPurpose, version int) (key []byte, err error) {
	return km.deriveKey(purpose, version)
}

func (km *DefaultKeyManager) CurrentVersion() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.current
}

// Versions implements [KeyManager].
func (km *DefaultKeyManager) Versions() []int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	versions := make([]int, 0, len(km.secrets))
	for v := range km.secrets {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	return versions
}

func decodeAndValidate(raw map[int]string, current int, cfg KeyManagerConfig) (map[int][]byte, error) {
	cfg = cfg.withDefaults()

	if len(raw) == 0 {
		return nil, errs.NewConfigurationError("KeyManager.Load", "source returned no secrets", nil)
	}

	if _, ok := raw[current]; !ok {
		return nil, errs.NewConfigurationError("KeyManager.Load",
			fmt.Sprintf("current version %d not present among provided secrets", current), nil)
	}

	decoded := make(map[int][]byte, len(raw))
	for version, value := range raw {
		if cfg.Environment == behemoth.EnvProduction && (value == "" || value == devDefaultSecret) {
			return nil, errs.NewConfigurationError("KeyManager.Load",
				fmt.Sprintf("refusing empty/default secret for version %d in production", version), nil)
		}
		b, err := hex.DecodeString(value)
		if err != nil {
			return nil, errs.NewConfigurationError("KeyManager.Load",
				fmt.Sprintf("version %d is not valid hex", version), err)
		}
		if len(b) < cfg.MinSecretBytes {
			return nil, errs.NewConfigurationError("KeyManager.Load",
				fmt.Sprintf("version %d is %d bytes, need >= %d", version, len(b), cfg.MinSecretBytes), nil)
		}
		decoded[version] = b
	}

	return decoded, nil
}

func (km *DefaultKeyManager) deriveKey(purpose KeyPurpose, version int) ([]byte, error) {
	ck := derivedKeyCacheKey{version: version, purpose: purpose}

	km.mu.RLock()
	if derived, ok := km.cache[ck]; ok {
		km.mu.RUnlock()
		return derived, nil
	}
	km.mu.RUnlock()

	km.mu.Lock()
	defer km.mu.Unlock()

	// Check again in case it was added while we were waiting for the lock
	if derived, ok := km.cache[ck]; ok {
		return derived, nil
	}

	masterSecret, ok := km.secrets[version]
	if !ok {
		return nil, errs.NewSecurityError("KeyManager.Derive", errs.CodeUnknownKeyVersion,
			fmt.Errorf("no key material for version %d, purpose %q", version, purpose))
	}

	hk := hkdf.New(sha256.New, masterSecret, nil, []byte(purpose))
	derived := make([]byte, 32)
	if _, err := hk.Read(derived); err != nil {
		return nil, errs.NewSecurityError("KeyManager.Derive", "hkdf_failure", err)
	}

	km.cache[ck] = derived
	return derived, nil
}

// applyUpdate is the hot-reload callback. It's triggered either by a watch callback or manually via an admin API.
// It only adds the new keys in secret. It doesn't remove the "old" keys. Removal must be done via the RemoveVersion function.
// It validates the incoming update before applying to the running KeyManager.
//
// A version's secret never changes. An update that gives a version km holds
// another secret is refused whole, like an invalid one: applying it would
// leave everything hashed, signed, or sealed under that version unverifiable
// or unreadable, and the replicas that did not get the update would disagree
// with the ones that did. A new secret gets a new version.
func (km *DefaultKeyManager) applyUpdate(rawSecrets map[int]string, newCurrent int) {
	decoded, err := decodeAndValidate(rawSecrets, newCurrent, km.cfg)
	if err != nil {
		km.tel.Logger.Error(context.Background(), "rejected invalid secret rotation update", telemetry.ErrorFields(err))
		return // old state keeps serving untouched
	}
	km.mu.Lock()
	defer km.mu.Unlock()

	added, changed := km.compare(decoded)
	if len(changed) > 0 {
		err := errs.NewConfigurationError("KeyManager.Update", fmt.Sprintf(
			"the update gives versions %v, which the key manager holds, another secret; a version's secret never changes, so add a new version instead", changed), nil)
		km.tel.Logger.Error(context.Background(), "rejected invalid secret rotation update", telemetry.ErrorFields(err))
		return
	}
	// A watcher that polls reports what it loaded, changed or not. An update
	// that changes nothing is not a rotation, and is neither logged nor
	// audited.
	if len(added) == 0 && newCurrent == km.current {
		return
	}

	// Only new versions are written, so the derived keys in the cache stay
	// those of the secrets km holds.
	for _, version := range added {
		km.secrets[version] = decoded[version]
	}

	km.current = newCurrent
	km.tel.Logger.Info(context.Background(), "secret rotation applied", behemoth.M{"newCurrent": newCurrent})
	km.tel.RecordAudit(context.Background(), telemetry.AuditEvent{
		Type: telemetry.AuditSecretRotated, ActorType: telemetry.ActorSystem,
		Metadata: behemoth.M{"current_version": newCurrent},
	})

}

// compare sorts the versions of decoded against the secrets km holds: added
// are the versions km lacks, and changed the ones it holds with another
// value, both in ascending order. The caller holds the lock.
func (km *DefaultKeyManager) compare(decoded map[int][]byte) (added, changed []int) {
	for version, secret := range decoded {
		held, ok := km.secrets[version]
		switch {
		case !ok:
			added = append(added, version)
		case !bytes.Equal(held, secret):
			changed = append(changed, version)
		}
	}
	slices.Sort(added)
	slices.Sort(changed)
	return added, changed
}

// RemoveVersion is explicit secret removal method
// It's impossible to remove the current secret before adding a new one
func (km *DefaultKeyManager) RemoveVersion(version int) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	if version == km.current {
		return errs.NewValidationError("KeyManager.RemoveVersion", "version",
			fmt.Errorf("cannot remove the current key version (%d)", version))
	}

	if _, ok := km.secrets[version]; !ok {
		return errs.NewNotFound("KeyManager.RemoveVersion", "key_version", nil)
	}

	// remove it from the secret map
	delete(km.secrets, version)

	// delete all keys created with the secret
	for k := range km.cache {
		if k.version == version {
			delete(km.cache, k)
		}
	}

	return nil
}

func buildKeyManager(secrets map[int][]byte, current int, cfg KeyManagerConfig, tel *telemetry.Telemetry) (*DefaultKeyManager, error) {
	if _, ok := secrets[current]; !ok {
		return nil, errs.NewConfigurationError("KeyManager.Build",
			fmt.Sprintf("current version %d missing after decode", current), nil) // defensive; decodeAndValidate already guarantees this
	}

	return &DefaultKeyManager{
		secrets: secrets,
		current: current,
		cache:   make(map[derivedKeyCacheKey][]byte),
		cfg:     cfg,
		tel:     tel,
	}, nil
}
