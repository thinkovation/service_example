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

## Quick start

Requires Go 1.24 or newer. No cgo, no external services for the dev path.

```bash
make run                      # SQLite at ./data/customers.db, listening on :8080
curl localhost:8080/status
curl -X POST localhost:8080/customers \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Ltd","status":"active"}'
```

Against Postgres:

```bash
make postgres-up              # disposable Postgres in Docker on :5432
make run-postgres             # STORE_DRIVER=postgres, schema created at startup
```

or `docker compose up --build`, which runs the service and Postgres together.

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

## Configuration

All configuration is environment variables; see `.env.example` for a copy-paste
starting point. Defaults are production-safe apart from the storage driver.

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

Both engines are served by one `database/sql` implementation
(`internal/store/sqlstore`); only bind-parameter syntax, timestamp encoding,
DDL and row locking differ, and those sit behind a small `dialect` interface.
Swapping engines is a config change, and the test suite runs the same
conformance tests against both.

```sql
-- Postgres
CREATE TABLE customers (
    id        UUID NOT NULL PRIMARY KEY,
    name      TEXT NOT NULL,
    status    TEXT NOT NULL,
    create_ts TIMESTAMPTZ NOT NULL,
    update_ts TIMESTAMPTZ NOT NULL
);
```

The schema is applied at startup when `DB_AUTO_MIGRATE=true`, using
`CREATE TABLE IF NOT EXISTS`, so it is safe to run on every boot and with
several instances starting at once. Set it to `false` and run the DDL yourself
if your environment manages migrations centrally.

Notes worth knowing:

- **SQLite** uses the pure-Go `modernc.org/sqlite` driver, so builds stay
  cgo-free and the container image is `FROM scratch`-thin. The pool is pinned
  to a single connection (SQLite serialises writers) and the DSN gets WAL,
  `busy_timeout` and foreign keys by default. It is the development and
  single-node story, not a horizontal-scaling one.
- **Postgres** uses `pgx` through `database/sql`. Timestamps keep microsecond
  precision there against SQLite's nanoseconds, which is why the service always
  returns UTC RFC 3339 and never relies on sub-microsecond ordering.
- Read-modify-write paths (`PUT`, `PATCH`) run in a transaction with
  `SELECT … FOR UPDATE` on Postgres, so concurrent writers cannot interleave.

## Project layout

```
cmd/customersvc        process entry point: config, logging, lifecycle
internal/config        environment configuration and validation
internal/customer      domain model, validation, query specification
internal/store         persistence interface and sentinel errors
internal/store/sqlstore  one database/sql implementation, two dialects
internal/httpapi       routing, middleware, handlers, JSON shapes
internal/buildinfo     version stamps injected at link time
```

## Development

```bash
make check          # gofmt, go vet, go test -race
make test           # tests (Postgres suites skip unless TEST_POSTGRES_DSN is set)
make test-postgres  # tests against SQLite and Postgres
make cover          # coverage summary
make build          # ./bin/customersvc with version stamps
```

The store tests run the same suite against every available engine. To include
Postgres:

```bash
make postgres-up
make test-postgres
make postgres-down
```

## Container

```bash
make docker-build
docker run --rm -p 8080:8080 \
  -e STORE_DRIVER=postgres \
  -e DATABASE_URL='postgres://customers:customers@host.docker.internal:5432/customers?sslmode=disable' \
  customersvc:dev
```

The image is a distroless static build running as a non-root user. For the
SQLite path, mount a volume at `/var/lib/customersvc`.

[draft-ietf-httpbis-safe-method-w-body]: https://datatracker.ietf.org/doc/draft-ietf-httpbis-safe-method-w-body/
