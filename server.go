package budgetstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

// maxImportBytes bounds one imported file. Ten years of a busy checking
// account is well under 5 MB.
const maxImportBytes = 32 << 20

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// decodeBody reads one JSON object and refuses unknown keys, so a misspelled
// field is a 400 rather than a silent no-op.
func decodeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	return nil
}

func respond[T any](w http.ResponseWriter, status int, value T, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, value)
}

// RegisterHandlers mounts every route on mux. CONTRACT.md is the route table.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /vocabulary", handleVocabulary)

	mux.HandleFunc("GET /institutions", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListInstitutions()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /institutions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name    string `json:"name"`
			Website string `json:"website"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateInstitution(Institution{Name: in.Name, Website: in.Website})
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListAccounts()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		var in AccountInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateAccount(in)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetAccount(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PATCH /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var patch AccountPatch
		if err := decodeBody(r, &patch); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.PatchAccount(r.PathValue("id"), patch)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/description-markers", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Marker string `json:"marker"`
			Note   string `json:"note"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.AddDescriptionMarker(r.PathValue("id"), in.Marker, in.Note)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("DELETE /accounts/{id}/description-markers/{marker_id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.DeleteDescriptionMarker(r.PathValue("id"), r.PathValue("marker_id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/opening-balance", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AsOf         string `json:"as_of"`
			BalanceCents int64  `json:"balance_cents"`
			Source       string `json:"source"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetOpeningBalance(BalanceAssertionInput{AccountID: r.PathValue("id"), AsOf: in.AsOf, BalanceCents: in.BalanceCents, Source: in.Source})
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /balance-assertions", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ListBalanceAssertions(r.URL.Query().Get("account_id"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /balance-assertions", func(w http.ResponseWriter, r *http.Request) {
		var in BalanceAssertionInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateBalanceAssertion(in)
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /categories", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListCategories()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /categories", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ParentID string `json:"parent_id"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateCategory(Category{ParentID: in.ParentID, Name: in.Name, Kind: in.Kind})
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /categories/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetCategory(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /payees/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetPayee(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /payees", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListPayees()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /payees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name              string   `json:"name"`
			DefaultCategoryID string   `json:"default_category_id"`
			Patterns          []string `json:"patterns"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreatePayee(Payee{Name: in.Name, DefaultCategoryID: in.DefaultCategoryID, Patterns: in.Patterns})
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("POST /payees/apply-rules", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.ApplyPayeeRules()
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /transactions", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		filter := TransactionFilter{
			AccountID:     query.Get("account_id"),
			CategoryID:    query.Get("category_id"),
			Month:         query.Get("month"),
			Query:         query.Get("q"),
			Uncategorized: query.Get("uncategorized") == "true",
		}
		for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
			if value := query.Get(name); value != "" {
				parsed, err := strconv.Atoi(value)
				if err != nil || parsed < 0 {
					writeError(w, fmt.Errorf("%w: %s %q is not a whole number", ErrInvalid, name, value))
					return
				}
				*target = parsed
			}
		}
		list, err := s.ListTransactions(filter)
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /transactions", func(w http.ResponseWriter, r *http.Request) {
		var in TransactionInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateTransaction(in)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetTransaction(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PATCH /transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var patch TransactionPatch
		if err := decodeBody(r, &patch); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.PatchTransaction(r.PathValue("id"), patch)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PUT /transactions/{id}/categories", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Splits []CategorySplit `json:"splits"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetTransactionCategories(r.PathValue("id"), in.Splits)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /transactions/{id}/undo-transfer-without-partner", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.UndoTransferWithoutPartner(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /transfers/match", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.MatchTransfers()
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /import-batches", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListImportBatches()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /import-batches", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		out, err := s.Import(ImportRequest{
			Source:    query.Get("source"),
			AccountID: query.Get("account_id"),
			FileName:  query.Get("file_name"),
			FileID:    query.Get("file_id"),
		}, http.MaxBytesReader(w, r.Body, maxImportBytes))
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /import-batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetImportBatch(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /import-batches/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.UndoImportBatch(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /reports/spending", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.SpendingByCategory(r.URL.Query().Get("month"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("GET /budget", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetBudget(r.URL.Query().Get("month"), r.URL.Query().Get("view"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PUT /budget-assignments", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CategoryID  string `json:"category_id"`
			Month       string `json:"month"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetBudgetAssignment(BudgetAssignment{CategoryID: in.CategoryID, Month: in.Month, AmountCents: in.AmountCents})
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /links", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ListLinks(r.URL.Query().Get("from_id"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /links", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FromID     string `json:"from_id"`
			EntityType string `json:"entity_type"`
			EntityRef  string `json:"entity_ref"`
			Note       string `json:"note"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateLink(Link{FromID: in.FromID, EntityType: in.EntityType, EntityRef: in.EntityRef, Note: in.Note})
		respond(w, http.StatusCreated, out, err)
	})
}
