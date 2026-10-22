// Package customer holds the customer domain model: the entity itself, the
// shapes used to create and change it, and the query specification used to
// search a collection of them.
package customer

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a customer.
type Status string

const (
	StatusActive    Status = "active"
	StatusInactive  Status = "inactive"
	StatusPending   Status = "pending"
	StatusSuspended Status = "suspended"
)

// DefaultStatus is applied when a create request omits the status.
const DefaultStatus = StatusPending

// Statuses lists every status the service accepts, in lifecycle order.
var Statuses = []Status{StatusPending, StatusActive, StatusInactive, StatusSuspended}

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	for _, known := range Statuses {
		if s == known {
			return true
		}
	}
	return false
}

func (s Status) String() string { return string(s) }

// MaxNameLen bounds the customer name so a single row cannot be used to push
// arbitrarily large values into the store.
const MaxNameLen = 256

// Customer is the stored entity.
type Customer struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Status   Status    `json:"status"`
	CreateTS time.Time `json:"create_ts"`
	UpdateTS time.Time `json:"update_ts"`
}

// Mutation carries the complete set of client-settable fields. It is used by
// create and by replace (PUT), both of which define every field.
type Mutation struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
}

// Patch carries a partial update (PATCH). A nil field is left untouched.
type Patch struct {
	Name   *string `json:"name"`
	Status *Status `json:"status"`
}

// Empty reports whether the patch would change nothing.
func (p Patch) Empty() bool { return p.Name == nil && p.Status == nil }

// SortField names a column a query may be ordered by.
type SortField string

const (
	SortCreateTS SortField = "create_ts"
	SortUpdateTS SortField = "update_ts"
	SortName     SortField = "name"
	SortStatus   SortField = "status"
)

// SortFields lists every field a query may be ordered by.
var SortFields = []SortField{SortCreateTS, SortUpdateTS, SortName, SortStatus}

// Valid reports whether f is a sortable field.
func (f SortField) Valid() bool {
	for _, known := range SortFields {
		if f == known {
			return true
		}
	}
	return false
}

// Query is the search specification shared by GET /customers and
// QUERY /customers. The zero value selects every customer, newest first.
type Query struct {
	// IDs restricts the result to the given identifiers.
	IDs []uuid.UUID `json:"ids,omitempty"`
	// Name matches the whole name, case-insensitively.
	Name *string `json:"name,omitempty"`
	// NameContains matches any name containing the value, case-insensitively.
	NameContains *string `json:"name_contains,omitempty"`
	// Status restricts the result to the given statuses (OR-ed together).
	Status []Status `json:"status,omitempty"`
	// CreatedAfter and the three bounds below are inclusive.
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	CreatedBefore *time.Time `json:"created_before,omitempty"`
	UpdatedAfter  *time.Time `json:"updated_after,omitempty"`
	UpdatedBefore *time.Time `json:"updated_before,omitempty"`

	Limit  int       `json:"limit,omitempty"`
	Offset int       `json:"offset,omitempty"`
	Sort   SortField `json:"sort,omitempty"`
	Desc   bool      `json:"desc,omitempty"`
}

// Page is one page of query results plus the total number of matches.
type Page struct {
	Items  []Customer `json:"items"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

// ValidationError reports one or more invalid fields in a request.
type ValidationError struct {
	Fields []FieldError
}

// FieldError describes why a single field was rejected.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func newValidationError(fields []FieldError) error {
	if len(fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: fields}
}

func validStatusList() string {
	out := make([]string, 0, len(Statuses))
	for _, s := range Statuses {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}

// Normalize trims the name and defaults the status, then validates the result.
func (m *Mutation) Normalize() error {
	m.Name = strings.TrimSpace(m.Name)
	if m.Status == "" {
		m.Status = DefaultStatus
	}
	m.Status = Status(strings.ToLower(strings.TrimSpace(string(m.Status))))

	var fields []FieldError
	if m.Name == "" {
		fields = append(fields, FieldError{Field: "name", Message: "must not be empty"})
	} else if len([]rune(m.Name)) > MaxNameLen {
		fields = append(fields, FieldError{
			Field:   "name",
			Message: fmt.Sprintf("must be at most %d characters", MaxNameLen),
		})
	}
	if !m.Status.Valid() {
		fields = append(fields, FieldError{
			Field:   "status",
			Message: "must be one of: " + validStatusList(),
		})
	}
	return newValidationError(fields)
}

// Normalize trims and validates whichever fields the patch sets.
func (p *Patch) Normalize() error {
	var fields []FieldError
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		p.Name = &name
		if name == "" {
			fields = append(fields, FieldError{Field: "name", Message: "must not be empty"})
		} else if len([]rune(name)) > MaxNameLen {
			fields = append(fields, FieldError{
				Field:   "name",
				Message: fmt.Sprintf("must be at most %d characters", MaxNameLen),
			})
		}
	}
	if p.Status != nil {
		status := Status(strings.ToLower(strings.TrimSpace(string(*p.Status))))
		p.Status = &status
		if !status.Valid() {
			fields = append(fields, FieldError{
				Field:   "status",
				Message: "must be one of: " + validStatusList(),
			})
		}
	}
	if len(fields) == 0 && p.Empty() {
		fields = append(fields, FieldError{Field: "body", Message: "must set at least one of: name, status"})
	}
	return newValidationError(fields)
}

// Normalize applies paging defaults and validates the query. defaultLimit and
// maxLimit come from configuration so the caller cannot ask for unbounded pages.
func (q *Query) Normalize(defaultLimit, maxLimit int) error {
	var fields []FieldError

	if q.Name != nil {
		name := strings.TrimSpace(*q.Name)
		q.Name = &name
	}
	if q.NameContains != nil {
		fragment := strings.TrimSpace(*q.NameContains)
		q.NameContains = &fragment
	}
	for i, s := range q.Status {
		normalized := Status(strings.ToLower(strings.TrimSpace(string(s))))
		q.Status[i] = normalized
		if !normalized.Valid() {
			fields = append(fields, FieldError{
				Field:   "status",
				Message: "must be one of: " + validStatusList(),
			})
			break
		}
	}

	if q.Sort == "" {
		q.Sort = SortCreateTS
	}
	q.Sort = SortField(strings.ToLower(strings.TrimSpace(string(q.Sort))))
	if !q.Sort.Valid() {
		fields = append(fields, FieldError{
			Field:   "sort",
			Message: "must be one of: create_ts, update_ts, name, status",
		})
	}

	switch {
	case q.Limit < 0:
		fields = append(fields, FieldError{Field: "limit", Message: "must not be negative"})
	case q.Limit == 0:
		q.Limit = defaultLimit
	case q.Limit > maxLimit:
		fields = append(fields, FieldError{
			Field:   "limit",
			Message: fmt.Sprintf("must be at most %d", maxLimit),
		})
	}
	if q.Offset < 0 {
		fields = append(fields, FieldError{Field: "offset", Message: "must not be negative"})
	}

	if q.CreatedAfter != nil && q.CreatedBefore != nil && q.CreatedAfter.After(*q.CreatedBefore) {
		fields = append(fields, FieldError{
			Field:   "created_after",
			Message: "must not be later than created_before",
		})
	}
	if q.UpdatedAfter != nil && q.UpdatedBefore != nil && q.UpdatedAfter.After(*q.UpdatedBefore) {
		fields = append(fields, FieldError{
			Field:   "updated_after",
			Message: "must not be later than updated_before",
		})
	}

	return newValidationError(fields)
}
