package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// baseTime is the instant the test clock starts from. Postgres keeps
// microsecond precision, so the fixture sits on a whole microsecond.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// newStore connects to the database named by TEST_POSTGRES_DSN and empties the
// table. Without that variable the package's tests skip, so `go test ./...`
// stays runnable with no database.
//
//	TEST_POSTGRES_DSN=postgres://customers:customers@localhost:5432/customers?sslmode=disable
func newStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set; skipping the Postgres tests")
	}

	s, err := Open(context.Background(), Config{
		DSN: dsn, AutoMigrate: true, MaxOpenConns: 4, MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.DB().ExecContext(context.Background(), "TRUNCATE TABLE customers"); err != nil {
		t.Fatalf("truncate customers: %v", err)
	}

	next := baseTime
	s.now = func() time.Time {
		t := next
		next = next.Add(time.Second)
		return t
	}
	return s
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
	s := newStore(t)
	ctx := context.Background()

	created := mustCreate(t, s, "Acme Ltd", customer.StatusActive)
	if created.ID.Version() != 7 {
		t.Errorf("id version = %d, want 7 (UUID v7)", created.ID.Version())
	}
	if !created.CreateTS.Equal(baseTime) || !created.UpdateTS.Equal(baseTime) {
		t.Errorf("timestamps = %s / %s, want both %s", created.CreateTS, created.UpdateTS, baseTime)
	}

	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertSame(t, got, created)
}

func TestGetUnknownID(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get(context.Background(), uuid.Must(uuid.NewV7())); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get unknown id: err = %v, want store.ErrNotFound", err)
	}
}

func TestReplaceExistingPreservesCreateTS(t *testing.T) {
	s := newStore(t)
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
		t.Errorf("replace result = %+v, want name and status replaced", replaced)
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
	assertSame(t, stored, replaced)
}

func TestReplaceUnknownIDCreates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())

	created, wasCreated, err := s.Replace(ctx, id, customer.Mutation{Name: "New Co", Status: customer.StatusActive})
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
	assertSame(t, stored, created)
}

func TestPatchAppliesOnlySetFields(t *testing.T) {
	s := newStore(t)
	created := mustCreate(t, s, "Acme Ltd", customer.StatusPending)

	status := customer.StatusSuspended
	patched, err := s.Patch(context.Background(), created.ID, customer.Patch{Status: &status})
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
}

func TestPatchUnknownID(t *testing.T) {
	s := newStore(t)
	name := "Nobody"
	_, err := s.Patch(context.Background(), uuid.Must(uuid.NewV7()), customer.Patch{Name: &name})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("patch unknown id: err = %v, want store.ErrNotFound", err)
	}
}

func TestQueryFilters(t *testing.T) {
	s := newStore(t)
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
		{"created after", customer.Query{CreatedAfter: &beta.CreateTS}, []uuid.UUID{beta.ID, gamma.ID}},
		{"created before", customer.Query{CreatedBefore: &beta.CreateTS}, []uuid.UUID{acme.ID, beta.ID}},
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
}

func TestQueryPagingAndSorting(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := mustCreate(t, s, "charlie", customer.StatusActive)
	second := mustCreate(t, s, "alpha", customer.StatusActive)
	third := mustCreate(t, s, "Bravo", customer.StatusActive)

	t.Run("sorted by name, case-insensitively", func(t *testing.T) {
		page := mustQuery(t, s, ctx, customer.Query{Sort: customer.SortName})
		assertIDs(t, page, []uuid.UUID{second.ID, third.ID, first.ID})
	})

	t.Run("newest first", func(t *testing.T) {
		page := mustQuery(t, s, ctx, customer.Query{Sort: customer.SortCreateTS, Desc: true})
		assertIDs(t, page, []uuid.UUID{third.ID, second.ID, first.ID})
	})

	t.Run("second page", func(t *testing.T) {
		page := mustQuery(t, s, ctx, customer.Query{Limit: 2, Offset: 2})
		if page.Total != 3 {
			t.Errorf("total = %d, want 3", page.Total)
		}
		if page.Limit != 2 || page.Offset != 2 {
			t.Errorf("limit/offset = %d/%d, want 2/2", page.Limit, page.Offset)
		}
		assertIDs(t, page, []uuid.UUID{third.ID})
	})

	t.Run("offset past the end", func(t *testing.T) {
		page := mustQuery(t, s, ctx, customer.Query{Offset: 99})
		if page.Total != 3 {
			t.Errorf("total = %d, want 3", page.Total)
		}
		assertIDs(t, page, nil)
	})
}

// TestColumnTypes checks the native column types are in use, since the whole
// point of this driver is to lean on them rather than store text.
func TestColumnTypes(t *testing.T) {
	s := newStore(t)

	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_name = 'customers'`)
	if err != nil {
		t.Fatalf("read column types: %v", err)
	}
	defer rows.Close()

	types := map[string]string{}
	for rows.Next() {
		var column, dataType string
		if err := rows.Scan(&column, &dataType); err != nil {
			t.Fatalf("scan column type: %v", err)
		}
		types[column] = dataType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read column types: %v", err)
	}

	want := map[string]string{
		"id":        "uuid",
		"name":      "text",
		"status":    "text",
		"create_ts": "timestamp with time zone",
		"update_ts": "timestamp with time zone",
	}
	for column, wantType := range want {
		if types[column] != wantType {
			t.Errorf("%s is %q, want %q", column, types[column], wantType)
		}
	}
}

func TestOpenRejectsEmptyDSN(t *testing.T) {
	if _, err := Open(context.Background(), Config{DSN: "  "}); err == nil {
		t.Fatal("open with an empty DSN: err = nil, want an error")
	}
}

func TestDriverName(t *testing.T) {
	if got := newStore(t).Driver(); got != "postgres" {
		t.Errorf("Driver() = %q, want \"postgres\"", got)
	}
}

func mustQuery(t *testing.T, s *Store, ctx context.Context, q customer.Query) customer.Page {
	t.Helper()
	if err := q.Normalize(50, 500); err != nil {
		t.Fatalf("normalize query: %v", err)
	}
	page, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return page
}

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

func assertSame(t *testing.T, got, want customer.Customer) {
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
