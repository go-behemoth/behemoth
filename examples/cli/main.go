// Command cli is an application whose migrations are generated with the
// behemoth command line. It runs against PostgreSQL.
//
//	behemoth generate            # show the next migration
//	behemoth generate -confirm   # write it to ./migrations
//	psql "$DATABASE_URL" -f migrations/0001_create_accounts_and_8_more.sql
//	behemoth generate            # now answers "No schema changes detected."
//	go run .                     # Prepare + Boot, then serve HTTP
//
// "behemoth" is the launcher. Install it once, from the repository root:
//
//	go install ./cmd/behemoth
//
// Without it, the same commands are "go run ./cmd/behemoth generate".
//
// DATABASE_URL defaults to postgres://postgres:postgres@localhost:5432/behemoth?sslmode=disable
//
// With Docker, compose.yaml runs PostgreSQL, the migration and the server,
// with no Go toolchain or local database:
//
//	docker compose up --build   # the server listens on :8080
//	docker compose down -v      # stop, and drop the database
//
// Its migrate job runs this example's own command (cmd/behemoth), built into
// the image, and applies the script with psql (migrate.sh).
//
// The setup is in the auth package. This file and the command line both
// call auth.Prepare and auth.OpenDB, so the tables the migration creates are
// the ones the server reads. With the server running:
//
//	curl -X POST localhost:8080/api/auth/sign-up/email \
//	  -d '{"email":"ada@example.com","password":"correct horse battery"}'
package main

import (
	"context"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/examples/cli/auth"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/plugins/adapters/nethttp"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
)

func main() {
	ctx := context.Background()

	// 1. Declarations. The command line starts from the same call.
	app, err := auth.Prepare()
	if err != nil {
		log.Fatal(err)
	}

	// 2. Storage, bound to the resolver Prepare derived from the schema.
	sqlDB, err := auth.OpenDB(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	db := postgres.NewPostgresAdapter(sqlDB, app.Resolver)

	// 3. Runtime, with behemoth's routes on the application's mux.
	cryptoCfg, err := cryptoConfig()
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	if _, err := engine.Boot(ctx, app, db, engine.BootConfig{
		Crypto:  cryptoCfg,
		HTTP:    nethttp.New(mux),
		Session: core.SessionConfig{ExpiresIn: 24 * time.Hour},
		Router:  httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}},
	}); err != nil {
		log.Fatal(err)
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// cryptoConfig reads the master secret (hex, at least 32 bytes) from
// BEHEMOTH_SECRET. Without one it generates a throwaway secret, which is
// fine locally but invalidates every session on restart.
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
	}, nil
}
