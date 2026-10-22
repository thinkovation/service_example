package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thinkovation/service_example/internal/customer"
)

// handleCreateCustomer serves POST /customers. The id (UUID v7) and both
// timestamps are assigned by the service.
func (s *Server) handleCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var m customer.Mutation
	if err := decodeJSON(w, r, s.opts.MaxBodyBytes, &m); err != nil {
		if errors.Is(err, errEmptyBody) {
			err = newAPIError(http.StatusBadRequest, codeInvalidJSON, "a JSON request body is required")
		}
		writeError(w, r, err)
		return
	}
	if err := m.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}

	created, err := s.opts.Store.Create(r.Context(), m)
	if err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/customers/"+created.ID.String())
	writeJSON(w, r, http.StatusCreated, created)
}

// handleGetCustomer serves GET /customers/{id}.
func (s *Server) handleGetCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	found, err := s.opts.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, found)
}

// handleReplaceCustomer serves PUT /customers/{id}: a full replacement of the
// client-settable fields. Per the semantics of PUT it creates the customer
// under the given id when it does not yet exist, answering 201; an existing
// customer keeps its Create_TS and answers 200.
func (s *Server) handleReplaceCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var m customer.Mutation
	if err := decodeJSON(w, r, s.opts.MaxBodyBytes, &m); err != nil {
		if errors.Is(err, errEmptyBody) {
			err = newAPIError(http.StatusBadRequest, codeInvalidJSON, "a JSON request body is required")
		}
		writeError(w, r, err)
		return
	}
	if err := m.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}

	replaced, created, err := s.opts.Store.Replace(r.Context(), id, m)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/customers/"+replaced.ID.String())
		writeJSON(w, r, http.StatusCreated, replaced)
		return
	}
	writeJSON(w, r, http.StatusOK, replaced)
}

// handlePatchCustomer serves PATCH /customers/{id}. Fields the body omits (or
// sets to null) are left as they are.
func (s *Server) handlePatchCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var p customer.Patch
	if err := decodeJSON(w, r, s.opts.MaxBodyBytes, &p); err != nil {
		if errors.Is(err, errEmptyBody) {
			err = newAPIError(http.StatusBadRequest, codeInvalidJSON, "a JSON request body is required")
		}
		writeError(w, r, err)
		return
	}
	if err := p.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}

	patched, err := s.opts.Store.Patch(r.Context(), id, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, patched)
}

// handleListCustomers serves GET /customers, taking its filters from the query
// string. It is the same search as QUERY /customers, expressed in a URL.
func (s *Server) handleListCustomers(w http.ResponseWriter, r *http.Request) {
	req, err := queryFromURL(r.URL.Query())
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.runQuery(w, r, req)
}

// handleQueryCustomers serves QUERY /customers (and its POST
// /customers/query alias), taking its filters from a JSON body. An empty body
// means "match everything".
func (s *Server) handleQueryCustomers(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := decodeJSON(w, r, s.opts.MaxBodyBytes, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeError(w, r, err)
		return
	}
	s.runQuery(w, r, req)
}

func (s *Server) runQuery(w http.ResponseWriter, r *http.Request, req queryRequest) {
	q, err := req.toQuery()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := q.Normalize(s.opts.DefaultPageSize, s.opts.MaxPageSize); err != nil {
		writeError(w, r, err)
		return
	}

	page, err := s.opts.Store.Query(r.Context(), q)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, page)
}

// queryRequest is the wire form of a search. It keeps ids, statuses and the
// sort order as strings so that a malformed value is reported as a field
// error rather than a JSON decoding failure.
type queryRequest struct {
	IDs           []string   `json:"ids,omitempty"`
	Name          *string    `json:"name,omitempty"`
	NameContains  *string    `json:"name_contains,omitempty"`
	Status        []string   `json:"status,omitempty"`
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	CreatedBefore *time.Time `json:"created_before,omitempty"`
	UpdatedAfter  *time.Time `json:"updated_after,omitempty"`
	UpdatedBefore *time.Time `json:"updated_before,omitempty"`
	Limit         *int       `json:"limit,omitempty"`
	Offset        *int       `json:"offset,omitempty"`
	Sort          string     `json:"sort,omitempty"`
	Order         string     `json:"order,omitempty"`
}

func (q queryRequest) toQuery() (customer.Query, error) {
	out := customer.Query{
		Name:          q.Name,
		NameContains:  q.NameContains,
		CreatedAfter:  q.CreatedAfter,
		CreatedBefore: q.CreatedBefore,
		UpdatedAfter:  q.UpdatedAfter,
		UpdatedBefore: q.UpdatedBefore,
		Sort:          customer.SortField(q.Sort),
	}
	var fields []customer.FieldError

	for _, raw := range q.IDs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			fields = append(fields, customer.FieldError{
				Field:   "ids",
				Message: "must contain UUIDs, got " + strconv.Quote(raw),
			})
			break
		}
		out.IDs = append(out.IDs, id)
	}

	for _, raw := range q.Status {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		out.Status = append(out.Status, customer.Status(raw))
	}

	if q.Limit != nil {
		out.Limit = *q.Limit
	}
	if q.Offset != nil {
		out.Offset = *q.Offset
	}

	switch strings.ToLower(strings.TrimSpace(q.Order)) {
	case "", "asc":
	case "desc":
		out.Desc = true
	default:
		fields = append(fields, customer.FieldError{
			Field:   "order",
			Message: `must be "asc" or "desc"`,
		})
	}

	if len(fields) > 0 {
		return customer.Query{}, &customer.ValidationError{Fields: fields}
	}
	return out, nil
}

// queryFromURL maps the query string of GET /customers onto a queryRequest.
// Repeated and comma-separated values are both accepted for ids and status.
func queryFromURL(values url.Values) (queryRequest, error) {
	req := queryRequest{
		IDs:    splitList(values["id"], values["ids"]),
		Status: splitList(values["status"]),
		Sort:   values.Get("sort"),
		Order:  values.Get("order"),
	}
	var fields []customer.FieldError

	if v, ok := firstValue(values, "name"); ok {
		req.Name = &v
	}
	if v, ok := firstValue(values, "name_contains", "q"); ok {
		req.NameContains = &v
	}

	for _, p := range []struct {
		key    string
		target **time.Time
	}{
		{"created_after", &req.CreatedAfter},
		{"created_before", &req.CreatedBefore},
		{"updated_after", &req.UpdatedAfter},
		{"updated_before", &req.UpdatedBefore},
	} {
		raw, ok := firstValue(values, p.key)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fields = append(fields, customer.FieldError{
				Field:   p.key,
				Message: "must be an RFC 3339 timestamp, e.g. 2026-01-31T09:00:00Z",
			})
			continue
		}
		utc := t.UTC()
		*p.target = &utc
	}

	for _, p := range []struct {
		key    string
		target **int
	}{
		{"limit", &req.Limit},
		{"offset", &req.Offset},
	} {
		raw, ok := firstValue(values, p.key)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			fields = append(fields, customer.FieldError{
				Field:   p.key,
				Message: "must be an integer",
			})
			continue
		}
		*p.target = &n
	}

	if len(fields) > 0 {
		return queryRequest{}, &customer.ValidationError{Fields: fields}
	}
	return req, nil
}

// firstValue returns the first non-empty value among the given keys.
func firstValue(values url.Values, keys ...string) (string, bool) {
	for _, key := range keys {
		if raw := strings.TrimSpace(values.Get(key)); raw != "" {
			return raw, true
		}
	}
	return "", false
}

// splitList flattens repeated parameters and comma-separated values into one
// list, so ?status=active&status=pending and ?status=active,pending agree.
func splitList(groups ...[]string) []string {
	var out []string
	for _, group := range groups {
		for _, raw := range group {
			for _, part := range strings.Split(raw, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

// pathID parses the {id} path segment.
func pathID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, newAPIError(http.StatusBadRequest, codeInvalidID,
			"customer id must be a UUID, got "+strconv.Quote(raw))
	}
	return id, nil
}
