package possessions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// ReviewServer serves the routes of the photo review, the owner's questions,
// the wearable audit and the passes that read outside sources (eBay mail,
// budget-store's order lines, product listings). Reviewer reaches no records
// until a request narrows it to its scope (server_scope.go).
type ReviewServer struct {
	Reviewer *Reviewer
}

// RegisterHandlers adds the review's routes to mux, and answers their
// patterns.
func (server *ReviewServer) RegisterHandlers(mux *http.ServeMux) []string {
	var patterns []string
	register := handlerOn(mux, server.narrowedTo)
	handle := func(pattern string, build func(*ReviewServer) http.HandlerFunc) {
		patterns = append(patterns, pattern)
		register(pattern, build)
	}
	handle("POST /possessions/classify-kindless", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor     string `json:"actor"`
				BatchSize int    `json:"batch_size"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			run, err := server.Reviewer.ClassifyKindless(r.Context(), body.Actor, body.BatchSize)
			respond(w, http.StatusOK, run, err)
		}
	})
	handle("POST /wearables/check-links", func(server *ReviewServer) http.HandlerFunc { return auditPass(server.Reviewer.CheckWearableLinks) })
	handle("POST /wearables/check-duplicates", func(server *ReviewServer) http.HandlerFunc {
		return auditPass(server.Reviewer.CheckWearableDuplicates)
	})
	handle("POST /outfit-pieces/rematch", func(server *ReviewServer) http.HandlerFunc { return auditPass(server.Reviewer.RematchOutfitPieces) })
	handle("POST /marketplace-orders/import-ebay-mail", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request EbayImportRequest
			if !decodeBody(w, r, &request) {
				return
			}
			run, err := server.Reviewer.ImportEbayOrders(r.Context(), request)
			writeRun(w, run, err)
		}
	})
	handle("POST /marketplace-order-lines/match", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor              string `json:"actor"`
				MarketplaceOrderID string `json:"marketplace_order_id"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			run, err := server.Reviewer.MatchOrderLines(r.Context(), body.Actor, body.MarketplaceOrderID)
			writeRun(w, run, err)
		}
	})
	handle("POST /products", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request ProductRequest
			if !decodeBody(w, r, &request) {
				return
			}
			product, err := server.Reviewer.CreateProduct(r.Context(), request)
			respond(w, http.StatusCreated, product, err)
		}
	})
	handle("GET /photo-reviews", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			reviews, err := server.Reviewer.Store.ListPhotoReviews(r.URL.Query().Get("status"), limit, offset)
			respond(w, http.StatusOK, reviews, err)
		}
	})
	handle("POST /photo-reviews", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PhotoID        string `json:"photo_id"`
				QueuedBy       string `json:"queued_by"`
				Reading        string `json:"reading"`
				WearerPersonID string `json:"wearer_person_id"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			review, created, err := server.Reviewer.Store.QueuePhotoReview(r.Context(), body.PhotoID, body.QueuedBy, body.Reading, body.WearerPersonID)
			if err != nil {
				writeError(w, err)
				return
			}
			status := http.StatusOK
			if created {
				status = http.StatusCreated
				server.Reviewer.Wake()
			}
			writeJSON(w, status, review)
		}
	})
	handle("GET /photo-reviews/{id}", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			review, err := server.Reviewer.Store.GetPhotoReview(r.PathValue("id"))
			respond(w, http.StatusOK, review, err)
		}
	})
	handle("POST /photo-reviews/{id}/retry", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			if body.Actor == "" {
				writeError(w, fmt.Errorf("%w: actor is required", ErrInvalid))
				return
			}
			review, err := server.Reviewer.Store.RetryPhotoReview(r.PathValue("id"), body.Actor)
			if err == nil {
				server.Reviewer.Wake()
			}
			respond(w, http.StatusOK, review, err)
		}
	})
	handle("POST /sightings/record-agreed", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			result, err := server.Reviewer.Store.RecordAgreedSightings(r.Context(), body.Actor)
			respond(w, http.StatusOK, result, err)
		}
	})
	handle("GET /sightings/{id}", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sighting, err := server.Reviewer.Store.GetSighting(r.PathValue("id"))
			respond(w, http.StatusOK, sighting, err)
		}
	})
	handle("GET /sightings/{id}/image", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.SightingImage(ctx, r.PathValue("id"))
			})
		}
	})
	handle("GET /owner-questions", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			questions, err := server.Reviewer.Store.ListQuestions(r.URL.Query().Get("include_answered") == "true", r.URL.Query().Get("photo_review_id"), r.URL.Query().Get("purpose"), limit, offset)
			respond(w, http.StatusOK, questions, err)
		}
	})
	handle("GET /owner-questions/{id}", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			question, err := server.Reviewer.Store.GetQuestion(r.PathValue("id"))
			respond(w, http.StatusOK, question, err)
		}
	})
	handle("POST /owner-questions/{id}/answer", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var answer Answer
			if !decodeBody(w, r, &answer) {
				return
			}
			question, err := server.Reviewer.AnswerQuestion(r.Context(), r.PathValue("id"), answer)
			respond(w, http.StatusOK, question, err)
		}
	})
	handle("GET /owner-questions/{id}/image", func(server *ReviewServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			question, err := server.Reviewer.Store.GetQuestion(r.PathValue("id"))
			if err != nil {
				writeError(w, err)
				return
			}
			writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.SightingImage(ctx, question.SightingID)
			})
		}
	})
	return patterns
}

// auditPass serves one wearable audit pass. It runs until done; a pass
// that stops on an error answers the error with what it did before it.
func auditPass(pass func(ctx context.Context, actor string) (AuditRun, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		run, err := pass(r.Context(), body.Actor)
		if err != nil && (len(run.Actions) > 0 || run.CostUSD > 0) {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrInvalid) {
				status = http.StatusBadRequest
			}
			log.Printf("audit pass stopped: %v", err)
			writeJSON(w, status, map[string]any{"error": err.Error(), "done_before_the_error": run})
			return
		}
		respond(w, http.StatusOK, run, err)
	}
}

// writeRun answers a pass's run; a pass that stopped on an error after
// spending answers the error with what it did before it.
func writeRun(w http.ResponseWriter, run AuditRun, err error) {
	if err != nil && (len(run.Actions) > 0 || run.CostUSD > 0) {
		log.Printf("pass stopped: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "done_before_the_error": run})
		return
	}
	respond(w, http.StatusOK, run, err)
}

func (server *ReviewServer) narrowedTo(ctx context.Context, scope ownership.Scope) (*ReviewServer, error) {
	store, err := server.Reviewer.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &ReviewServer{Reviewer: server.Reviewer.Narrowed(store)}, nil
}
