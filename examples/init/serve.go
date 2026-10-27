package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/httpapi"
	ginadapter "github.com/go-behemoth/behemoth/plugins/adapters/gin"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
	"github.com/go-behemoth/behemoth/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// serve is the full startup: Prepare, build the adapter with the prepared
// resolver, Boot onto the application's gin engine, then serve.
func serve(ctx context.Context) error {
	// The application owns its engine and its own routes; behemoth only adds
	// its routes to it, under Router.BasePath.
	// Stop on Ctrl-C or SIGTERM, so the server can finish its requests and
	// the exporters can send what they still hold. A process that is killed
	// without this loses its last few seconds of traces and metrics.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	obs, err := newObservability(ctx)
	if err != nil {
		return err
	}

	ginEngine := gin.New()
	if obs.tracerProvider != nil {
		// The application's own tracing middleware, registered before any
		// route. It continues the caller's trace or starts one, and puts the
		// span on the request's context. behemoth reads no trace headers:
		// its behemoth.request span is a child of this one because it is
		// started from that context.
		ginEngine.Use(otelgin.Middleware("behemoth-example", otelgin.WithTracerProvider(obs.tracerProvider)))
	}
	ginEngine.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "my app") })

	ac, plugs, sqlDB, err := boot(ctx, ginadapter.New(ginEngine), obs.tel)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	// The pages the mailed links open, and those of a sign-in with a
	// provider when one is configured.
	mountPages(ginEngine, plugs.oauthProviders)
	if len(plugs.oauthProviders) > 0 {
		log.Printf("sign in with %v at %s%s", plugs.oauthProviders, appURL(), pathOAuthPage)
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: ginEngine}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	log.Printf("listening on %s", addr)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// ctx is cancelled by now, so the shutdown gets a context of its own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log.Print("shutting down")
	err = server.Shutdown(shutdownCtx)
	// Links are mailed in the background. Close waits for the messages
	// still queued, so a link requested just before the stop is not lost.
	err = errors.Join(err, ac.Mailer.Close(shutdownCtx))
	return errors.Join(err, obs.shutdown(shutdownCtx)) // flush traces and metrics last
}

// boot runs Prepare and Boot. driver mounts behemoth's routes on the
// application's HTTP framework; nil boots without serving, which is what the
// other commands do. It returns the plugins, for a command that calls one
// directly. The returned *sql.DB is the caller's to close.
//
// tel is the application's telemetry (telemetry.go). The same logger goes to
// the database adapter, so SQL statements are logged at debug with the
// request ID of the request that ran them.
func boot(ctx context.Context, driver httpapi.FrameworkDriver, tel *telemetry.Telemetry) (*core.AuthContext, appPlugins, *sql.DB, error) {
	// 1. Declarations: plugins, hooks, tokens, rate limits, schema -> resolver.
	plugs := plugins()
	app, err := engine.Prepare(plugs.all, prepareConfig())
	if err != nil {
		return nil, appPlugins{}, nil, err
	}

	// 2. Storage, bound to the resolver Prepare derived from the schema.
	sqlDB, err := openDB()
	if err != nil {
		return nil, appPlugins{}, nil, err
	}
	db := postgres.NewPostgresAdapter(sqlDB, app.Resolver).WithLogger(tel.Logger)

	// 3. Runtime: hook chains, rate limiter, session/token managers, plugin Init.
	cryptoCfg, err := cryptoConfig()
	if err != nil {
		sqlDB.Close()
		return nil, appPlugins{}, nil, err
	}
	ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
		Crypto:    cryptoCfg,
		HTTP:      driver,
		Telemetry: tel,
		// The one mail sender every plugin goes through (mail.go). The magic
		// link plugin and password reset fail Boot without one.
		Mail: core.MailConfig{Sender: core.MailSenderFunc(printMail)},
		// The application's hook handlers (hooks.go). The plugins registered
		// theirs in Register.
		Hooks: appHooks,
		Session: core.SessionConfig{
			ExpiresIn:         24 * time.Hour,
			PendingExpiresIn:  10 * time.Minute,
			CaptureIPAndAgent: true, // sessions record the client's address and user agent
		},
		// The session token travels in the Set-Auth-Token header, and comes
		// back as a bearer token. The rest of the router is left at its
		// defaults: routes under /api/auth. A magic link request that names
		// a redirect to another origin would need that origin in
		// Router.TrustedOrigins.
		Router: httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
		// KV, RateLimit and Token left at their defaults: no KV store
		// (sessions and rate limits use the DB).
	})
	if err != nil {
		sqlDB.Close()
		return nil, appPlugins{}, nil, err
	}
	// Boot has logged its own summary line ("behemoth booted") through tel.
	log.Printf("schema resolver: todos -> %s", app.Resolver.Resolve("todos"))
	return ac, plugs, sqlDB, nil
}

// cryptoConfig reads the master secret (hex, >= 32 bytes) from
// BEHEMOTH_SECRET. Without one it generates a throwaway secret, which is
// fine locally but invalidates every session and token on restart.
func cryptoConfig() (crypto.Config, error) {
	secret := os.Getenv("BEHEMOTH_SECRET")
	if secret == "" {
		b, err := crypto.NewRandomizer().SecureRandomBytes(32)
		if err != nil {
			return crypto.Config{}, err
		}
		secret = hex.EncodeToString(b)
		log.Print("BEHEMOTH_SECRET not set: using a random secret, sessions won't survive a restart")
	}

	return crypto.Config{
		Secrets:    crypto.StaticSecretSource{Secrets: map[int]string{1: secret}, Current: 1},
		KeyManager: crypto.KeyManagerConfig{Environment: behemoth.EnvDev},
		Password:   nil, // crypto.DefaultParams
	}, nil
}
