package engine

import (
	"testing"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaringPlugin is a plugin whose Declare is declare.
type declaringPlugin struct {
	name    string
	declare func(ic *core.PluginInitContext) error
}

func (p *declaringPlugin) Meta() core.PluginMeta                    { return core.PluginMeta{Name: p.name} }
func (p *declaringPlugin) Version() string                          { return "test" }
func (p *declaringPlugin) Init(*core.AuthContext) error             { return nil }
func (p *declaringPlugin) Routes() []httpapi.Route                  { return nil }
func (p *declaringPlugin) Middlewares() []httpapi.Middleware        { return nil }
func (p *declaringPlugin) Register(core.HookRegistry) error         { return nil }
func (p *declaringPlugin) Declare(ic *core.PluginInitContext) error { return p.declare(ic) }

// A declarer reads, through its scoped catalogs, what was declared before
// it: core's hook points, tables, and permissions. It contributes to a table
// it does not own, and it can't freeze the schema registry, which is
// Prepare's to do once every declarer has run.
func TestDeclarersReadThroughTheirScopedCatalogs(t *testing.T) {
	byOrg := schema.Index{Name: "idx_users_org", Columns: []string{"email"}}
	toSelf := schema.ForeignKey{Name: "fk_users_referrer", Columns: []string{"id"}, RefTable: models.UserTable, RefColumns: []string{"id"}}
	plugin := &declaringPlugin{name: "orgs", declare: func(ic *core.PluginInitContext) error {
		_, ok := ic.Hooks.Lookup(hooks.HookUserBeforeCreate)
		assert.True(t, ok, "core declares its hook points first")
		assert.NotEmpty(t, ic.Hooks.All())
		_, ok = ic.Tokens.Lookup("undeclared")
		assert.False(t, ok)
		assert.Empty(t, ic.RateLimits.RulesForHook(hooks.HookUserBeforeCreate))
		_, ok = ic.RateLimits.Rule("undeclared")
		assert.False(t, ok)
		assert.False(t, ic.RateLimits.HasRule("undeclared"))
		_, ok = ic.Permissions.Lookup("undeclared:permission")
		assert.False(t, ok)
		assert.NotEmpty(t, ic.Permissions.All(), "core declares its permissions first")

		_, ok = ic.Schemas.Lookup(models.UserTable)
		assert.True(t, ok)
		_, ok = ic.Schemas.LookupModel(models.UserTable)
		assert.True(t, ok)
		assert.NotEmpty(t, ic.Schemas.All())
		assert.True(t, errs.IsCategory(ic.Schemas.Freeze(), errs.CategoryConfiguration), "a declarer can't freeze the registry")

		if err := ic.Schemas.ExtendIndex(schema.IndexContribution{Table: models.UserTable, Index: byOrg}); err != nil {
			return err
		}
		return ic.Schemas.ExtendForeignKey(schema.ForeignKeyContribution{Table: models.UserTable, ForeignKey: toSelf})
	}}

	app, err := Prepare([]core.Plugin{plugin}, PrepareConfig{})
	require.NoError(t, err)
	users, ok := app.Schemas.Lookup(models.UserTable)
	require.True(t, ok)
	assert.Contains(t, users.Indexes, byOrg)
	assert.Contains(t, users.ForeignKeys, toSelf)
}
