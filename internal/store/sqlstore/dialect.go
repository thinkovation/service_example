package sqlstore

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	// sqlitelib also registers the database/sql driver "sqlite".
	sqlitelib "modernc.org/sqlite"
)

// Supported dialect names, as accepted by Open and reported by Store.Driver.
const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

// dialect isolates the handful of places where SQLite and Postgres differ.
type dialect interface {
	// name is the dialect name exposed to operators, e.g. "sqlite".
	name() string
	// driverName is the database/sql driver to open.
	driverName() string
	// schema returns the DDL statements that bring an empty database up to date.
	schema() []string
	// placeholder renders the nth (1-based) bind parameter.
	placeholder(n int) string
	// bindTime renders a timestamp for storage.
	bindTime(t time.Time) any
	// lockClause is appended to a SELECT that a read-modify-write depends on.
	lockClause() string
	// isUniqueViolation reports whether err is a uniqueness constraint failure.
	isUniqueViolation(err error) bool
	// tune applies connection-pool settings the dialect requires.
	tune(db *sql.DB)
}

// sqliteTimeLayout is fixed-width and always UTC so that stored timestamps sort
// and compare lexicographically, which is how SQLite compares TEXT columns.
// time.RFC3339Nano would not do: it trims trailing zeros from the fraction.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

type sqliteDialect struct{}

func (sqliteDialect) name() string       { return DialectSQLite }
func (sqliteDialect) driverName() string { return "sqlite" }

func (sqliteDialect) schema() []string {
	return []string{
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
}

func (sqliteDialect) placeholder(int) string { return "?" }

func (sqliteDialect) bindTime(t time.Time) any { return t.UTC().Format(sqliteTimeLayout) }

func (sqliteDialect) lockClause() string { return "" }

func (sqliteDialect) isUniqueViolation(err error) bool {
	var serr *sqlitelib.Error
	if errors.As(err, &serr) {
		// SQLITE_CONSTRAINT_PRIMARYKEY (1555) and SQLITE_CONSTRAINT_UNIQUE (2067).
		return serr.Code() == 1555 || serr.Code() == 2067
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (sqliteDialect) tune(db *sql.DB) {
	// SQLite serialises writers. One connection keeps the dev store free of
	// SQLITE_BUSY retries; it is not meant to carry production concurrency.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
}

type postgresDialect struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
	connMaxIdleTime time.Duration
}

func (postgresDialect) name() string       { return DialectPostgres }
func (postgresDialect) driverName() string { return "pgx" }

func (postgresDialect) schema() []string {
	return []string{
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
}

func (postgresDialect) placeholder(n int) string { return "$" + strconv.Itoa(n) }

func (postgresDialect) bindTime(t time.Time) any { return t.UTC() }

func (postgresDialect) lockClause() string { return " FOR UPDATE" }

func (postgresDialect) isUniqueViolation(err error) bool {
	var perr *pgconn.PgError
	return errors.As(err, &perr) && perr.Code == "23505"
}

func (d postgresDialect) tune(db *sql.DB) {
	db.SetMaxOpenConns(d.maxOpenConns)
	db.SetMaxIdleConns(d.maxIdleConns)
	db.SetConnMaxLifetime(d.connMaxLifetime)
	db.SetConnMaxIdleTime(d.connMaxIdleTime)
}
