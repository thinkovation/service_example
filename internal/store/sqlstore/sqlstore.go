// Package sqlstore implements store.Store on top of database/sql. One
// implementation serves both supported engines: SQLite for development and
// Postgres for everything else. Only the few genuinely engine-specific details
// (bind parameters, timestamp encoding, DDL, row locking) live behind the
// dialect interface in dialect.go.
package sqlstore

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

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// Config describes how to reach the database.
type Config struct {
	// Dialect is DialectSQLite or DialectPostgres.
	Dialect string
	// DSN is the engine-specific connection string. For SQLite a bare path is
	// accepted and turned into a file: URL with sensible pragmas.
	DSN string
	// Pool settings; Postgres only, SQLite is pinned to a single connection.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// AutoMigrate creates the schema at startup when true.
	AutoMigrate bool
	// PingTimeout bounds the connectivity check performed by Open.
	PingTimeout time.Duration
}

// Store is a database/sql-backed implementation of store.Store.
type Store struct {
	db *sql.DB
	d  dialect

	// now and newID are injectable so tests can pin time and identifiers.
	now   func() time.Time
	newID func() (uuid.UUID, error)
}

var _ store.Store = (*Store)(nil)

const columns = "id, name, status, create_ts, update_ts"

// Open connects to the configured database, optionally applies the schema, and
// verifies connectivity.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	var (
		d   dialect
		dsn = cfg.DSN
		err error
	)

	switch cfg.Dialect {
	case DialectSQLite:
		d = sqliteDialect{}
		if dsn, err = normalizeSQLiteDSN(dsn); err != nil {
			return nil, err
		}
	case DialectPostgres:
		if strings.TrimSpace(dsn) == "" {
			return nil, errors.New("sqlstore: postgres DSN must not be empty")
		}
		d = postgresDialect{
			maxOpenConns:    cfg.MaxOpenConns,
			maxIdleConns:    cfg.MaxIdleConns,
			connMaxLifetime: cfg.ConnMaxLifetime,
			connMaxIdleTime: cfg.ConnMaxIdleTime,
		}
	default:
		return nil, fmt.Errorf("sqlstore: unsupported dialect %q (want %q or %q)",
			cfg.Dialect, DialectSQLite, DialectPostgres)
	}

	db, err := sql.Open(d.driverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open %s: %w", d.name(), err)
	}
	d.tune(db)

	s := &Store{db: db, d: d, now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewV7}

	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlstore: connect to %s: %w", d.name(), err)
	}

	if cfg.AutoMigrate {
		if err := s.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// normalizeSQLiteDSN accepts either a file path or a file: URL, makes sure the
// containing directory exists, and applies default pragmas when the caller has
// not set any of their own.
func normalizeSQLiteDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", errors.New("sqlstore: sqlite DSN must not be empty")
	}
	if dsn == ":memory:" {
		return dsn, nil
	}
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("sqlstore: parse sqlite DSN: %w", err)
	}
	q := u.Query()
	inMemory := q.Get("mode") == "memory"

	if !inMemory {
		path := u.Opaque
		if path == "" {
			path = u.Path
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("sqlstore: create sqlite directory %q: %w", dir, err)
			}
		}
	}
	if len(q["_pragma"]) == 0 {
		q["_pragma"] = []string{"busy_timeout(5000)", "foreign_keys(1)"}
		if !inMemory {
			q["_pragma"] = append(q["_pragma"], "journal_mode(WAL)")
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Migrate applies the schema. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range s.d.schema() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlstore: apply schema: %w", err)
		}
	}
	return nil
}

// DB exposes the underlying handle, for callers that need their own queries.
func (s *Store) DB() *sql.DB { return s.db }

// Driver implements store.Store.
func (s *Store) Driver() string { return s.d.name() }

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close implements store.Store.
func (s *Store) Close() error { return s.db.Close() }

// Create implements store.Store.
func (s *Store) Create(ctx context.Context, m customer.Mutation) (customer.Customer, error) {
	id, err := s.newID()
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlstore: generate id: %w", err)
	}
	now := s.now()
	c := customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}

	if err := s.insert(ctx, s.db, c); err != nil {
		return customer.Customer{}, err
	}
	return c, nil
}

// Replace implements store.Store. It is an upsert: the row is created when the
// id is unknown, and Create_TS is preserved when it is not.
func (s *Store) Replace(ctx context.Context, id uuid.UUID, m customer.Mutation) (customer.Customer, bool, error) {
	var (
		result  customer.Customer
		created bool
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getTx(ctx, tx, id, true)
		switch {
		case errors.Is(err, store.ErrNotFound):
			now := s.now()
			result = customer.Customer{ID: id, Name: m.Name, Status: m.Status, CreateTS: now, UpdateTS: now}
			created = true
			return s.insert(ctx, tx, result)
		case err != nil:
			return err
		}

		result = existing
		result.Name = m.Name
		result.Status = m.Status
		result.UpdateTS = s.now()
		return s.update(ctx, tx, result)
	})
	if err != nil {
		return customer.Customer{}, false, err
	}
	return result, created, nil
}

// Patch implements store.Store.
func (s *Store) Patch(ctx context.Context, id uuid.UUID, p customer.Patch) (customer.Customer, error) {
	var result customer.Customer
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		result = existing
		if p.Name != nil {
			result.Name = *p.Name
		}
		if p.Status != nil {
			result.Status = *p.Status
		}
		result.UpdateTS = s.now()
		return s.update(ctx, tx, result)
	})
	if err != nil {
		return customer.Customer{}, err
	}
	return result, nil
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (customer.Customer, error) {
	q := "SELECT " + columns + " FROM customers WHERE id = " + s.d.placeholder(1)
	return s.scanOne(s.db.QueryRowContext(ctx, q, id.String()))
}

// Query implements store.Store.
func (s *Store) Query(ctx context.Context, q customer.Query) (customer.Page, error) {
	where, args := s.where(q)

	var total int
	countSQL := "SELECT COUNT(*) FROM customers" + where
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return customer.Page{}, fmt.Errorf("sqlstore: count customers: %w", err)
	}

	page := customer.Page{Items: []customer.Customer{}, Total: total, Limit: q.Limit, Offset: q.Offset}
	if total == 0 || q.Offset >= total {
		return page, nil
	}

	listSQL := "SELECT " + columns + " FROM customers" + where +
		" ORDER BY " + orderBy(q) +
		" LIMIT " + s.d.placeholder(len(args)+1) +
		" OFFSET " + s.d.placeholder(len(args)+2)
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return customer.Page{}, fmt.Errorf("sqlstore: query customers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		c, err := s.scanRow(rows)
		if err != nil {
			return customer.Page{}, err
		}
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return customer.Page{}, fmt.Errorf("sqlstore: query customers: %w", err)
	}
	return page, nil
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) insert(ctx context.Context, e execer, c customer.Customer) error {
	q := fmt.Sprintf("INSERT INTO customers (%s) VALUES (%s, %s, %s, %s, %s)",
		columns,
		s.d.placeholder(1), s.d.placeholder(2), s.d.placeholder(3), s.d.placeholder(4), s.d.placeholder(5))

	_, err := e.ExecContext(ctx, q,
		c.ID.String(), c.Name, string(c.Status), s.d.bindTime(c.CreateTS), s.d.bindTime(c.UpdateTS))
	if err != nil {
		if s.d.isUniqueViolation(err) {
			return fmt.Errorf("%w: id %s", store.ErrConflict, c.ID)
		}
		return fmt.Errorf("sqlstore: insert customer: %w", err)
	}
	return nil
}

func (s *Store) update(ctx context.Context, e execer, c customer.Customer) error {
	q := fmt.Sprintf("UPDATE customers SET name = %s, status = %s, update_ts = %s WHERE id = %s",
		s.d.placeholder(1), s.d.placeholder(2), s.d.placeholder(3), s.d.placeholder(4))

	res, err := e.ExecContext(ctx, q, c.Name, string(c.Status), s.d.bindTime(c.UpdateTS), c.ID.String())
	if err != nil {
		return fmt.Errorf("sqlstore: update customer: %w", err)
	}
	// The row was selected for update in the same transaction, so a missing row
	// here means it was deleted concurrently.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) getTx(ctx context.Context, tx *sql.Tx, id uuid.UUID, forUpdate bool) (customer.Customer, error) {
	q := "SELECT " + columns + " FROM customers WHERE id = " + s.d.placeholder(1)
	if forUpdate {
		q += s.d.lockClause()
	}
	return s.scanOne(tx.QueryRowContext(ctx, q, id.String()))
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: commit transaction: %w", err)
	}
	return nil
}

// where renders the filter clause shared by the count and list queries.
func (s *Store) where(q customer.Query) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	bind := func(v any) string {
		args = append(args, v)
		return s.d.placeholder(len(args))
	}

	if len(q.IDs) > 0 {
		placeholders := make([]string, 0, len(q.IDs))
		for _, id := range q.IDs {
			placeholders = append(placeholders, bind(id.String()))
		}
		clauses = append(clauses, "id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.Name != nil && *q.Name != "" {
		clauses = append(clauses, "LOWER(name) = LOWER("+bind(*q.Name)+")")
	}
	if q.NameContains != nil && *q.NameContains != "" {
		clauses = append(clauses,
			"LOWER(name) LIKE LOWER("+bind("%"+escapeLike(*q.NameContains)+"%")+") ESCAPE '\\'")
	}
	if len(q.Status) > 0 {
		placeholders := make([]string, 0, len(q.Status))
		for _, st := range q.Status {
			placeholders = append(placeholders, bind(string(st)))
		}
		clauses = append(clauses, "status IN ("+strings.Join(placeholders, ", ")+")")
	}
	for _, f := range []struct {
		column string
		op     string
		value  *time.Time
	}{
		{"create_ts", ">=", q.CreatedAfter},
		{"create_ts", "<=", q.CreatedBefore},
		{"update_ts", ">=", q.UpdatedAfter},
		{"update_ts", "<=", q.UpdatedBefore},
	} {
		if f.value != nil {
			clauses = append(clauses, f.column+" "+f.op+" "+bind(s.d.bindTime(*f.value)))
		}
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// orderBy renders the sort clause. The sort field is validated by
// customer.Query.Normalize, so it is never interpolated unchecked.
func orderBy(q customer.Query) string {
	column := string(q.Sort)
	if q.Sort == customer.SortName {
		column = "LOWER(name)"
	}
	direction := "ASC"
	if q.Desc {
		direction = "DESC"
	}
	// id is the tie-breaker: UUID v7 is time-ordered, so equal sort keys come
	// back in a stable, creation-ordered sequence.
	return column + " " + direction + ", id ASC"
}

// escapeLike neutralises LIKE wildcards in a user-supplied fragment.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanOne(row rowScanner) (customer.Customer, error) {
	c, err := s.scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return customer.Customer{}, store.ErrNotFound
	}
	return c, err
}

func (s *Store) scanRow(row rowScanner) (customer.Customer, error) {
	var (
		c                    customer.Customer
		rawID                any
		status               string
		rawCreate, rawUpdate any
	)
	if err := row.Scan(&rawID, &c.Name, &status, &rawCreate, &rawUpdate); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return customer.Customer{}, err
		}
		return customer.Customer{}, fmt.Errorf("sqlstore: scan customer: %w", err)
	}

	id, err := asUUID(rawID)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlstore: decode id: %w", err)
	}
	createTS, err := asTime(rawCreate)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlstore: decode create_ts: %w", err)
	}
	updateTS, err := asTime(rawUpdate)
	if err != nil {
		return customer.Customer{}, fmt.Errorf("sqlstore: decode update_ts: %w", err)
	}

	c.ID = id
	c.Status = customer.Status(status)
	c.CreateTS = createTS
	c.UpdateTS = updateTS
	return c, nil
}

// asUUID accepts the representations the two drivers return for an id column.
func asUUID(v any) (uuid.UUID, error) {
	switch value := v.(type) {
	case uuid.UUID:
		return value, nil
	case string:
		return uuid.Parse(value)
	case []byte:
		return uuid.ParseBytes(value)
	case [16]byte:
		return uuid.UUID(value), nil
	default:
		return uuid.Nil, fmt.Errorf("unexpected type %T", v)
	}
}

// asTime accepts the representations the two drivers return for a timestamp
// column: time.Time from Postgres, text from SQLite.
func asTime(v any) (time.Time, error) {
	switch value := v.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseTime(value)
	case []byte:
		return parseTime(string(value))
	default:
		return time.Time{}, fmt.Errorf("unexpected type %T", v)
	}
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{sqliteTimeLayout, time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}
