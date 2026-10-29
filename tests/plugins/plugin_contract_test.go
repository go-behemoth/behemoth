package plugins_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/plugins/emailverification"
	"github.com/go-behemoth/behemoth/plugins/magiclink"
	"github.com/go-behemoth/behemoth/plugins/phonenumber"
	"github.com/go-behemoth/behemoth/plugins/twofactor"
	"github.com/go-behemoth/behemoth/plugins/username"
	dbschema "github.com/go-behemoth/behemoth/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPlugins returns, by name, a constructor for each plugin, with the
// options that Declare checks.
func newPlugins() map[string]func() core.Plugin {
	return map[string]func() core.Plugin{
		"emailpassword": func() core.Plugin {
			return emailpassword.New(emailpassword.Options{Reset: emailpassword.ResetOptions{LinkURL: "https://app.example.com/auth/reset"}})
		},
		"emailverification": func() core.Plugin {
			return emailverification.New(emailverification.Options{
				LinkURL:       "https://app.example.com/auth/verify-email",
				ChangeLinkURL: "https://app.example.com/auth/change-email",
				RevertLinkURL: "https://app.example.com/auth/undo-email-change",
			})
		},
		"magiclink": func() core.Plugin {
			return magiclink.New(magiclink.Options{LinkURL: "https://app.example.com/auth/magic"})
		},
		"phonenumber": func() core.Plugin {
			return phonenumber.New(emailpassword.New(emailpassword.Options{}), phonenumber.Options{PasswordReset: true})
		},
		"twofactor": func() core.Plugin {
			return twofactor.New(twofactor.Options{Issuer: "Example"})
		},
		"username": func() core.Plugin {
			return username.New(emailpassword.New(emailpassword.Options{}), username.Options{})
		},
	}
}

// errDeclare is a declaration or a registration that fails.
var errDeclare = errors.New("declaration refused")

// declarationsFailing returns a PluginInitContext that counts every
// declaration in f, and keeps none. No plugin reads its declarations back.
func declarationsFailing(f *faults) *core.PluginInitContext {
	return &core.PluginInitContext{
		Hooks:       countedHooks{f: f},
		Tokens:      countedTokens{f: f},
		RateLimits:  countedRateLimits{f: f},
		Schemas:     countedSchemas{f: f},
		Permissions: countedPermissions{f: f},
	}
}

type countedHooks struct {
	core.HookCatalog
	f *faults
}

func (c countedHooks) Declare(core.HookPointDef) error { return c.f.next() }

type countedTokens struct {
	core.TokenCatalog
	f *faults
}

func (c countedTokens) Declare(core.TokenKindDef) error { return c.f.next() }

type countedRateLimits struct {
	core.RateLimitCatalog
	f *faults
}

func (c countedRateLimits) DeclareHookRateLimitRule(core.HookRateLimitRule) error { return c.f.next() }
func (c countedRateLimits) DeclareRule(core.RateLimitRule) error                  { return c.f.next() }

type countedSchemas struct {
	dbschema.Registry
	f *faults
}

func (c countedSchemas) Declare(behemoth.Model, dbschema.Table) error           { return c.f.next() }
func (c countedSchemas) ExtendColumn(dbschema.ColumnContribution) error         { return c.f.next() }
func (c countedSchemas) ExtendIndex(dbschema.IndexContribution) error           { return c.f.next() }
func (c countedSchemas) ExtendForeignKey(dbschema.ForeignKeyContribution) error { return c.f.next() }

type countedPermissions struct {
	core.PermissionCatalog
	f *faults
}

func (c countedPermissions) Declare(core.PermissionDef) error { return c.f.next() }

// countedRegistry is a HookRegistry that counts every handler in f, and
// keeps none.
type countedRegistry struct{ f *faults }

func (c countedRegistry) OnBefore(core.HookPoint, core.BeforeHookFunc, *core.HookOptions) error {
	return c.f.next()
}
func (c countedRegistry) OnAfter(core.HookPoint, core.AfterHookFunc, *core.HookOptions) error {
	return c.f.next()
}
func (c countedRegistry) OnFailed(core.HookPoint, core.FailedHookFunc, *core.HookOptions) error {
	return c.f.next()
}

// A plugin passes on a declaration or a registration that fails, whichever
// it is, so that Prepare and Boot stop rather than run a plugin with part
// of its hook points, tokens, rules, tables, permissions, or handlers.
func TestPluginsPassOnAFailedDeclaration(t *testing.T) {
	for name, newPlugin := range newPlugins() {
		t.Run(name, func(t *testing.T) {
			for step, run := range map[string]func(core.Plugin, *faults) error{
				"Declare":  func(p core.Plugin, f *faults) error { return p.Declare(declarationsFailing(f)) },
				"Register": func(p core.Plugin, f *faults) error { return p.Register(countedRegistry{f: f}) },
			} {
				for n := 1; ; n++ {
					f := &faults{err: errDeclare}
					f.failCall(n)
					err := run(newPlugin(), f)
					if f.stop() < n {
						require.NoError(t, err, "%s, with no failure", step)
						break
					}
					assert.ErrorIs(t, err, errDeclare, "%s: declaration %d failed", step, n)
				}
			}
		})
	}
}

// A flow refuses to run before Boot has given its plugin the services it
// needs, with a configuration error rather than a crash.
func TestPluginsRefuseToRunBeforeBoot(t *testing.T) {
	ctx := context.Background()
	passwords := emailpassword.New(emailpassword.Options{})
	phones := phonenumber.New(passwords, phonenumber.Options{PasswordReset: true})
	factors := twofactor.New(twofactor.Options{Issuer: "Example"})
	for name, call := range map[string]func() error{
		"twofactor.Challenge": func() error { _, err := factors.Challenge(ctx); return err },
		"twofactor.SendCode": func() error {
			_, err := factors.SendCode(ctx, twofactor.SendCodeRequest{Method: twofactor.MethodEmail})
			return err
		},
		"twofactor.Verify": func() error {
			_, err := factors.Verify(ctx, twofactor.VerifyRequest{Method: twofactor.MethodTOTP, Code: "123456"})
			return err
		},
		"twofactor.Reset":  func() error { _, err := factors.Reset(ctx, twofactor.ResetRequest{UserID: "u1"}); return err },
		"twofactor.Status": func() error { _, err := factors.Status(ctx); return err },
		"twofactor.Enroll": func() error {
			_, err := factors.Enroll(ctx, twofactor.EnrollRequest{Method: twofactor.MethodTOTP})
			return err
		},
		"twofactor.Confirm": func() error {
			_, err := factors.Confirm(ctx, twofactor.ConfirmRequest{Method: twofactor.MethodTOTP, Code: "123456"})
			return err
		},
		"twofactor.Disable": func() error {
			_, err := factors.Disable(ctx, twofactor.DisableRequest{Method: twofactor.MethodTOTP})
			return err
		},
		"twofactor.RegenerateRecoveryCodes": func() error { _, err := factors.RegenerateRecoveryCodes(ctx); return err },
		"phonenumber.SendCode": func() error {
			_, err := phones.SendCode(ctx, phonenumber.SendRequest{PhoneNumber: "+14155550100"})
			return err
		},
		"phonenumber.AddNumber": func() error {
			_, err := phones.AddNumber(ctx, "u1", "+14155550100", "123456")
			return err
		},
		"phonenumber.SignInWithCode": func() error {
			_, err := phones.SignInWithCode(ctx, "+14155550100", "123456")
			return err
		},
		"phonenumber.SetPhoneNumber": func() error {
			_, err := phones.SetPhoneNumber(ctx, "u1", "+14155550100")
			return err
		},
	} {
		err := call()
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%s: %v", name, err)
	}
}

// A body that is not JSON, or whose fields have other types than a route
// reads, is the client's mistake: no route fails on the server's side for
// it.
func TestRoutesTreatAMalformedBodyAsTheClients(t *testing.T) {
	app := bootFaultApp(t)
	bodies := []string{`{`, `[]`, `"text"`, `{"metadata": "text"}`}
	for _, key := range []string{"email", "password", "token", "code", "method", "phone_number", "new_email", "redirect_url", "metadata", "username"} {
		bodies = append(bodies, fmt.Sprintf(`{%q: 12345}`, key), fmt.Sprintf(`{%q: ["a"]}`, key))
	}
	require.NotEmpty(t, app.routes)
	for path, route := range app.routes {
		for _, body := range bodies {
			w := call(t, app.ac, route, body)
			assert.Less(t, w.Code, 500, "%s %s: %s", path, body, w.Body)
		}
	}
}

// Every plugin has a version, and a result of a hook point that names no
// user names no subject in the audit log.
func TestPluginVersionsAndUnnamedSubjects(t *testing.T) {
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+`)
	for name, newPlugin := range newPlugins() {
		assert.Regexp(t, semver, newPlugin().Version(), name)
	}

	for name, result := range map[string]interface {
		AuditSubject() (subjectType, subjectID string)
	}{
		"emailverification.SendResult":     (*emailverification.SendResult)(nil),
		"emailverification.ChangeResult":   (*emailverification.ChangeResult)(nil),
		"magiclink.LinkResult":             (*magiclink.LinkResult)(nil),
		"emailpassword.ResetRequestResult": (*emailpassword.ResetRequestResult)(nil),
		"phonenumber.SendResult":           &phonenumber.SendResult{},
		"twofactor.SendResult":             &twofactor.SendResult{},
		"twofactor.EnableResult":           (*twofactor.EnableResult)(nil),
		"twofactor.DisableResult":          &twofactor.DisableResult{},
		"twofactor.RegenerateResult":       (*twofactor.RegenerateResult)(nil),
	} {
		subjectType, subjectID := result.AuditSubject()
		assert.Empty(t, subjectType+subjectID, name)
	}
}
