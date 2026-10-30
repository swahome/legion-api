# legion-api

Go API for Legion, the rewrite of the LegionCore (Chamasoft) group savings platform.

## Stack

| Concern | Choice |
|---|---|
| HTTP router | [chi](https://github.com/go-chi/chi) |
| Postgres driver | [pgx v5](https://github.com/jackc/pgx) with `pgxpool` |
| Queries | [sqlc](https://sqlc.dev): SQL in `db/queries`, generated Go in `internal/store` |
| Migrations | [goose](https://github.com/pressly/goose), embedded in the binary |
| Auth | Short-lived JWT access tokens + rotating opaque refresh tokens in an HttpOnly cookie |
| Logging | `log/slog` JSON |

## Getting started

You need Go (version in `go.mod`) and a Postgres 16 database.

```sh
cp .env.example .env     # set DATABASE_URL and LEGION_JWT_SECRET
make user PHONE=0700000001 PASSWORD=secret123   # dev admin user
make run                 # API on :8080, applies migrations on start
make test                # needs TEST_DATABASE_URL in .env
```

## Build and deploy

`make build` produces one static binary, `bin/legion-api`, with the migrations
embedded. Cross-compile with `GOOS`/`GOARCH` (default linux/amd64). To deploy, copy
the binary and a `.env` file to the server:

```sh
./legion-api migrate        # apply migrations (or set LEGION_MIGRATE_ON_START=true)
./legion-api                # serve
./legion-api create-user -phone 0700000001 -password ... -admin
./legion-api version
```

The binary reads `./.env` from its working directory, or the file named by
`LEGION_ENV_FILE`. Real environment variables override the file. See
`.env.example` for every setting.

## Layout

```
cmd/legion-api   the binary: serve, migrate, create-user, version
db/migrations    goose SQL migrations (embedded)
db/queries       sqlc query files
internal/auth    login, throttling, tokens, refresh rotation
internal/config  .env and environment settings
internal/httpapi chi routes and handlers
internal/store   sqlc generated code (do not edit; run `make generate`)
docs/            schema mapping and API notes
```

## Database

Migrations live in `db/migrations` and run with [goose](https://github.com/pressly/goose):

```sh
goose -dir db/migrations postgres "$DATABASE_URL" up
```

Schema design and the legacy mapping for each slice are in `docs/schema/`.
Auth behaviour is described in `docs/api/auth.md`.
