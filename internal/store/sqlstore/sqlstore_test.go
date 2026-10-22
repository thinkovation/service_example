package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// baseTime is the instant the fake clock starts from. Postgres keeps
// microsecond precision, so the fixture stays on whole microseconds to let the
// same assertions run against both engines.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fakeClock hands out a new instant one second later on every call, so the
// order of writes is visible in the stored timestamps.
type fakeClock struct {
	mu   sync.Mutex
	next time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.next
	c.next = c.next.Add(time.Second)
	return t
}

func newSQLiteStore(t *testing.T) *Store {
	t.Helper()

	// A named in-memory database with a shared cache survives for as long as a
	// connection is open, which the single-connection pool guarantees.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	s, err := Open(context.Background(), Config{Dialect: DialectSQLite, DSN: dsn, AutoMigrate: true})
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newPostgresStore returns a Postgres-backed store, or skips the test when no
// database is configured. Set TEST_POSTGRES_DSN to run against Postgres, e.g.
// postgres://customers:customers@localhost:5432/customers?sslmode=disable
func newPostgresStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set; skipping Postgres tests")
	}
	s, err := Open(context.Background(), Config{
		Dialect: DialectPostgres, DSN: dsn, AutoMigrate: true,
		MaxOpenConns: 4, MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	if _, err := s.DB().ExecContext(context.Background(), "TRUNCATE TABLE customers"); err != nil {
		t.Fatalf("truncate customers: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// backends returns every engine the suite can run against.
func backends() []struct {
	name string
	open func(*testing.T) *Store
} {
	return []struct {
		name string
		open func(*testing.T) *Store
	}{
		{DialectSQLite, newSQLiteStore},
		{DialectPostgres, newPostgresStore},
	}
}

// withStore runs fn against every available backend with a pinned clock.
func withStore(t *testing.T, fn func(t *testing.T, s *Store, clock *fakeClock)) {
	t.Helper()
	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			clock := &fakeClock{next: baseTime}
			s.now = clock.now
			fn(t, s, clock)
		})
	}
}

func mustCreate(t *testing.T, s *Store, name string, status customer.Status) customer.Customer {
	t.Helper()
	c, err := s.Create(context.Background(), customer.Mutation{Name: name, Status: status})
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return c
}

func TestCreateAndGet(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()

		created := mustCreate(t, s, "Acme Ltd", customer.StatusActive)
		if created.ID == uuid.Nil {
			t.Fatal("created customer has a nil id")
		}
		if got := created.ID.Version(); got != 7 {
			t.Errorf("id version = %d, want 7 (UUID v7)", got)
		}
		if !created.CreateTS.Equal(baseTime) || !created.UpdateTS.Equal(baseTime) {
			t.Errorf("timestamps = %s / %s, want both %s", created.CreateTS, created.UpdateTS, baseTime)
		}

		got, err := s.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		assertSameCustomer(t, got, created)
	})
}

func TestGetUnknownID(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		_, err := s.Get(context.Background(), uuid.Must(uuid.NewV7()))
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get unknown id: err = %v, want store.ErrNotFound", err)
		}
	})
}

func TestReplaceExistingPreservesCreateTS(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()
		created := mustCreate(t, s, "Acme Ltd", customer.StatusPending)

		replaced, wasCreated, err := s.Replace(ctx, created.ID,
			customer.Mutation{Name: "Acme Limited", Status: customer.StatusActive})
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
		if wasCreated {
			t.Error("replace reported a create for an existing customer")
		}
		if replaced.Name != "Acme Limited" || replaced.Status != customer.StatusActive {
			t.Errorf("replace result = %+v, want name/status replaced", replaced)
		}
		if !replaced.CreateTS.Equal(created.CreateTS) {
			t.Errorf("create_ts = %s, want it preserved at %s", replaced.CreateTS, created.CreateTS)
		}
		if !replaced.UpdateTS.After(created.UpdateTS) {
			t.Errorf("update_ts = %s, want it after %s", replaced.UpdateTS, created.UpdateTS)
		}

		stored, err := s.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("get after replace: %v", err)
		}
		assertSameCustomer(t, stored, replaced)
	})
}

func TestReplaceUnknownIDCreates(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()
		id := uuid.Must(uuid.NewV7())

		created, wasCreated, err := s.Replace(ctx, id,
			customer.Mutation{Name: "New Co", Status: customer.StatusActive})
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
		if !wasCreated {
			t.Error("replace of an unknown id did not report a create")
		}
		if created.ID != id {
			t.Errorf("id = %s, want the requested %s", created.ID, id)
		}

		stored, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get after replace: %v", err)
		}
		assertSameCustomer(t, stored, created)
	})
}

func TestPatchAppliesOnlySetFields(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()
		created := mustCreate(t, s, "Acme Ltd", customer.StatusPending)

		status := customer.StatusSuspended
		patched, err := s.Patch(ctx, created.ID, customer.Patch{Status: &status})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if patched.Name != created.Name {
			t.Errorf("name = %q, want it untouched at %q", patched.Name, created.Name)
		}
		if patched.Status != customer.StatusSuspended {
			t.Errorf("status = %q, want %q", patched.Status, customer.StatusSuspended)
		}
		if !patched.CreateTS.Equal(created.CreateTS) {
			t.Errorf("create_ts = %s, want it preserved at %s", patched.CreateTS, created.CreateTS)
		}
		if !patched.UpdateTS.After(created.UpdateTS) {
			t.Errorf("update_ts = %s, want it after %s", patched.UpdateTS, created.UpdateTS)
		}
	})
}

func TestPatchUnknownID(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		name := "Nobody"
		_, err := s.Patch(context.Background(), uuid.Must(uuid.NewV7()), customer.Patch{Name: &name})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("patch unknown id: err = %v, want store.ErrNotFound", err)
		}
	})
}

func TestQueryFilters(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()
		acme := mustCreate(t, s, "Acme Ltd", customer.StatusActive)
		beta := mustCreate(t, s, "Beta Industries", customer.StatusPending)
		gamma := mustCreate(t, s, "Gamma Acme Holdings", customer.StatusInactive)

		name := "acme ltd"
		fragment := "acme"
		missing := "no such customer"
		wildcard := "%"

		cases := []struct {
			name  string
			query customer.Query
			want  []uuid.UUID
		}{
			{"all", customer.Query{}, []uuid.UUID{acme.ID, beta.ID, gamma.ID}},
			{"by id", customer.Query{IDs: []uuid.UUID{beta.ID}}, []uuid.UUID{beta.ID}},
			{"exact name is case-insensitive", customer.Query{Name: &name}, []uuid.UUID{acme.ID}},
			{"name contains", customer.Query{NameContains: &fragment}, []uuid.UUID{acme.ID, gamma.ID}},
			{"name contains nothing", customer.Query{NameContains: &missing}, nil},
			{"wildcards are literal", customer.Query{NameContains: &wildcard}, nil},
			{"status", customer.Query{Status: []customer.Status{customer.StatusActive, customer.StatusPending}},
				[]uuid.UUID{acme.ID, beta.ID}},
			{"created after", customer.Query{CreatedAfter: ptr(beta.CreateTS)}, []uuid.UUID{beta.ID, gamma.ID}},
			{"created before", customer.Query{CreatedBefore: ptr(beta.CreateTS)}, []uuid.UUID{acme.ID, beta.ID}},
			{"updated after the last write", customer.Query{UpdatedAfter: ptr(gamma.UpdateTS.Add(time.Second))}, nil},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				q := tc.query
				if err := q.Normalize(50, 500); err != nil {
					t.Fatalf("normalize query: %v", err)
				}
				page, err := s.Query(ctx, q)
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				assertIDs(t, page, tc.want)
			})
		}
	})
}

func TestQueryPagingAndSorting(t *testing.T) {
	withStore(t, func(t *testing.T, s *Store, _ *fakeClock) {
		ctx := context.Background()
		first := mustCreate(t, s, "charlie", customer.StatusActive)
		second := mustCreate(t, s, "alpha", customer.StatusActive)
		third := mustCreate(t, s, "Bravo", customer.StatusActive)

		t.Run("sorted by name, case-insensitively", func(t *testing.T) {
			q := customer.Query{Sort: customer.SortName}
			if err := q.Normalize(50, 500); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			page, err := s.Query(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			assertIDs(t, page, []uuid.UUID{second.ID, third.ID, first.ID})
		})

		t.Run("newest first", func(t *testing.T) {
			q := customer.Query{Sort: customer.SortCreateTS, Desc: true}
			if err := q.Normalize(50, 500); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			page, err := s.Query(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			assertIDs(t, page, []uuid.UUID{third.ID, second.ID, first.ID})
		})

		t.Run("second page", func(t *testing.T) {
			q := customer.Query{Limit: 2, Offset: 2}
			if err := q.Normalize(50, 500); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			page, err := s.Query(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if page.Total != 3 {
				t.Errorf("total = %d, want 3", page.Total)
			}
			if page.Limit != 2 || page.Offset != 2 {
				t.Errorf("limit/offset = %d/%d, want 2/2", page.Limit, page.Offset)
			}
			assertIDs(t, page, []uuid.UUID{third.ID})
		})

		t.Run("offset past the end", func(t *testing.T) {
			q := customer.Query{Offset: 99}
			if err := q.Normalize(50, 500); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			page, err := s.Query(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if page.Total != 3 {
				t.Errorf("total = %d, want 3", page.Total)
			}
			assertIDs(t, page, nil)
		})
	})
}

func TestOpenRejectsUnknownDialect(t *testing.T) {
	if _, err := Open(context.Background(), Config{Dialect: "mysql", DSN: "ignored"}); err == nil {
		t.Fatal("open with an unknown dialect: err = nil, want an error")
	}
}

func TestNormalizeSQLiteDSN(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name        string
		in          string
		wantPrefix  string
		wantPragmas bool
	}{
		{"bare path", dir + "/nested/app.db", "file:" + dir + "/nested/app.db", true},
		{"file url", "file:" + dir + "/app.db", "file:" + dir + "/app.db", true},
		{"in-memory", "file:test?mode=memory&cache=shared", "file:test", true},
		{"caller pragmas are left alone", "file:" + dir + "/app.db?_pragma=busy_timeout(1)", "file:" + dir + "/app.db", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSQLiteDSN(tc.in)
			if err != nil {
				t.Fatalf("normalizeSQLiteDSN(%q): %v", tc.in, err)
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("got %q, want it to start with %q", got, tc.wantPrefix)
			}
			if tc.wantPragmas && !strings.Contains(got, "_pragma=") {
				t.Errorf("got %q, want it to carry pragmas", got)
			}
		})
	}

	if _, err := normalizeSQLiteDSN("   "); err == nil {
		t.Error("normalizeSQLiteDSN(\"\"): err = nil, want an error")
	}
}

func TestSQLiteCreatesMissingDirectory(t *testing.T) {
	path := t.TempDir() + "/deeper/still/customers.db"

	s, err := Open(context.Background(), Config{Dialect: DialectSQLite, DSN: path, AutoMigrate: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.Create(context.Background(),
		customer.Mutation{Name: "On Disk", Status: customer.StatusActive}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func assertIDs(t *testing.T, page customer.Page, want []uuid.UUID) {
	t.Helper()
	got := make([]uuid.UUID, 0, len(page.Items))
	for _, c := range page.Items {
		got = append(got, c.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d customers %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("customer %d = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
}

func assertSameCustomer(t *testing.T, got, want customer.Customer) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.Status != want.Status {
		t.Errorf("customer = %+v, want %+v", got, want)
	}
	if !got.CreateTS.Equal(want.CreateTS) {
		t.Errorf("create_ts = %s, want %s", got.CreateTS, want.CreateTS)
	}
	if !got.UpdateTS.Equal(want.UpdateTS) {
		t.Errorf("update_ts = %s, want %s", got.UpdateTS, want.UpdateTS)
	}
}
