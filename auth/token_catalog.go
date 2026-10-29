package auth

import (
	"fmt"
	"sync"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
)

type DefaultTokenCatalog struct {
	mu     sync.RWMutex
	kinds  map[models.TokenKind]core.TokenKindDef
	frozen bool
}

func NewDefaultTokenCatalog() *DefaultTokenCatalog {
	return &DefaultTokenCatalog{kinds: map[models.TokenKind]core.TokenKindDef{}}
}

func (c *DefaultTokenCatalog) Declare(def core.TokenKindDef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return errs.NewConfigurationError("TokenCatalog.Declare", "cannot Declare after boot has completed", nil)
	}
	if def.Kind == "" {
		return errs.NewConfigurationError("TokenCatalog.Declare", "TokenKindDef.Kind must not be empty", nil)
	}
	if def.Owner == "" {
		return errs.NewConfigurationError("TokenCatalog.Declare", fmt.Sprintf("kind %q missing Owner", def.Kind), nil)
	}
	if def.Backend != core.TokenBackendDB && def.Backend != core.TokenBackendKV {
		return errs.NewConfigurationError("TokenCatalog.Declare", fmt.Sprintf("kind %q declared with invalid Backend %q", def.Kind, def.Backend), nil)
	}
	if def.SingleUse && def.Backend == core.TokenBackendKV {
		return errs.NewConfigurationError("TokenCatalog.Declare", fmt.Sprintf("kind %q is single-use and needs TokenBackendDB: the key-value storage can't consume a token exactly once", def.Kind), nil)
	}
	if existing, dup := c.kinds[def.Kind]; dup {
		return errs.NewConfigurationError("TokenCatalog.Declare", fmt.Sprintf("kind %q already declared by %q", def.Kind, existing.Owner), nil)
	}
	c.kinds[def.Kind] = def
	return nil
}

func (c *DefaultTokenCatalog) Lookup(kind models.TokenKind) (core.TokenKindDef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	def, ok := c.kinds[kind]
	return def, ok
}

func (c *DefaultTokenCatalog) Freeze() { c.mu.Lock(); c.frozen = true; c.mu.Unlock() }
