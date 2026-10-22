# service_example — customer microservice

A small, production-shaped Go microservice exposing `/status`, `/zhealth` and a
`/customers` resource. It runs on SQLite for local development and on Postgres
everywhere else, behind one storage interface.

A customer is:

| Field       | Type                  | Notes                                            |
|-------------|-----------------------|--------------------------------------------------|
| `id`        | UUID v7               | Assigned by the service; time-ordered            |
| `name`      | string (1–256 chars)  | Trimmed; required                                |
| `status`    | enum                  | `pending` (default), `active`, `inactive`, `suspended` |
| `create_ts` | RFC 3339 timestamp    | Set on insert, never changed                     |
| `update_ts` | RFC 3339 timestamp    | Set on every write                               |

```json
{
  "id": "01a164a4-4169-727f-933f-6a43e711d485",
  "name": "Acme Ltd",
  "status": "active",
  "create_ts": "2026-10-22T17:24:08.681164146Z",
  "update_ts": "2026-10-22T17:24:08.681164146Z"
}
```

## Running it

Three modes, one binary. They differ only in where the service runs and which
database it talks to.

Commands are shown as `make` targets. On Windows, where make usually is not
installed, `scripts\run.ps1` takes the same command names
(`.\scripts\run.ps1 stage-up`), and VS Code users can run the same things from
**Tasks: Run Task**. See [Windows and VS Code](#windows-and-vs-code).

| Mode  | Runs                          | Database                     | Start with       |
|-------|-------------------------------|------------------------------|------------------|
| dev   | on your machine (`go run`)    | local SQLite file            | `make dev`       |
| stage | service container             | Postgres container beside it | `make stage-up`  |
| prod  | service container image       | whatever `DATABASE_URL` names | `make prod-build` |

### Dev — local machine, local SQLite

Requires Go 1.24 or newer. Nothing else: the SQLite driver is pure Go, so there
is no cgo, no toolchain, no server to install.

```bash
make dev          # :8080, ./data/customers.db, text logs at debug level
make dev-smoke    # exercise every endpoint against it
make dev-reset    # throw the database away and start clean
```

```bash
curl localhost:8080/status
curl -X POST localhost:8080/customers \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Ltd","status":"active"}'
```

`make dev-build` produces `./bin/customersvc` with version stamps if you want
the binary rather than `go run`, and `make dev-run` builds and runs it.

### Stage — service container plus Postgres container

`deploy/compose.stage.yaml` builds the service image from the repository and
starts it alongside Postgres on a private network. The service waits for the
database to pass its health check before starting, then creates its schema, so
a cold `make stage-up` lands on a working stack with no manual steps.

```bash
make stage-up      # build and start, waits until both containers are healthy
make stage-smoke   # exercise every endpoint against the stack
make stage-logs    # follow the service log
make stage-psql    # psql into the stage database
make stage-down    # stop, keeping the data volume
make stage-clean   # stop and delete the data volume
```

The service is published on `localhost:8080` and Postgres on `localhost:5432`,
both overridable (`make stage-up HTTP_PORT=9000 POSTGRES_PORT=55432`). Service
settings live in `deploy/stage.env`; the database URL is assembled in the
compose file from the `POSTGRES_*` values so the credentials have one source.
Data survives `stage-down` in a named volume.

Because Postgres is published, this is also how to run the full test suite
against both engines:

```bash
make stage-up
make test-postgres
```

### Prod — the service as an image

Production builds the service alone. The database is external — a managed
Postgres, an operator-run cluster — and reached through `DATABASE_URL`.

```bash
make prod-build                        # customersvc:<version> and :latest
make prod-push REGISTRY=ghcr.io/thinkovation
```

The image is a distroless static build running as a non-root user, with no
shell and a read-only root filesystem in the compose file. Its `HEALTHCHECK`
is the binary probing its own `/zhealth` (`customersvc healthcheck`), since
there is no `curl` or `wget` inside to call.

To run that image locally:

```bash
cp deploy/prod.env.example deploy/prod.env   # then fill in DATABASE_URL
make prod-up
make prod-down
```

Or hand it to whatever runs your containers:

```bash
docker run --rm -p 8080:8080 \
  -e STORE_DRIVER=postgres \
  -e DATABASE_URL='postgres://user:pass@db.internal:5432/customers?sslmode=require' \
  customersvc:latest
```

Version, commit and build date are stamped in at link time and reported by
`/status` and `customersvc version`, so a running container can always be
traced back to a commit.

## Endpoints

| Method  | Path                | Purpose                                            |
|---------|---------------------|----------------------------------------------------|
| `GET`   | `/status`           | Process identity, build info, uptime, store driver |
| `GET`   | `/zhealth`          | Readiness probe: pings the database                |
| `GET`   | `/customers`        | List/search using query-string filters             |
| `POST`  | `/customers`        | Create a customer                                  |
| `QUERY` | `/customers`        | Search using a JSON filter body                    |
| `POST`  | `/customers/query`  | Same search, for clients that cannot send `QUERY`  |
| `GET`   | `/customers/{id}`   | Fetch one customer                                 |
| `PUT`   | `/customers/{id}`   | Replace a customer (creates it if the id is new)   |
| `PATCH` | `/customers/{id}`   | Update the fields the body sets                    |
| `GET`   | `/swagger`          | Swagger UI, and the OpenAPI description it reads   |

`HEAD` and `OPTIONS` are handled for every path; anything else gets a `405`
with an `Allow` header.

### `GET /status`

Always `200` while the process is alive — it touches no dependency, so it is a
liveness signal, not a readiness one.

```json
{
  "service": "customer-service",
  "env": "development",
  "status": "ok",
  "build": {"version": "v1.0.0", "commit": "1bd5bac", "go_version": "go1.24.7"},
  "started_at": "2026-10-22T17:23:49.109363686Z",
  "uptime_seconds": 9.54,
  "now": "2026-10-22T17:23:58.650238971Z",
  "store": {"driver": "sqlite"}
}
```

### `GET /zhealth`

`200` when every check passes, `503` when one fails, and `Cache-Control:
no-store` either way. Point a readiness probe here.

```json
{
  "status": "ok",
  "service": "customer-service",
  "checked_at": "2026-10-22T17:23:58.694579154Z",
  "checks": [{"name": "database", "status": "ok", "detail": "sqlite", "latency_ms": 0.076}]
}
```

### Creating, replacing and patching

```bash
# Create — the service assigns id, create_ts and update_ts
curl -X POST localhost:8080/customers \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Ltd","status":"active"}'
# 201 Created, Location: /customers/{id}

# Replace every client-settable field
curl -X PUT localhost:8080/customers/$ID \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Limited","status":"active"}'
# 200 OK; create_ts is preserved, update_ts moves

# Change one field
curl -X PATCH localhost:8080/customers/$ID \
  -H 'Content-Type: application/json' \
  -d '{"status":"suspended"}'
# 200 OK
```

`PUT` to an unknown id creates the customer under that id and answers `201` with
a `Location` header, which is what PUT is defined to do. `PATCH` leaves out
whatever the body leaves out; a body setting nothing is rejected rather than
silently bumping `update_ts`.

### Searching

`QUERY` is the HTTP method for a safe, idempotent read whose parameters are too
rich for a URL ([draft-ietf-httpbis-safe-method-w-body]). `POST /customers/query`
is an identical alias for clients, proxies or SDKs that cannot send it.

```bash
curl -X QUERY localhost:8080/customers \
  -H 'Content-Type: application/json' \
  -d '{
        "name_contains": "acme",
        "status": ["active", "pending"],
        "created_after": "2026-01-01T00:00:00Z",
        "sort": "name",
        "order": "asc",
        "limit": 25,
        "offset": 0
      }'
```

An empty body matches everything. The same filters are available on
`GET /customers` as query-string parameters:

```bash
curl 'localhost:8080/customers?status=active,pending&name_contains=acme&limit=25&sort=name&order=asc'
```

| Filter                                            | Meaning                                                  |
|---------------------------------------------------|----------------------------------------------------------|
| `ids` (body) / `id`, `ids` (URL)                  | Restrict to these UUIDs; repeated or comma-separated      |
| `name`                                            | Whole name, case-insensitive                              |
| `name_contains` (URL also accepts `q`)            | Substring, case-insensitive; `%` and `_` are literal      |
| `status`                                          | One or more statuses; repeated or comma-separated         |
| `created_after` / `created_before`                | Inclusive bounds on `create_ts`, RFC 3339                 |
| `updated_after` / `updated_before`                | Inclusive bounds on `update_ts`, RFC 3339                 |
| `sort`                                            | `create_ts` (default), `update_ts`, `name`, `status`      |
| `order`                                           | `asc` (default) or `desc`                                 |
| `limit` / `offset`                                | Defaults from `PAGE_SIZE_DEFAULT`, capped by `PAGE_SIZE_MAX` |

Responses carry the page and the full match count, so a client can page without
a second call:

```json
{"items": [ /* ... */ ], "total": 3, "limit": 50, "offset": 0}
```

Ties on the sort key are broken by `id`, and since ids are UUID v7 that is
creation order — paging is stable across requests.

### Errors

Every failure uses one envelope. `code` is stable and safe to branch on;
`message` is for humans. Validation failures list every bad field at once
rather than one per round trip.

```json
{
  "error": {
    "code": "validation_failed",
    "message": "one or more fields are invalid",
    "details": [
      {"field": "name", "message": "must not be empty"},
      {"field": "status", "message": "must be one of: pending, active, inactive, suspended"}
    ],
    "request_id": "6ef92116-276f-4e9b-9d76-014420bf51e2"
  }
}
```

| Code                     | Status | Raised when                                      |
|--------------------------|--------|--------------------------------------------------|
| `validation_failed`      | 400    | A field is missing, malformed or out of range    |
| `invalid_json`           | 400    | Body is not a single valid JSON object, or has unknown fields |
| `invalid_id`             | 400    | The `{id}` path segment is not a UUID            |
| `not_found`              | 404    | No such customer, or no such route               |
| `method_not_allowed`     | 405    | Method not supported on that path                |
| `unsupported_media_type` | 415    | `Content-Type` is not `application/json`         |
| `payload_too_large`      | 413    | Body exceeds `HTTP_MAX_BODY_BYTES`               |
| `conflict`               | 409    | A write collided with an existing record         |
| `request_timeout`        | 504    | The request exceeded `HTTP_REQUEST_TIMEOUT`      |
| `internal_error`         | 500    | Anything unexpected; details stay in the logs    |

Unknown JSON fields are rejected rather than ignored, so a typo in a client
payload fails loudly instead of silently doing nothing.

### Request correlation

Every response carries `X-Request-Id`. An inbound value is reused so a gateway's
correlation id survives into the logs; otherwise one is generated. Error bodies
repeat it.

## API documentation

`http://localhost:8080/swagger` is Swagger UI, with every endpoint listed and
**Try it out** wired to the service you are looking at. The description it
reads is at `/swagger/openapi.yaml`, and the same file is in the repository at
[`api/openapi.yaml`](api/openapi.yaml) for anything that generates clients or
checks contracts.

The UI is served from the binary — `swagger-ui.css` and `swagger-ui-bundle.js`
are vendored under `internal/httpapi/swaggerui` — so the page works on a
machine with no internet access, and the service never serves third-party code
from its own origin. That costs about 1.9 MB in the repository and in the
binary. `internal/httpapi/swaggerui/README.md` says which release they came
from and how to update them.

The description is hand-written rather than generated from annotations, which
means it can drift from the code. `TestSpecMatchesRoutes` is the guard: it
fails if a route is missing from the description, if the description names a
route that does not exist, or if it documents a path with a method the router
does not accept.

One endpoint cannot be described properly: OpenAPI 3.0 has no way to express a
method outside its fixed list, so `QUERY /customers` appears in the
description's prose and as its identical `POST /customers/query` alias, which
is the operation **Try it out** will exercise.

If you would rather not expose the docs publicly, they are four `GET` routes
under `/swagger/` — block the prefix at your gateway.

## Windows and VS Code

Nothing here needs make. The service is plain Go and the containers are plain
`docker compose`, so there are three equivalent ways to drive it:

| Task                 | make                 | PowerShell                          | VS Code task             |
|----------------------|----------------------|-------------------------------------|--------------------------|
| Run against SQLite   | `make dev`           | `.\scripts\run.ps1 dev`             | dev: run service (SQLite)|
| Smoke test it        | `make dev-smoke`     | `.\scripts\run.ps1 dev-smoke`       | dev: smoke test          |
| Tests                | `make test`          | `.\scripts\run.ps1 test`            | test                     |
| Start the stage stack| `make stage-up`      | `.\scripts\run.ps1 stage-up`        | stage: up                |
| Stop it              | `make stage-down`    | `.\scripts\run.ps1 stage-down`      | stage: down              |
| Build the prod image | `make prod-build`    | `.\scripts\run.ps1 prod-build`      | prod: build image        |

`.\scripts\run.ps1 help` lists every command; it takes `-Port`, `-Image` and
`-Registry` where those apply. In VS Code the tasks are under **Ctrl+Shift+P →
Tasks: Run Task**, and **F5** runs the service under the debugger (the
*Debug service (SQLite)* configuration; there is a Postgres one too, for use
with the stage stack).

Or skip all of it and use the Go toolchain directly, which is all any of the
above does:

```powershell
$env:STORE_DRIVER="sqlite"; $env:SQLITE_DSN="file:./data/customers.db"
go run ./cmd/customersvc
go run ./cmd/smoke http://localhost:8080
go test ./...
docker compose -f deploy/compose.stage.yaml up --build -d --wait
```

Windows specifics worth knowing:

- Requires Go, and Docker Desktop for the stage and prod modes. No C compiler:
  the SQLite driver is pure Go, so `go build` works on a bare Windows box.
- `.gitattributes` checks everything out with LF except `.ps1`, so files that
  end up inside the Linux containers are not broken by CRLF.
- WSL works too, with the make targets, and so does a mix: editing in Windows
  VS Code with the terminal in WSL Ubuntu. The one thing not to do is share a
  `./data/customers.db` across both at once — SQLite file locking over the
  WSL/Windows filesystem boundary is unreliable. `make dev-reset` or
  `.\scripts\run.ps1 dev-reset` clears it.

## Configuration

All configuration is environment variables. Each mode has its own source: dev
reads an optional `.env` (copy `.env.example`), stage reads `deploy/stage.env`,
prod reads `deploy/prod.env`. Defaults are production-safe apart from the
storage driver.

| Variable                | Default                   | Purpose                                      |
|-------------------------|---------------------------|----------------------------------------------|
| `SERVICE_NAME`          | `customer-service`        | Reported by `/status` and attached to logs   |
| `SERVICE_ENV`           | `development`             | Environment label                            |
| `HTTP_ADDR`             | `:8080`                   | Listen address                               |
| `HTTP_READ_TIMEOUT`     | `15s`                     | Header and body read timeout                 |
| `HTTP_WRITE_TIMEOUT`    | `30s`                     | Response write timeout                       |
| `HTTP_IDLE_TIMEOUT`     | `60s`                     | Keep-alive idle timeout                      |
| `HTTP_REQUEST_TIMEOUT`  | `10s`                     | Per-request deadline (`0` disables)          |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s`                     | Grace period for in-flight requests          |
| `HTTP_MAX_BODY_BYTES`   | `1048576`                 | Maximum request body                         |
| `STORE_DRIVER`          | `sqlite`                  | `sqlite` or `postgres`                       |
| `SQLITE_DSN`            | `file:./data/customers.db`| SQLite path or `file:` URL                   |
| `DATABASE_URL`          | —                         | Postgres DSN; required when driver is `postgres` |
| `DB_AUTO_MIGRATE`       | `true`                    | Create the schema at startup                 |
| `DB_MAX_OPEN_CONNS`     | `25`                      | Pool size (Postgres)                         |
| `DB_MAX_IDLE_CONNS`     | `5`                       | Idle pool size (Postgres)                    |
| `DB_CONN_MAX_LIFETIME`  | `1h`                      | Connection lifetime (Postgres)               |
| `DB_CONN_MAX_IDLE_TIME` | `5m`                      | Idle connection lifetime (Postgres)          |
| `DB_PING_TIMEOUT`       | `5s`                      | Startup connect and `/zhealth` check timeout |
| `PAGE_SIZE_DEFAULT`     | `50`                      | Page size when the caller gives none         |
| `PAGE_SIZE_MAX`         | `500`                     | Largest page a caller may ask for            |
| `LOG_LEVEL`             | `info`                    | `debug`, `info`, `warn`, `error`             |
| `LOG_FORMAT`            | `json`                    | `json` or `text`                             |

Misconfiguration fails at startup with every problem listed at once.

## Storage

There are two drivers, one per database, each a package of its own with its own
SQL, its own schema and its own tests:

- `internal/store/sqlite` — the development driver. `modernc.org/sqlite`, a
  pure-Go implementation, so builds stay free of cgo and the image needs no
  shared libraries. Timestamps are stored as fixed-width UTC text (SQLite
  compares TEXT lexicographically, so a format that trimmed trailing zeros
  would break range filters and ordering) and the pool is pinned to one
  connection, since SQLite serialises writers. It is the single-node story.
- `internal/store/postgres` — the stage and production driver. `pgx` through
  `database/sql`, native `uuid` and `timestamptz` columns, a real pool, and
  `SELECT … FOR UPDATE` on the read-modify-write paths so concurrent writers
  cannot interleave.

They share the `store.Store` interface and nothing else. `cmd/customersvc` is
the only place that chooses between them, on `STORE_DRIVER`.

```sql
-- internal/store/postgres
CREATE TABLE customers (
    id        UUID NOT NULL PRIMARY KEY,
    name      TEXT NOT NULL,
    status    TEXT NOT NULL,
    create_ts TIMESTAMPTZ NOT NULL,
    update_ts TIMESTAMPTZ NOT NULL
);
```

```sql
-- internal/store/sqlite
CREATE TABLE customers (
    id        TEXT NOT NULL PRIMARY KEY,
    name      TEXT NOT NULL,
    status    TEXT NOT NULL,
    create_ts TEXT NOT NULL,
    update_ts TEXT NOT NULL
);
```

Each driver applies its own schema at startup when `DB_AUTO_MIGRATE=true`,
using `CREATE TABLE IF NOT EXISTS`, so it is safe on every boot and with
several instances starting at once. Set it to `false` and run the DDL yourself
if your environment manages migrations centrally.

One difference is visible through the API: Postgres keeps timestamps to
microsecond precision where SQLite keeps nanoseconds. The service always
returns UTC RFC 3339 and never relies on sub-microsecond ordering.

## Project layout

```
api/openapi.yaml         the OpenAPI description, embedded and served
cmd/customersvc          process entry point: config, logging, lifecycle
cmd/smoke                end-to-end check of a running instance, any mode
internal/config          environment configuration and validation
internal/customer        domain model, validation, query specification
internal/store           persistence interface and sentinel errors
internal/store/sqlite    the SQLite driver
internal/store/postgres  the Postgres driver
internal/httpapi         routing, middleware, handlers, JSON shapes
internal/httpapi/swaggerui  vendored Swagger UI assets
internal/buildinfo       version stamps injected at link time
deploy/                  compose files and per-mode environment
scripts/run.ps1          the make targets, for Windows
.vscode/                 tasks and debug configurations
```

## Tests and quality

```bash
make check          # gofmt, go vet, go test -race
make test           # tests (Postgres suites skip unless TEST_POSTGRES_DSN is set)
make test-postgres  # tests against SQLite and Postgres (needs make stage-up)
make cover          # coverage summary
```

Each driver has its own test suite covering the same behaviour, so the two are
held to the same contract without sharing an implementation. The Postgres suite
skips unless `TEST_POSTGRES_DSN` is set, which keeps `go test ./...` runnable
with no database at all.

`go run ./cmd/smoke <base-url>` checks a running instance in any mode and exits
non-zero if anything fails, which makes it usable as a post-deploy gate. It is
Go rather than a shell script so it runs the same everywhere.

[draft-ietf-httpbis-safe-method-w-body]: https://datatracker.ietf.org/doc/draft-ietf-httpbis-safe-method-w-body/
