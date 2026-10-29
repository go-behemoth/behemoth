// Command quickstart is the smallest Behemoth application: email and
// password sign-up and sign-in on PostgreSQL and net/http.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/plugins/adapters/nethttp"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"

	"github.com/go-behemoth/behemoth/examples/quickstart/auth"
)

func main() {
	ctx := context.Background()

	// 1. Declarations: plugins and schema. No database yet.
	app, err := auth.Prepare()
	if err != nil {
		log.Fatal(err)
	}

	// 2. Storage, bound to the resolver that Prepare derived.
	sqlDB, err := auth.OpenDB(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	db := postgres.NewPostgresAdapter(sqlDB, app.Resolver)

	// 3. Runtime: sessions and the routes, on your own mux.
	mux := http.NewServeMux()
	_, err = engine.Boot(ctx, app, db, engine.BootConfig{
		Crypto: crypto.Config{
			// The master secret: 64 hex characters, from `openssl rand -hex 32`.
			Secrets: crypto.StaticSecretSource{
				Secrets: map[int]string{1: os.Getenv("BEHEMOTH_SECRET")},
				Current: 1,
			},
		},
		HTTP:    nethttp.New(mux),
		Session: core.SessionConfig{ExpiresIn: 24 * time.Hour},
		// The session token travels in a header, which curl shows.
		Router: httpapi.RouterConfig{
			Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(http.ListenAndServe(":8080", mux))
}
