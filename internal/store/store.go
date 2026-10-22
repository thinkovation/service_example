// Package store defines the persistence contract for customers. Concrete
// implementations live in sub-packages; sqlstore covers both SQLite and
// Postgres through database/sql.
package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thinkovation/service_example/internal/customer"
)

// ErrNotFound is returned when the requested customer does not exist.
var ErrNotFound = errors.New("customer not found")

// ErrConflict is returned when a write violates a uniqueness constraint.
var ErrConflict = errors.New("customer conflicts with an existing record")

// Store persists customers. Implementations are safe for concurrent use.
//
// Identifiers (UUID v7) and the Create_TS/Update_TS timestamps are owned by the
// implementation, not the caller, so every record is stamped the same way
// whichever transport created it.
type Store interface {
	// Create inserts a new customer and returns it as stored.
	Create(ctx context.Context, m customer.Mutation) (customer.Customer, error)

	// Get returns the customer with the given id, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (customer.Customer, error)

	// Replace sets every field of the customer with the given id. When no such
	// customer exists it is inserted under that id and created is true; the
	// original Create_TS is preserved on replacement.
	Replace(ctx context.Context, id uuid.UUID, m customer.Mutation) (c customer.Customer, created bool, err error)

	// Patch applies the non-nil fields of p, or returns ErrNotFound.
	Patch(ctx context.Context, id uuid.UUID, p customer.Patch) (customer.Customer, error)

	// Query returns one page of matching customers plus the total match count.
	Query(ctx context.Context, q customer.Query) (customer.Page, error)

	// Ping verifies the backing database is reachable.
	Ping(ctx context.Context) error

	// Driver names the backing database, e.g. "sqlite" or "postgres".
	Driver() string

	// Close releases the underlying resources.
	Close() error
}
