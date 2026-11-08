package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

// Server serves the HTTP API.
type Server struct {
	Store    *Store
	Reviewer *Reviewer
}

// RegisterHandlers adds every route to mux.
func (server *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", server.health)
	mux.HandleFunc("GET /vocabulary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, Vocabulary()) })
	mux.HandleFunc("GET /kinds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, KindTypes) })

	mux.HandleFunc("GET /possessions", server.listPossessions)
	mux.HandleFunc("POST /possessions", server.createPossession)
	mux.HandleFunc("GET /possessions/{id}", server.getPossession)
	mux.HandleFunc("PATCH /possessions/{id}", server.patchPossession)
	mux.HandleFunc("DELETE /possessions/{id}", server.deletePossession)
	mux.HandleFunc("POST /possessions/{id}/restore", server.restorePossession)
	mux.HandleFunc("GET /possessions/{id}/revisions", server.revisions)
	mux.HandleFunc("GET /possessions/{id}/image", server.possessionImage)
	mux.HandleFunc("POST /possessions/{id}/photos", server.linkPhoto)
	mux.HandleFunc("POST /possessions/classify-kindless", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor     string `json:"actor"`
			BatchSize int    `json:"batch_size"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		run, err := server.Reviewer.ClassifyKindless(r.Context(), body.Actor, body.BatchSize)
		respond(w, http.StatusOK, run, err)
	})
	mux.HandleFunc("PUT /possessions/{id}/kind", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind   string                     `json:"kind"`
			Fields map[string]json.RawMessage `json:"fields"`
			Actor  string                     `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		possession, err := server.Store.ChangeKind(r.Context(), r.PathValue("id"), body.Kind, body.Fields, body.Actor)
		respond(w, http.StatusOK, possession, err)
	})
	mux.HandleFunc("POST /possessions/{id}/merge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Into   string `json:"into"`
			Actor  string `json:"actor"`
			Reason string `json:"reason"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		kept, err := server.Store.MergePossession(r.Context(), r.PathValue("id"), body.Into, body.Actor, body.Reason)
		respond(w, http.StatusOK, kept, err)
	})
	mux.HandleFunc("GET /possession-merges", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		merges, err := server.Store.ListPossessionMerges(r.URL.Query().Get("possession_id"), limit, offset)
		respond(w, http.StatusOK, merges, err)
	})
	mux.HandleFunc("GET /possession-merges/{id}", func(w http.ResponseWriter, r *http.Request) {
		merge, err := server.Store.GetPossessionMerge(r.PathValue("id"))
		respond(w, http.StatusOK, merge, err)
	})
	mux.HandleFunc("POST /possession-merges/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		merge, err := server.Store.UndoPossessionMerge(r.Context(), r.PathValue("id"), body.Actor)
		respond(w, http.StatusOK, merge, err)
	})
	mux.HandleFunc("POST /wearables/check-links", server.auditPass(server.Reviewer.CheckWearableLinks))
	mux.HandleFunc("POST /wearables/check-duplicates", server.auditPass(server.Reviewer.CheckWearableDuplicates))
	mux.HandleFunc("POST /outfits/consolidate", server.auditPass(server.Reviewer.ConsolidateOutfits))
	mux.HandleFunc("POST /outfit-pieces/rematch", server.auditPass(server.Reviewer.RematchOutfitPieces))
	mux.HandleFunc("POST /outfit-looks/read", server.auditPass(server.Reviewer.ReadLooks))
	mux.HandleFunc("POST /outfit-slots/identify", server.auditPass(server.Reviewer.IdentifySlots))
	mux.HandleFunc("POST /outfits/rebuild-from-looks", server.auditPass(server.Store.RebuildOutfitsFromLooks))
	mux.HandleFunc("GET /outfit-looks", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		looks, err := server.Store.ListLooks(r.URL.Query().Get("worn_on"), r.URL.Query().Get("outfit_id"), limit, offset)
		respond(w, http.StatusOK, looks, err)
	})
	mux.HandleFunc("GET /outfit-looks/{id}", func(w http.ResponseWriter, r *http.Request) {
		look, err := server.Store.GetLook(r.PathValue("id"))
		respond(w, http.StatusOK, look, err)
	})
	mux.HandleFunc("GET /outfit-slots/{id}", func(w http.ResponseWriter, r *http.Request) {
		slot, err := server.Store.GetSlot(r.PathValue("id"))
		respond(w, http.StatusOK, slot, err)
	})
	mux.HandleFunc("GET /outfit-slots/{id}/image", func(w http.ResponseWriter, r *http.Request) {
		server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) { return server.Reviewer.SlotImage(ctx, r.PathValue("id")) })
	})
	mux.HandleFunc("GET /outfit-rebuilds/{id}", func(w http.ResponseWriter, r *http.Request) {
		rebuild, err := server.Store.GetOutfitRebuild(r.PathValue("id"))
		respond(w, http.StatusOK, rebuild, err)
	})
	mux.HandleFunc("POST /outfit-rebuilds/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		rebuild, err := server.Store.UndoOutfitRebuild(r.Context(), r.PathValue("id"), body.Actor)
		respond(w, http.StatusOK, rebuild, err)
	})
	mux.HandleFunc("GET /wearable-groups", func(w http.ResponseWriter, r *http.Request) {
		groups, err := server.Store.ListWearableGroups()
		respond(w, http.StatusOK, groups, err)
	})
	mux.HandleFunc("PUT /wearable-groups/{group}", func(w http.ResponseWriter, r *http.Request) {
		var body WearableGroupChange
		if !decodeBody(w, r, &body) {
			return
		}
		group, err := server.Store.ChangeWearableGroup(r.Context(), r.PathValue("group"), body)
		respond(w, http.StatusOK, group, err)
	})
	mux.HandleFunc("POST /outfits/{id}/combine", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Into   string `json:"into"`
			Actor  string `json:"actor"`
			Reason string `json:"reason"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		merge, err := server.Store.CombineOutfits(r.Context(), r.PathValue("id"), body.Into, body.Actor, body.Reason)
		respond(w, http.StatusOK, merge, err)
	})
	mux.HandleFunc("GET /outfit-merges", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		merges, err := server.Store.ListOutfitMerges(r.URL.Query().Get("outfit_id"), limit, offset)
		respond(w, http.StatusOK, merges, err)
	})
	mux.HandleFunc("GET /outfit-merges/{id}", func(w http.ResponseWriter, r *http.Request) {
		merge, err := server.Store.GetOutfitMerge(r.PathValue("id"))
		respond(w, http.StatusOK, merge, err)
	})
	mux.HandleFunc("POST /outfit-merges/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		merge, err := server.Store.UndoOutfitMerge(r.Context(), r.PathValue("id"), body.Actor)
		respond(w, http.StatusOK, merge, err)
	})
	mux.HandleFunc("DELETE /possessions/{id}/photos/{photo_id}", server.unlinkPhoto)

	for _, kind := range KindTypes {
		kind := kind
		mux.HandleFunc("GET /"+kind.Route, func(w http.ResponseWriter, r *http.Request) {
			filter, err := possessionFilter(r)
			if err != nil {
				writeError(w, err)
				return
			}
			filter.Kind = kind.Name
			possessions, err := server.Store.ListPossessions(filter)
			respond(w, http.StatusOK, possessions, err)
		})
		mux.HandleFunc("GET /"+kind.Route+"/{id}", func(w http.ResponseWriter, r *http.Request) {
			possessionID, err := server.Store.PossessionIDForKindRecord(kind, r.PathValue("id"))
			if err != nil {
				writeError(w, err)
				return
			}
			possession, err := server.Store.GetPossession(possessionID, r.URL.Query().Get("include_deleted") == "true")
			respond(w, http.StatusOK, possession, err)
		})
		mux.HandleFunc("GET /"+kind.Route+"/{id}/image", func(w http.ResponseWriter, r *http.Request) {
			possessionID, err := server.Store.PossessionIDForKindRecord(kind, r.PathValue("id"))
			if err != nil {
				writeError(w, err)
				return
			}
			server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) { return server.Reviewer.PossessionImage(ctx, possessionID) })
		})
	}
	mux.HandleFunc("GET /harness-machines/{machine_id}/computer", func(w http.ResponseWriter, r *http.Request) {
		possession, err := server.Store.ComputerForHarnessMachine(r.PathValue("machine_id"))
		respond(w, http.StatusOK, possession, err)
	})
	mux.HandleFunc("POST /vehicles/{id}/odometer-readings", func(w http.ResponseWriter, r *http.Request) {
		var reading OdometerReading
		if !decodeBody(w, r, &reading) {
			return
		}
		if err := server.Store.AddOdometerReading(r.PathValue("id"), reading); err != nil {
			writeError(w, err)
			return
		}
		vehicle, _ := KindByName("vehicle")
		possessionID, err := server.Store.PossessionIDForKindRecord(vehicle, r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		possession, err := server.Store.GetPossession(possessionID, false)
		respond(w, http.StatusCreated, possession, err)
	})

	mux.HandleFunc("GET /outfits", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		query := r.URL.Query()
		favoriteOnly := false
		switch query.Get("favorite") {
		case "", "false":
		case "true":
			favoriteOnly = true
		default:
			writeError(w, fmt.Errorf("%w: favorite is true or false", ErrInvalid))
			return
		}
		outfits, err := server.Store.FindOutfits(OutfitFilter{Status: query.Get("status"), PossessionID: query.Get("possession_id"),
			FavoriteOnly: favoriteOnly, Tag: query.Get("tag"), Limit: limit, Offset: offset})
		if err == nil {
			err = server.Reviewer.FillOutfitCoverBoxes(r.Context(), outfits)
		}
		if err == nil {
			server.Reviewer.WarmOutfitCovers(outfits)
			server.Reviewer.QueueCoverWearerReads(outfits)
		}
		respond(w, http.StatusOK, outfits, err)
	})
	mux.HandleFunc("GET /outfit-tags", func(w http.ResponseWriter, r *http.Request) {
		counts, err := server.Store.OutfitTagCounts()
		respond(w, http.StatusOK, counts, err)
	})
	mux.HandleFunc("POST /outfit-covers/read-wearer-boxes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		result, err := server.Reviewer.ReadMissingCoverWearerBoxes(r.Context(), body.Actor)
		respond(w, http.StatusOK, result, err)
	})
	mux.HandleFunc("POST /outfits", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OutfitInput
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		outfit, err := server.Store.CreateOutfit(r.Context(), body.OutfitInput, body.Actor)
		if err == nil {
			err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
		}
		respond(w, http.StatusCreated, outfit, err)
	})
	mux.HandleFunc("GET /outfits/{id}", func(w http.ResponseWriter, r *http.Request) {
		outfit, err := server.Store.GetOutfit(r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
		if err == nil {
			err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
		}
		respond(w, http.StatusOK, outfit, err)
	})
	mux.HandleFunc("PATCH /outfits/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OutfitInput
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		outfit, err := server.Store.PatchOutfit(r.Context(), r.PathValue("id"), body.OutfitInput, body.Actor)
		if err == nil {
			err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
		}
		respond(w, http.StatusOK, outfit, err)
	})
	mux.HandleFunc("DELETE /outfits/{id}", func(w http.ResponseWriter, r *http.Request) {
		actor := r.URL.Query().Get("actor")
		if actor == "" {
			writeError(w, fmt.Errorf("%w: ?actor= is required", ErrInvalid))
			return
		}
		if err := server.Store.DeleteOutfit(r.PathValue("id"), actor); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /outfits/{id}/cover", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PhotoID *string `json:"photo_id"`
			Actor   string  `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.PhotoID == nil {
			writeError(w, fmt.Errorf("%w: photo_id is required (\"\" takes the owner's pick away)", ErrInvalid))
			return
		}
		outfit, err := server.Store.SetOutfitCover(r.Context(), r.PathValue("id"), *body.PhotoID, body.Actor)
		if err == nil {
			// The pick is kept even when the reader fails; the error says so.
			if readErr := server.Reviewer.EnsureCoverWearerBox(r.Context(), outfit, body.Actor); readErr != nil {
				err = fmt.Errorf("the cover is now %s, but its wearer box could not be read, so it is cut to the piece boxes: %w", valueOrEmpty(outfit.CoverPhotoID), readErr)
			}
		}
		if err == nil {
			err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
		}
		respond(w, http.StatusOK, outfit, err)
	})
	mux.HandleFunc("GET /outfits/{id}/image", func(w http.ResponseWriter, r *http.Request) {
		server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
			return server.Reviewer.OutfitCoverImage(ctx, r.PathValue("id"))
		})
	})
	mux.HandleFunc("POST /outfits/{id}/wearings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			WornOn string `json:"worn_on"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		outfit, err := server.Store.RecordWearing(r.PathValue("id"), body.WornOn)
		respond(w, http.StatusCreated, outfit, err)
	})

	mux.HandleFunc("GET /purchases", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		purchases, err := server.Store.ListPurchases(r.URL.Query().Get("marketplace"), limit, offset)
		respond(w, http.StatusOK, purchases, err)
	})
	mux.HandleFunc("POST /purchases", func(w http.ResponseWriter, r *http.Request) {
		var purchase Purchase
		if !decodeBody(w, r, &purchase) {
			return
		}
		created, err := server.Store.CreatePurchase(r.Context(), purchase)
		respond(w, http.StatusCreated, created, err)
	})
	mux.HandleFunc("DELETE /purchases/{id}", func(w http.ResponseWriter, r *http.Request) {
		withdrawn, err := server.Store.DeletePurchase(r.Context(), r.PathValue("id"), r.URL.Query().Get("actor"), r.URL.Query().Get("reason"))
		respond(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id"), "withdrawn_question_ids": withdrawn}, err)
	})
	mux.HandleFunc("GET /purchases/{id}", func(w http.ResponseWriter, r *http.Request) {
		purchase, err := server.Store.GetPurchase(r.PathValue("id"))
		respond(w, http.StatusOK, purchase, err)
	})
	mux.HandleFunc("POST /marketplace-orders/import-ebay-mail", func(w http.ResponseWriter, r *http.Request) {
		var request EbayImportRequest
		if !decodeBody(w, r, &request) {
			return
		}
		run, err := server.Reviewer.ImportEbayOrders(r.Context(), request)
		server.writeRun(w, run, err)
	})
	mux.HandleFunc("POST /marketplace-order-lines/match", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor              string `json:"actor"`
			MarketplaceOrderID string `json:"marketplace_order_id"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		run, err := server.Reviewer.MatchOrderLines(r.Context(), body.Actor, body.MarketplaceOrderID)
		server.writeRun(w, run, err)
	})
	mux.HandleFunc("GET /products", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		products, err := server.Store.ListProducts(r.URL.Query().Get("q"), limit, offset)
		respond(w, http.StatusOK, products, err)
	})
	mux.HandleFunc("POST /products", func(w http.ResponseWriter, r *http.Request) {
		var request ProductRequest
		if !decodeBody(w, r, &request) {
			return
		}
		product, err := server.Reviewer.CreateProduct(r.Context(), request)
		respond(w, http.StatusCreated, product, err)
	})
	mux.HandleFunc("GET /products/{id}", func(w http.ResponseWriter, r *http.Request) {
		product, err := server.Store.GetProduct(r.PathValue("id"))
		respond(w, http.StatusOK, product, err)
	})
	mux.HandleFunc("PATCH /products/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if !decodeBody(w, r, &body) {
			return
		}
		var actor string
		if raw, has := body["actor"]; has {
			if err := json.Unmarshal(raw, &actor); err != nil {
				writeError(w, fmt.Errorf("%w: actor: %v", ErrInvalid, err))
				return
			}
			delete(body, "actor")
		}
		product, err := server.Store.PatchProduct(r.Context(), r.PathValue("id"), body, actor)
		respond(w, http.StatusOK, product, err)
	})
	mux.HandleFunc("PATCH /purchase-lines/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Disposition string `json:"disposition"`
			Actor       string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.Actor == "" {
			writeError(w, fmt.Errorf("%w: actor is required", ErrInvalid))
			return
		}
		if err := server.Store.SetPurchaseLineDisposition(r.PathValue("id"), body.Disposition, body.Actor); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /photo-reviews", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		reviews, err := server.Store.ListPhotoReviews(r.URL.Query().Get("status"), limit, offset)
		respond(w, http.StatusOK, reviews, err)
	})
	mux.HandleFunc("POST /photo-reviews", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PhotoID        string `json:"photo_id"`
			QueuedBy       string `json:"queued_by"`
			Reading        string `json:"reading"`
			WearerPersonID string `json:"wearer_person_id"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		review, created, err := server.Store.QueuePhotoReview(r.Context(), body.PhotoID, body.QueuedBy, body.Reading, body.WearerPersonID)
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
	})
	mux.HandleFunc("GET /photo-reviews/{id}", func(w http.ResponseWriter, r *http.Request) {
		review, err := server.Store.GetPhotoReview(r.PathValue("id"))
		respond(w, http.StatusOK, review, err)
	})
	mux.HandleFunc("POST /photo-reviews/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
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
		review, err := server.Store.RetryPhotoReview(r.PathValue("id"), body.Actor)
		if err == nil {
			server.Reviewer.Wake()
		}
		respond(w, http.StatusOK, review, err)
	})
	mux.HandleFunc("POST /sightings/record-agreed", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		result, err := server.Store.RecordAgreedSightings(r.Context(), body.Actor)
		respond(w, http.StatusOK, result, err)
	})
	mux.HandleFunc("GET /sightings/{id}", func(w http.ResponseWriter, r *http.Request) {
		sighting, err := server.Store.GetSighting(r.PathValue("id"))
		respond(w, http.StatusOK, sighting, err)
	})
	mux.HandleFunc("GET /sightings/{id}/image", func(w http.ResponseWriter, r *http.Request) {
		server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
			return server.Reviewer.SightingImage(ctx, r.PathValue("id"))
		})
	})
	mux.HandleFunc("GET /owner-questions", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := paging(r)
		if err != nil {
			writeError(w, err)
			return
		}
		questions, err := server.Store.ListQuestions(r.URL.Query().Get("include_answered") == "true", r.URL.Query().Get("photo_review_id"), r.URL.Query().Get("purpose"), limit, offset)
		respond(w, http.StatusOK, questions, err)
	})
	mux.HandleFunc("POST /owner-questions/triage-outfit-doubts", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		result, err := server.Reviewer.TriageOutfitDoubts(r.Context(), body.Actor)
		respond(w, http.StatusOK, result, err)
	})
	mux.HandleFunc("GET /owner-questions/{id}", func(w http.ResponseWriter, r *http.Request) {
		question, err := server.Store.GetQuestion(r.PathValue("id"))
		respond(w, http.StatusOK, question, err)
	})
	mux.HandleFunc("POST /owner-questions/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		var answer Answer
		if !decodeBody(w, r, &answer) {
			return
		}
		question, err := server.Reviewer.AnswerQuestion(r.Context(), r.PathValue("id"), answer)
		respond(w, http.StatusOK, question, err)
	})
	mux.HandleFunc("GET /owner-questions/{id}/image", func(w http.ResponseWriter, r *http.Request) {
		question, err := server.Store.GetQuestion(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
			return server.Reviewer.SightingImage(ctx, question.SightingID)
		})
	})
}

// auditPass serves one wearable audit pass. It runs until done; a pass
// that stops on an error answers the error with what it did before it.
func (server *Server) auditPass(pass func(ctx context.Context, actor string) (AuditRun, error)) http.HandlerFunc {
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
func (server *Server) writeRun(w http.ResponseWriter, run AuditRun, err error) {
	if err != nil && (len(run.Actions) > 0 || run.CostUSD > 0) {
		log.Printf("pass stopped: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "done_before_the_error": run})
		return
	}
	respond(w, http.StatusOK, run, err)
}

func (server *Server) health(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	for name, query := range map[string]string{
		"possessions":             `SELECT COUNT(*) FROM possessions WHERE deleted_at = 0`,
		"outfits":                 `SELECT COUNT(*) FROM outfits WHERE deleted_at = 0`,
		"purchases":               `SELECT COUNT(*) FROM purchases WHERE deleted_at = 0`,
		"products":                `SELECT COUNT(*) FROM products WHERE deleted_at = 0`,
		"photo_reviews_queued":    `SELECT COUNT(*) FROM photo_reviews WHERE status IN ('queued', 'reading', 'reconciling')`,
		"photo_reviews_failed":    `SELECT COUNT(*) FROM photo_reviews WHERE status = 'failed'`,
		"open_decision_questions": `SELECT COUNT(*) FROM owner_questions WHERE answered_at = 0 AND purpose = 'decision'`,
		"open_detail_questions":   `SELECT COUNT(*) FROM owner_questions WHERE answered_at = 0 AND purpose = 'detail'`,
	} {
		var count int
		if err := server.Store.db.QueryRow(query).Scan(&count); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
			return
		}
		counts[name] = count
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func possessionFilter(r *http.Request) (PossessionFilter, error) {
	limit, offset, err := paging(r)
	if err != nil {
		return PossessionFilter{}, err
	}
	query := r.URL.Query()
	return PossessionFilter{Query: query.Get("q"), Status: query.Get("status"), Kind: query.Get("kind"), PlaceID: query.Get("place_id"),
		IncludeDeleted: query.Get("include_deleted") == "true", Limit: limit, Offset: offset}, nil
}

func (server *Server) listPossessions(w http.ResponseWriter, r *http.Request) {
	filter, err := possessionFilter(r)
	if err != nil {
		writeError(w, err)
		return
	}
	possessions, err := server.Store.ListPossessions(filter)
	respond(w, http.StatusOK, possessions, err)
}

// actorAndFields reads a possession body: its fields plus "actor".
func actorAndFields(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, string, bool) {
	var fields map[string]json.RawMessage
	if !decodeBody(w, r, &fields) {
		return nil, "", false
	}
	var actor string
	if raw, present := fields["actor"]; present {
		if err := json.Unmarshal(raw, &actor); err != nil {
			writeError(w, fmt.Errorf("%w: actor must be a string", ErrInvalid))
			return nil, "", false
		}
		delete(fields, "actor")
	}
	return fields, actor, true
}

func (server *Server) createPossession(w http.ResponseWriter, r *http.Request) {
	fields, actor, ok := actorAndFields(w, r)
	if !ok {
		return
	}
	possession, err := server.Store.CreatePossession(r.Context(), fields, actor)
	respond(w, http.StatusCreated, possession, err)
}

func (server *Server) getPossession(w http.ResponseWriter, r *http.Request) {
	possession, err := server.Store.GetPossession(r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	respond(w, http.StatusOK, possession, err)
}

func (server *Server) patchPossession(w http.ResponseWriter, r *http.Request) {
	fields, actor, ok := actorAndFields(w, r)
	if !ok {
		return
	}
	possession, err := server.Store.PatchPossession(r.Context(), r.PathValue("id"), fields, actor)
	respond(w, http.StatusOK, possession, err)
}

func (server *Server) deletePossession(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	if err := server.Store.DeletePossession(r.PathValue("id"), actor); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) restorePossession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Actor string `json:"actor"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	possession, err := server.Store.RestorePossession(r.PathValue("id"), body.Actor)
	respond(w, http.StatusOK, possession, err)
}

func (server *Server) revisions(w http.ResponseWriter, r *http.Request) {
	revisions, err := server.Store.Revisions(r.PathValue("id"))
	respond(w, http.StatusOK, revisions, err)
}

func (server *Server) possessionImage(w http.ResponseWriter, r *http.Request) {
	server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
		return server.Reviewer.PossessionImage(ctx, r.PathValue("id"))
	})
}

func (server *Server) linkPhoto(w http.ResponseWriter, r *http.Request) {
	var link PhotoLink
	if !decodeBody(w, r, &link) {
		return
	}
	possession, err := server.Store.LinkPhoto(r.Context(), r.PathValue("id"), link)
	respond(w, http.StatusOK, possession, err)
}

func (server *Server) unlinkPhoto(w http.ResponseWriter, r *http.Request) {
	possession, err := server.Store.UnlinkPhoto(r.Context(), r.PathValue("id"), r.PathValue("photo_id"))
	respond(w, http.StatusOK, possession, err)
}

func (server *Server) writeImage(w http.ResponseWriter, ctx context.Context, make func(context.Context) ([]byte, error)) {
	image, err := make(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if _, err := w.Write(image); err != nil {
		log.Printf("write image: %v", err)
	}
}

func paging(r *http.Request) (int, int, error) {
	limit, offset := 0, 0
	var err error
	if text := r.URL.Query().Get("limit"); text != "" {
		if limit, err = strconv.Atoi(text); err != nil || limit < 0 {
			return 0, 0, fmt.Errorf("%w: limit must be a whole number", ErrInvalid)
		}
	}
	if text := r.URL.Query().Get("offset"); text != "" {
		if offset, err = strconv.Atoi(text); err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("%w: offset must be a whole number", ErrInvalid)
		}
	}
	return limit, offset, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeError(w, fmt.Errorf("%w: read body: %v", ErrInvalid, err))
		return false
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, fmt.Errorf("%w: body: %v", ErrInvalid, err))
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, value)
}

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
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	}
	if status == http.StatusInternalServerError {
		log.Printf("error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func valueOrEmpty(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
