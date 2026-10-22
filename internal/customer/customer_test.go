package customer

import (
	"errors"
	"testing"
	"time"
)

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}
	names := make([]string, 0, len(verr.Fields))
	for _, f := range verr.Fields {
		names = append(names, f.Field)
	}
	return names
}

func TestMutationNormalize(t *testing.T) {
	t.Run("trims and defaults", func(t *testing.T) {
		m := Mutation{Name: "  Acme Ltd "}
		if err := m.Normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if m.Name != "Acme Ltd" {
			t.Errorf("name = %q, want it trimmed", m.Name)
		}
		if m.Status != DefaultStatus {
			t.Errorf("status = %q, want the default %q", m.Status, DefaultStatus)
		}
	})

	t.Run("accepts a status in any case", func(t *testing.T) {
		m := Mutation{Name: "Acme", Status: "ACTIVE"}
		if err := m.Normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if m.Status != StatusActive {
			t.Errorf("status = %q, want %q", m.Status, StatusActive)
		}
	})

	t.Run("reports every bad field at once", func(t *testing.T) {
		m := Mutation{Name: "  ", Status: "retired"}
		got := fieldsOf(t, m.Normalize())
		if len(got) != 2 {
			t.Errorf("fields = %v, want both name and status reported", got)
		}
	})
}

func TestPatchNormalize(t *testing.T) {
	t.Run("an empty patch is rejected", func(t *testing.T) {
		p := Patch{}
		if got := fieldsOf(t, p.Normalize()); len(got) != 1 || got[0] != "body" {
			t.Errorf("fields = %v, want a single error on \"body\"", got)
		}
	})

	t.Run("a set field is trimmed", func(t *testing.T) {
		name := "  Acme  "
		p := Patch{Name: &name}
		if err := p.Normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if *p.Name != "Acme" {
			t.Errorf("name = %q, want it trimmed", *p.Name)
		}
	})
}

func TestQueryNormalize(t *testing.T) {
	t.Run("applies paging and sort defaults", func(t *testing.T) {
		q := Query{}
		if err := q.Normalize(25, 100); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if q.Limit != 25 {
			t.Errorf("limit = %d, want the default 25", q.Limit)
		}
		if q.Sort != SortCreateTS {
			t.Errorf("sort = %q, want the default %q", q.Sort, SortCreateTS)
		}
	})

	t.Run("rejects out-of-range paging", func(t *testing.T) {
		q := Query{Limit: 101}
		if got := fieldsOf(t, q.Normalize(25, 100)); len(got) != 1 || got[0] != "limit" {
			t.Errorf("fields = %v, want a single error on \"limit\"", got)
		}
	})

	t.Run("rejects an inverted time window", func(t *testing.T) {
		q := Query{}
		early, late := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-02-01T00:00:00Z")
		q.CreatedAfter, q.CreatedBefore = &late, &early
		if got := fieldsOf(t, q.Normalize(25, 100)); len(got) != 1 || got[0] != "created_after" {
			t.Errorf("fields = %v, want a single error on \"created_after\"", got)
		}
	})

	t.Run("rejects an unsortable column", func(t *testing.T) {
		q := Query{Sort: "id; DROP TABLE customers"}
		if got := fieldsOf(t, q.Normalize(25, 100)); len(got) != 1 || got[0] != "sort" {
			t.Errorf("fields = %v, want a single error on \"sort\"", got)
		}
	})
}

func mustTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}
