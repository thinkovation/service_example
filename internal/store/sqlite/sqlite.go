// Package sqlite stores customers in a SQLite database. It is the development
// driver: no server to install and the database is a file you can delete.
//
// The driver is modernc.org/sqlite, a pure-Go implementation, so builds stay
// free of cgo.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	// Registers the database/sql driver named "sqlite".
	_ "modernc.org/sqlite"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// Driver is the name this store reports and the value STORE_DRIVER takes.
const Driver = "sqlite"

// timeLayout is fixed-width and always UTC. SQLite compares TEXT
// lexicographically, so a format that trims trailing zeros (RFC3339Nano would)
// breaks both range filters and ORDER BY.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// schema is applied by Migrate. Every statement is idempotent, so running it on
// every startup, from several instances at once, is safe.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS customers (
		id        TEXT NOT NULL PRIMARY KEY,
		name      TEXT NOT NULL,
		status    TEXT NOT NULL,
		create_ts TEXT NOT NULL,
		update_ts TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_status ON customers (status)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_create_ts ON customers (create_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_update_ts ON customers (update_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_name_lower ON customers (LOWER(name))`,
}

const (
	insertSQL = `INSERT INTO customers (id, name, status, create_ts, update_ts)
		VALUES (?, ?, ?, ?, ?)`

	selectByIDSQL = `SELECT id, name, status, create_ts, update_ts
		FROM customers WHERE id = ?`

	updateSQL = `UPDATE customers SET name = ?, status = ?, update_ts = ? WHERE id = ?`
)

// Config describes how to reach the database.
type Config struct {
	// DSN is a file path or a file: URL. A bare path is turned into a URL with
	// WAL, a busy timeout and foreign keys enabled.
	DSN string
	// AutoMigrate creates the schema during Open.
	AutoMigrate bool
	// PingTimeout bounds the connectivity check Open performs.
	PingTimeout time.Duration
}

// Store is the SQLite implementation of store.Store.
type Store struct {
	db *sql.DB

	// now and newID are fields so tests can pin time and identifiers.
	now   func() time.Time
	newID func() (uuid.UUID, error)
}

var _ store.Store = (*Store)(nil)

// Open connects to the database, optionally creates the schema, and verifies
// the connection works.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	dsn, err := normalizeDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(Driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}

	// SQLite serialises writers. A single connection keeps the development
	// store free of SQLITE_BUSY retries; it is not built to carry production
	// concurrency, which is what the Postgres driver is for.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: connect: %w", err)
	}

	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewV7}

	if cfg.AutoMigrate {
		if err := s.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// normalizeDSN accepts a file path or a file: URL, creates the containing
// directory, and applies default pragmas unless the caller set their own.
func normalizeDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", errors.New("sqlite: DSN must not be empty")
	}
	if dsn == ":memory:" {
		return dsn, nil
	}
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("sqlite: parse DSN: %w", err)
	}
	query := u.Query()
	inMemory := query.Get("mode") == "memory"

	if !inMemory {
		path := u.Opaque
		if path == "" {
			path = u.Path
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("sqlite: create directory %q: %w", dir, err)
			}
		}
	}
	if len(query["_pragma"]) == 0 {
		query["_pragma"] = []string{"busy_timeout(5000)", "foreign_keys(1)"}
		if !inMemory {
			query["_pragma"] = append(query["_pragma"], "journal_mode(WAL)")
		}
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

// Migrate creates the schema. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: apply schema: %w", err)
		}
	}
	return nil
}

// DB exposes the handle for callers that need their own queries.
func (s *Store) DB() *sql.DB { return s.db }

// Driver implements store.Store.
func (s *Store) Driver() string { return Driver }

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close implements store.Store.
func (s *Store) Close() error { return s.db.Close() }

// Create implements store.Store.
func (s *Store) Create(ctx context.Context, m customer.Mutation) (customer.Customer, error) {
	id, err := s.newID()
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: generate id: %w", err)
	}
	now := s.now()
	c := customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}

	_, err = s.db.ExecContext(ctx, insertSQL,
		c.ID.String(), c.Name, string(c.Status), formatTime(c.CreateTS), formatTime(c.UpdateTS))
	if err != nil {
		if isUniqueViolation(err) {
			return customer.Customer{}, fmt.Errorf("%w: id %s", store.ErrConflict, c.ID)
		}
		return customer.Customer{}, fmt.Errorf("sqlite: insert customer: %w", err)
	}
	return c, nil
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (customer.Customer, error) {
	c, err := scanCustomer(s.db.QueryRowContext(ctx, selectByIDSQL, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return customer.Customer{}, store.ErrNotFound
	}
	return c, err
}

// Replace implements store.Store: an upsert that keeps the original Create_TS.
func (s *Store) Replace(ctx context.Context, id uuid.UUID, m customer.Mutation) (customer.Customer, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return customer.Customer{}, false, fmt.Errorf("sqlite: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the transaction is committed

	existing, err := scanCustomer(tx.QueryRowContext(ctx, selectByIDSQL, id.String()))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := s.now()
		created := customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}

		_, err = tx.ExecContext(ctx, insertSQL, created.ID.String(), created.Name, string(created.Status),
			formatTime(created.CreateTS), formatTime(created.UpdateTS))
		if err != nil {
			return customer.Customer{}, false, fmt.Errorf("sqlite: insert customer: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return customer.Customer{}, false, fmt.Errorf("sqlite: commit transaction: %w", err)
		}
		return created, true, nil

	case err != nil:
		return customer.Customer{}, false, err
	}

	replaced := existing
	replaced.Name = m.Name
	replaced.Status = m.Status
	replaced.UpdateTS = s.now()

	if err := update(ctx, tx, replaced); err != nil {
		return customer.Customer{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return customer.Customer{}, false, fmt.Errorf("sqlite: commit transaction: %w", err)
	}
	return replaced, false, nil
}

// Patch implements store.Store.
func (s *Store) Patch(ctx context.Context, id uuid.UUID, p customer.Patch) (customer.Customer, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the transaction is committed

	patched, err := scanCustomer(tx.QueryRowContext(ctx, selectByIDSQL, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return customer.Customer{}, store.ErrNotFound
	}
	if err != nil {
		return customer.Customer{}, err
	}

	if p.Name != nil {
		patched.Name = *p.Name
	}
	if p.Status != nil {
		patched.Status = *p.Status
	}
	patched.UpdateTS = s.now()

	if err := update(ctx, tx, patched); err != nil {
		return customer.Customer{}, err
	}
	if err := tx.Commit(); err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: commit transaction: %w", err)
	}
	return patched, nil
}

// Query implements store.Store.
func (s *Store) Query(ctx context.Context, q customer.Query) (customer.Page, error) {
	where, args := filter(q)

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM customers"+where, args...).Scan(&total); err != nil {
		return customer.Page{}, fmt.Errorf("sqlite: count customers: %w", err)
	}

	page := customer.Page{Items: []customer.Customer{}, Total: total, Limit: q.Limit, Offset: q.Offset}
	if total == 0 || q.Offset >= total {
		return page, nil
	}

	listSQL := "SELECT id, name, status, create_ts, update_ts FROM customers" + where +
		" ORDER BY " + orderBy(q) + " LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return customer.Page{}, fmt.Errorf("sqlite: query customers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		c, err := scanCustomer(rows)
		if err != nil {
			return customer.Page{}, err
		}
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return customer.Page{}, fmt.Errorf("sqlite: query customers: %w", err)
	}
	return page, nil
}

// update writes the mutable columns of c. The row was read in the same
// transaction, so no rows affected means it was deleted meanwhile.
func update(ctx context.Context, tx *sql.Tx, c customer.Customer) error {
	res, err := tx.ExecContext(ctx, updateSQL, c.Name, string(c.Status), formatTime(c.UpdateTS), c.ID.String())
	if err != nil {
		return fmt.Errorf("sqlite: update customer: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// filter renders the WHERE clause shared by the count and the list query.
func filter(q customer.Query) (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if len(q.IDs) > 0 {
		placeholders := make([]string, len(q.IDs))
		for i, id := range q.IDs {
			placeholders[i] = "?"
			args = append(args, id.String())
		}
		clauses = append(clauses, "id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.Name != nil && *q.Name != "" {
		clauses = append(clauses, "LOWER(name) = LOWER(?)")
		args = append(args, *q.Name)
	}
	if q.NameContains != nil && *q.NameContains != "" {
		clauses = append(clauses, `LOWER(name) LIKE LOWER(?) ESCAPE '\'`)
		args = append(args, "%"+escapeLike(*q.NameContains)+"%")
	}
	if len(q.Status) > 0 {
		placeholders := make([]string, len(q.Status))
		for i, status := range q.Status {
			placeholders[i] = "?"
			args = append(args, string(status))
		}
		clauses = append(clauses, "status IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.CreatedAfter != nil {
		clauses = append(clauses, "create_ts >= ?")
		args = append(args, formatTime(*q.CreatedAfter))
	}
	if q.CreatedBefore != nil {
		clauses = append(clauses, "create_ts <= ?")
		args = append(args, formatTime(*q.CreatedBefore))
	}
	if q.UpdatedAfter != nil {
		clauses = append(clauses, "update_ts >= ?")
		args = append(args, formatTime(*q.UpdatedAfter))
	}
	if q.UpdatedBefore != nil {
		clauses = append(clauses, "update_ts <= ?")
		args = append(args, formatTime(*q.UpdatedBefore))
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// orderBy renders the sort clause. customer.Query.Normalize has already checked
// the field against its whitelist, so nothing unvetted reaches the SQL.
func orderBy(q customer.Query) string {
	column := string(q.Sort)
	if q.Sort == customer.SortName {
		column = "LOWER(name)"
	}
	direction := "ASC"
	if q.Desc {
		direction = "DESC"
	}
	// ids are UUID v7, so breaking ties on id means creation order, and paging
	// stays stable between requests.
	return column + " " + direction + ", id ASC"
}

// escapeLike neutralises LIKE wildcards in a caller-supplied fragment.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCustomer(row rowScanner) (customer.Customer, error) {
	var id, name, status, createTS, updateTS string
	if err := row.Scan(&id, &name, &status, &createTS, &updateTS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return customer.Customer{}, err
		}
		return customer.Customer{}, fmt.Errorf("sqlite: scan customer: %w", err)
	}

	parsedID, err := uuid.Parse(id)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: decode id %q: %w", id, err)
	}
	created, err := parseTime(createTS)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: decode create_ts: %w", err)
	}
	updated, err := parseTime(updateTS)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlite: decode update_ts: %w", err)
	}

	return customer.Customer{
		ID:       parsedID,
		Name:     name,
		Status:   customer.Status(status),
		CreateTS: created,
		UpdateTS: updated,
	}, nil
}

// parseTime reads a stored timestamp. RFC 3339 is accepted as well as our own
// layout, so rows written by hand or by another tool still load.
func parseTime(value string) (time.Time, error) {
	for _, layout := range []string{timeLayout, time.RFC3339Nano} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", value)
}

// isUniqueViolation reports whether err is a uniqueness constraint failure.
func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed: UNIQUE")
}
