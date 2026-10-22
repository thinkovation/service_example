// Package postgres stores customers in PostgreSQL. It is the driver for stage
// and production: a real connection pool, native uuid and timestamptz columns,
// and row locking on the read-modify-write paths.
//
// The driver is jackc/pgx through database/sql.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	// Registers the database/sql driver named "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// Driver is the name this store reports and the value STORE_DRIVER takes.
const Driver = "postgres"

// driverName is what database/sql knows pgx as.
const driverName = "pgx"

// uniqueViolation is the SQLSTATE code Postgres raises for a duplicate key.
const uniqueViolation = "23505"

// schema is applied by Migrate. Every statement is idempotent, so running it on
// every startup, from several instances at once, is safe.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS customers (
		id        UUID NOT NULL PRIMARY KEY,
		name      TEXT NOT NULL,
		status    TEXT NOT NULL,
		create_ts TIMESTAMPTZ NOT NULL,
		update_ts TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_status ON customers (status)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_create_ts ON customers (create_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_update_ts ON customers (update_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_customers_name_lower ON customers (LOWER(name))`,
}

const (
	insertSQL = `INSERT INTO customers (id, name, status, create_ts, update_ts)
		VALUES ($1, $2, $3, $4, $5)`

	selectByIDSQL = `SELECT id, name, status, create_ts, update_ts
		FROM customers WHERE id = $1`

	// Taken inside a transaction before a write, so two writers cannot
	// interleave a read-modify-write on the same row.
	selectForUpdateSQL = selectByIDSQL + ` FOR UPDATE`

	updateSQL = `UPDATE customers SET name = $1, status = $2, update_ts = $3 WHERE id = $4`
)

// Config describes how to reach the database and how to size the pool.
type Config struct {
	// DSN is a Postgres connection URL or key/value string.
	DSN string
	// AutoMigrate creates the schema during Open.
	AutoMigrate bool
	// PingTimeout bounds the connectivity check Open performs.
	PingTimeout time.Duration

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// Store is the Postgres implementation of store.Store.
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
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("postgres: DSN must not be empty")
	}

	db, err := sql.Open(driverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
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

// Migrate creates the schema. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("postgres: apply schema: %w", err)
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
		return customer.Customer{}, fmt.Errorf("postgres: generate id: %w", err)
	}
	now := s.now()
	c := customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}

	_, err = s.db.ExecContext(ctx, insertSQL, c.ID, c.Name, string(c.Status), c.CreateTS, c.UpdateTS)
	if err != nil {
		if isUniqueViolation(err) {
			return customer.Customer{}, fmt.Errorf("%w: id %s", store.ErrConflict, c.ID)
		}
		return customer.Customer{}, fmt.Errorf("postgres: insert customer: %w", err)
	}
	return c, nil
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (customer.Customer, error) {
	c, err := scanCustomer(s.db.QueryRowContext(ctx, selectByIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return customer.Customer{}, store.ErrNotFound
	}
	return c, err
}

// Replace implements store.Store: an upsert that keeps the original Create_TS.
func (s *Store) Replace(ctx context.Context, id uuid.UUID, m customer.Mutation) (customer.Customer, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return customer.Customer{}, false, fmt.Errorf("postgres: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the transaction is committed

	existing, err := scanCustomer(tx.QueryRowContext(ctx, selectForUpdateSQL, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := s.now()
		created := customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}

		_, err = tx.ExecContext(ctx, insertSQL, created.ID, created.Name, string(created.Status),
			created.CreateTS, created.UpdateTS)
		if err != nil {
			if isUniqueViolation(err) {
				// Another writer inserted this id between the select and here.
				return customer.Customer{}, false, fmt.Errorf("%w: id %s", store.ErrConflict, created.ID)
			}
			return customer.Customer{}, false, fmt.Errorf("postgres: insert customer: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return customer.Customer{}, false, fmt.Errorf("postgres: commit transaction: %w", err)
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
		return customer.Customer{}, false, fmt.Errorf("postgres: commit transaction: %w", err)
	}
	return replaced, false, nil
}

// Patch implements store.Store.
func (s *Store) Patch(ctx context.Context, id uuid.UUID, p customer.Patch) (customer.Customer, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("postgres: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the transaction is committed

	patched, err := scanCustomer(tx.QueryRowContext(ctx, selectForUpdateSQL, id))
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
		return customer.Customer{}, fmt.Errorf("postgres: commit transaction: %w", err)
	}
	return patched, nil
}

// Query implements store.Store.
func (s *Store) Query(ctx context.Context, q customer.Query) (customer.Page, error) {
	where, args := filter(q)

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM customers"+where, args...).Scan(&total); err != nil {
		return customer.Page{}, fmt.Errorf("postgres: count customers: %w", err)
	}

	page := customer.Page{Items: []customer.Customer{}, Total: total, Limit: q.Limit, Offset: q.Offset}
	if total == 0 || q.Offset >= total {
		return page, nil
	}

	listSQL := "SELECT id, name, status, create_ts, update_ts FROM customers" + where +
		" ORDER BY " + orderBy(q) +
		" LIMIT $" + strconv.Itoa(len(args)+1) +
		" OFFSET $" + strconv.Itoa(len(args)+2)
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return customer.Page{}, fmt.Errorf("postgres: query customers: %w", err)
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
		return customer.Page{}, fmt.Errorf("postgres: query customers: %w", err)
	}
	return page, nil
}

// update writes the mutable columns of c. The row is locked by the caller's
// transaction, so no rows affected means it was deleted meanwhile.
func update(ctx context.Context, tx *sql.Tx, c customer.Customer) error {
	res, err := tx.ExecContext(ctx, updateSQL, c.Name, string(c.Status), c.UpdateTS, c.ID)
	if err != nil {
		return fmt.Errorf("postgres: update customer: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// filter renders the WHERE clause shared by the count and the list query, and
// returns the arguments its $n placeholders refer to.
func filter(q customer.Query) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	// bind records an argument and returns the placeholder that reads it back.
	bind := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}

	if len(q.IDs) > 0 {
		placeholders := make([]string, len(q.IDs))
		for i, id := range q.IDs {
			placeholders[i] = bind(id)
		}
		clauses = append(clauses, "id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.Name != nil && *q.Name != "" {
		clauses = append(clauses, "LOWER(name) = LOWER("+bind(*q.Name)+")")
	}
	if q.NameContains != nil && *q.NameContains != "" {
		clauses = append(clauses, `LOWER(name) LIKE LOWER(`+bind("%"+escapeLike(*q.NameContains)+"%")+`) ESCAPE '\'`)
	}
	if len(q.Status) > 0 {
		placeholders := make([]string, len(q.Status))
		for i, status := range q.Status {
			placeholders[i] = bind(string(status))
		}
		clauses = append(clauses, "status IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.CreatedAfter != nil {
		clauses = append(clauses, "create_ts >= "+bind(q.CreatedAfter.UTC()))
	}
	if q.CreatedBefore != nil {
		clauses = append(clauses, "create_ts <= "+bind(q.CreatedBefore.UTC()))
	}
	if q.UpdatedAfter != nil {
		clauses = append(clauses, "update_ts >= "+bind(q.UpdatedAfter.UTC()))
	}
	if q.UpdatedBefore != nil {
		clauses = append(clauses, "update_ts <= "+bind(q.UpdatedBefore.UTC()))
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
		// LOWER() rather than the column's collation, so ordering does not
		// change with the database's locale.
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

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanCustomer reads one row. pgx hands back uuid as a string and timestamptz
// as a time.Time, so the columns map straight onto the domain types.
func scanCustomer(row rowScanner) (customer.Customer, error) {
	var (
		id       string
		name     string
		status   string
		createTS time.Time
		updateTS time.Time
	)
	if err := row.Scan(&id, &name, &status, &createTS, &updateTS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return customer.Customer{}, err
		}
		return customer.Customer{}, fmt.Errorf("postgres: scan customer: %w", err)
	}

	parsedID, err := uuid.Parse(id)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("postgres: decode id %q: %w", id, err)
	}

	return customer.Customer{
		ID:       parsedID,
		Name:     name,
		Status:   customer.Status(status),
		CreateTS: createTS.UTC(),
		UpdateTS: updateTS.UTC(),
	}, nil
}

// isUniqueViolation reports whether err is a uniqueness constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
