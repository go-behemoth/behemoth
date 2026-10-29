package auth

import (
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenCatalogDeclare(t *testing.T) {
	valid := core.TokenKindDef{Kind: "invite", SingleUse: true, DefaultTTL: time.Hour, Backend: core.TokenBackendDB, Owner: "teams"}

	for name, mutate := range map[string]func(*core.TokenKindDef){
		"no kind":            func(d *core.TokenKindDef) { d.Kind = "" },
		"no owner":           func(d *core.TokenKindDef) { d.Owner = "" },
		"no backend":         func(d *core.TokenKindDef) { d.Backend = "" },
		"an unknown backend": func(d *core.TokenKindDef) { d.Backend = "file" },
		"single-use, kv":     func(d *core.TokenKindDef) { d.Backend = core.TokenBackendKV },
	} {
		t.Run(name, func(t *testing.T) {
			def := valid
			mutate(&def)
			err := NewDefaultTokenCatalog().Declare(def)
			assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
		})
	}

	catalog := NewDefaultTokenCatalog()
	require.NoError(t, catalog.Declare(valid))
	multiUse := core.TokenKindDef{Kind: "api_key", Backend: core.TokenBackendKV, DefaultTTL: time.Hour, Owner: "core"}
	require.NoError(t, catalog.Declare(multiUse), "a multi-use kind can live in the key-value storage")

	other := valid
	other.Owner = "another"
	assert.True(t, errs.IsCategory(catalog.Declare(other), errs.CategoryConfiguration), "a kind has one owner")

	def, ok := catalog.Lookup("invite")
	assert.True(t, ok)
	assert.Equal(t, valid, def)
	_, ok = catalog.Lookup("unknown")
	assert.False(t, ok)

	catalog.Freeze()
	late := core.TokenKindDef{Kind: "late", Backend: core.TokenBackendDB, Owner: "teams"}
	assert.True(t, errs.IsCategory(catalog.Declare(late), errs.CategoryConfiguration), "no kind after Boot")
}
