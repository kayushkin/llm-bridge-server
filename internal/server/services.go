package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/serviceinventory"
	"github.com/kayushkin/llm-bridge/msg"
)

// serviceInventoryMaxAge is how long one reading of the host is reused. The
// page reads the list, then a schema, then rows, within seconds; healthcheck
// itself only re-checks every 60s, so a fresher walk of /proc buys nothing.
const serviceInventoryMaxAge = 15 * time.Second

// databaseRowsDefaultLimit and databaseRowsMaxLimit bound one rows read. The
// page shows "the most recent stuff"; a caller wanting a table dump has the
// file's own service to ask.
const (
	databaseRowsDefaultLimit = 50
	databaseRowsMaxLimit     = 500
)

var serviceInventoryClient = &http.Client{Timeout: 5 * time.Second}

// currentServiceInventory answers the held reading when it is younger than
// serviceInventoryMaxAge, and takes a new one otherwise. force skips the age
// check: a database path the held reading does not list may have been opened
// since, and the read routes retry once before refusing it.
func (s *Server) currentServiceInventory(ctx context.Context, force bool) (*serviceinventory.Inventory, error) {
	s.serviceInventoryMu.Lock()
	defer s.serviceInventoryMu.Unlock()
	if !force && s.serviceInventory != nil && time.Since(s.serviceInventory.ReadAt) < serviceInventoryMaxAge {
		return s.serviceInventory, nil
	}
	inv, err := serviceinventory.Read(ctx, serviceInventoryClient, s.cfg.HealthcheckURL)
	if err != nil {
		return nil, err
	}
	s.serviceInventory = inv
	return inv, nil
}

// databaseIsOpenBySomeService is the gate on every read: a path is readable
// only while a service healthcheck watches holds it open. That is what keeps
// these routes a view onto the running services rather than a way to read
// any SQLite file the bridge's user can.
func (s *Server) databaseIsOpenBySomeService(ctx context.Context, path string) (bool, error) {
	inv, err := s.currentServiceInventory(ctx, false)
	if err != nil {
		return false, err
	}
	if inv.Databases[path] {
		return true, nil
	}
	inv, err = s.currentServiceInventory(ctx, true)
	if err != nil {
		return false, err
	}
	return inv.Databases[path], nil
}

func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	inv, err := s.currentServiceInventory(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "healthcheck_unreachable", err.Error())
		return
	}
	writeJSON(w, inv.Response)
}

// requireOpenDatabase reads and checks the path parameter, writing the
// refusal itself. It returns "" when the caller should stop.
func (s *Server) requireOpenDatabase(w http.ResponseWriter, r *http.Request) string {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "path_required", "path is required: the file as GET /services lists it")
		return ""
	}
	open, err := s.databaseIsOpenBySomeService(r.Context(), path)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "healthcheck_unreachable", err.Error())
		return ""
	}
	if !open {
		writeJSONError(w, http.StatusNotFound, "unknown_database", "no watched service holds "+path+" open; only a file listed by GET /services can be read")
		return ""
	}
	return path
}

func (s *Server) handleDatabaseSchema(w http.ResponseWriter, r *http.Request) {
	path := s.requireOpenDatabase(w, r)
	if path == "" {
		return
	}
	schema, err := serviceinventory.ReadSchema(r.Context(), path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "database_read_failed", err.Error())
		return
	}
	writeJSON(w, schema)
}

func (s *Server) handleDatabaseRows(w http.ResponseWriter, r *http.Request) {
	path := s.requireOpenDatabase(w, r)
	if path == "" {
		return
	}
	query := r.URL.Query()
	q := serviceinventory.RowsQuery{
		Table:      query.Get("table"),
		Limit:      databaseRowsDefaultLimit,
		OrderBy:    query.Get("order_by"),
		Descending: true,
		Filters:    []msg.DatabaseRowFilter{},
	}
	if q.Table == "" {
		writeJSONError(w, http.StatusBadRequest, "table_required", "table is required")
		return
	}
	if text := query.Get("limit"); text != "" {
		limit, err := strconv.Atoi(text)
		if err != nil || limit < 1 || limit > databaseRowsMaxLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to "+strconv.Itoa(databaseRowsMaxLimit))
			return
		}
		q.Limit = limit
	}
	switch query.Get("order") {
	case "", "desc":
	case "asc":
		q.Descending = false
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_order", "order must be asc or desc")
		return
	}
	for _, text := range query["filter"] {
		f, err := serviceinventory.ParseFilter(text)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		q.Filters = append(q.Filters, f)
	}
	rows, err := serviceinventory.ReadRows(r.Context(), path, q)
	if err != nil {
		var requestErr *serviceinventory.RequestError
		if errors.As(err, &requestErr) {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "database_read_failed", err.Error())
		return
	}
	writeJSON(w, rows)
}
