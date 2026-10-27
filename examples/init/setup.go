package main

import (
	"database/sql"
	"os"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/plugins/magiclink"
	"github.com/go-behemoth/behemoth/plugins/oauth"
	"github.com/go-behemoth/behemoth/plugins/twofactor"
	_ "github.com/lib/pq"
)

// Everything below is shared by both commands, so `migrate` and `serve`
// always see the same plugins, the same schema and therefore the same
// SchemaResolver.

// appPlugins are the plugins for Prepare, and those a command calls
// directly.
type appPlugins struct {
	all []core.Plugin
	// passwords is the email/password plugin, whose SignUp and SignIn the
	// signup command calls.
	passwords *emailpassword.Plugin
	// oauth is the OAuth plugin, whose AccessToken the oauth-token command
	// calls, and oauthProviders the ids of its providers, which the OAuth
	// page offers. oauth is nil when the environment configures no provider
	// (oauth.go).
	oauth          *oauth.Plugin
	oauthProviders []string
}

// plugins returns the plugins for Prepare.
//
// Two of them mail a link, and each is told which page of the application
// its link opens (pages.go). The messages go to the sender in BootConfig.Mail
// (mail.go). The OAuth plugin has no table of its own, so `migrate` writes
// the same migration with or without it.
func plugins() appPlugins {
	ep := emailpassword.New(emailpassword.Options{
		// Setting the link URL is what turns password reset on.
		Reset: emailpassword.ResetOptions{LinkURL: appURL() + pathResetPage},
	})
	// Sign-in by a mailed link, next to sign-in by password. A user created
	// by either plugin can use both.
	ml := magiclink.New(magiclink.Options{LinkURL: appURL() + pathMagicPage})
	// A second factor for a user who turns one on: an authenticator app, or
	// a code by email, which goes to the same sender as the links. Every
	// sign-in of that user then waits for it, by password or by link.
	tf := twofactor.New(twofactor.Options{
		Issuer:  "Behemoth example",
		Methods: []twofactor.Method{twofactor.MethodTOTP, twofactor.MethodEmail},
	})
	all := []core.Plugin{ep, ml, tf, &ActivityPlugin{}}
	// Sign-in with a provider, when one is configured.
	oa, providers := oauthPlugin()
	if oa != nil {
		all = append(all, oa)
	}
	return appPlugins{all: all, passwords: ep, oauth: oa, oauthProviders: providers}
}

func prepareConfig() engine.PrepareConfig {
	return engine.PrepareConfig{
		Migration: migration.MigrationConfig{
			FolderPath: "migrations",
			// Path is left empty: PathGenerateOnly, behemoth writes files
			// and never touches the database schema on its own.
		},
		Schema: declareAppSchema,
	}
}

func openDB() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/behemoth?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	return db, db.Ping()
}
