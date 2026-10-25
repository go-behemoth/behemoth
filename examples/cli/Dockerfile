# syntax=docker/dockerfile:1

# Two images of the cli example, run together by compose.yaml in this
# directory:
#
#   cd examples/cli
#   docker compose up --build   # PostgreSQL, the migration, then the server on :8080
#   docker compose down         # add -v to drop the database and the migrations
#
# migrate  writes the next migration with the example's own behemoth command
#          (cmd/behemoth), applies the script with psql, and exits.
# server   the HTTP server (main.go). It is the last stage, so a build
#          without --target gives it.
#
# The example's go.mod replaces behemoth and its PostgreSQL adapter with
# paths inside this repository, so the build context is the REPOSITORY ROOT:
#
#   docker build -f examples/cli/Dockerfile -t behemoth-cli .

ARG POSTGRES_IMAGE=postgres:18-alpine

FROM golang:1.26-bookworm AS build
WORKDIR /src

# Module files first, so dependency downloads are cached across source edits.
COPY go.mod go.sum ./
COPY storage/adapters/postgres/go.mod storage/adapters/postgres/go.sum storage/adapters/postgres/
COPY examples/cli/go.mod examples/cli/go.sum examples/cli/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd examples/cli && go mod download

COPY . .
# No cgo: lib/pq is pure Go, so both binaries are fully static and run on
# Alpine and on distroless alike.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd examples/cli \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server . \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/behemoth ./cmd/behemoth

# The PostgreSQL image brings psql and a shell, which applying the script
# needs. The binary built without the launcher is what runs where there is no
# Go toolchain.
FROM ${POSTGRES_IMAGE} AS migrate
WORKDIR /app
COPY --from=build /out/behemoth /usr/local/bin/behemoth
COPY --chmod=0755 examples/cli/migrate.sh /usr/local/bin/migrate
ENTRYPOINT ["migrate"]

FROM gcr.io/distroless/static-debian12:nonroot AS server
WORKDIR /app
COPY --from=build /out/server /app/server
ENV ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/app/server"]
