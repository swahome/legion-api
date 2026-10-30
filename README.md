# legion-api

Go API for Legion, the rewrite of the LegionCore (Chamasoft) group savings platform.

## Database

Migrations live in `db/migrations` and run with [goose](https://github.com/pressly/goose):

```sh
goose -dir db/migrations postgres "$DATABASE_URL" up
```

Schema design and the legacy mapping for each slice are in `docs/schema/`.
