package possessions

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

// Server serves the HTTP API. Store and Reviewer reach no records until a
// request narrows them to its scope (server_scope.go).
type Server struct {
	Store    *Store
	Reviewer *Reviewer
	// LocalMachine is the name of the machine this server runs on, as
	// harness-store records it: photo-store's source_machine of an original
	// kept here.
	LocalMachine func(ctx context.Context) (string, error)
}

// RegisterHandlers adds every route to mux, and answers their patterns.
func (server *Server) RegisterHandlers(mux *http.ServeMux) []string {
	var patterns []string
	register := server.handlerOn(mux)
	handle := func(pattern string, build func(*Server) http.HandlerFunc) {
		patterns = append(patterns, pattern)
		register(pattern, build)
	}
	handle("GET /health", func(server *Server) http.HandlerFunc { return server.health })
	handle("GET /vocabulary", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, Vocabulary()) }
	})
	handle("GET /kinds", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, KindTypes) }
	})

	handle("GET /possessions", func(server *Server) http.HandlerFunc { return server.listPossessions })
	handle("POST /possessions", func(server *Server) http.HandlerFunc { return server.createPossession })
	handle("GET /possessions/{id}", func(server *Server) http.HandlerFunc { return server.getPossession })
	handle("PATCH /possessions/{id}", func(server *Server) http.HandlerFunc { return server.patchPossession })
	handle("DELETE /possessions/{id}", func(server *Server) http.HandlerFunc { return server.deletePossession })
	handle("POST /possessions/{id}/restore", func(server *Server) http.HandlerFunc { return server.restorePossession })
	handle("GET /possessions/{id}/revisions", func(server *Server) http.HandlerFunc { return server.revisions })
	handle("GET /possessions/{id}/image", func(server *Server) http.HandlerFunc { return server.possessionImage })
	handle("POST /possessions/{id}/photos", func(server *Server) http.HandlerFunc { return server.linkPhoto })
	handle("PUT /originals/{sha256}", func(server *Server) http.HandlerFunc { return server.keepOriginal })
	handle("POST /possessions/classify-kindless", func(server *Server) http.HandlerFunc {
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
	handle("PUT /possessions/{id}/kind", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("POST /possessions/{id}/merge", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("GET /possession-merges", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			merges, err := server.Store.ListPossessionMerges(r.URL.Query().Get("possession_id"), limit, offset)
			respond(w, http.StatusOK, merges, err)
		}
	})
	handle("GET /possession-merges/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			merge, err := server.Store.GetPossessionMerge(r.PathValue("id"))
			respond(w, http.StatusOK, merge, err)
		}
	})
	handle("POST /possession-merges/{id}/undo", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			merge, err := server.Store.UndoPossessionMerge(r.Context(), r.PathValue("id"), body.Actor)
			respond(w, http.StatusOK, merge, err)
		}
	})
	handle("POST /wearables/check-links", func(server *Server) http.HandlerFunc { return server.auditPass(server.Reviewer.CheckWearableLinks) })
	handle("POST /wearables/check-duplicates", func(server *Server) http.HandlerFunc {
		return server.auditPass(server.Reviewer.CheckWearableDuplicates)
	})
	handle("POST /outfits/consolidate", func(server *Server) http.HandlerFunc { return server.auditPass(server.Reviewer.ConsolidateOutfits) })
	handle("POST /outfit-pieces/rematch", func(server *Server) http.HandlerFunc { return server.auditPass(server.Reviewer.RematchOutfitPieces) })
	handle("POST /outfit-looks/read", func(server *Server) http.HandlerFunc { return server.auditPass(server.Reviewer.ReadLooks) })
	handle("POST /outfit-slots/identify", func(server *Server) http.HandlerFunc { return server.auditPass(server.Reviewer.IdentifySlots) })
	handle("POST /outfits/rebuild-from-looks", func(server *Server) http.HandlerFunc {
		return server.auditPass(server.Reviewer.RebuildOutfitsFromLooks)
	})
	handle("GET /outfit-looks", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			looks, err := server.Store.ListLooks(r.URL.Query().Get("worn_on"), r.URL.Query().Get("outfit_id"), limit, offset)
			respond(w, http.StatusOK, looks, err)
		}
	})
	handle("GET /outfit-looks/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			look, err := server.Store.GetLook(r.PathValue("id"))
			respond(w, http.StatusOK, look, err)
		}
	})
	handle("GET /outfit-slots/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			slot, err := server.Store.GetSlot(r.PathValue("id"))
			respond(w, http.StatusOK, slot, err)
		}
	})
	handle("GET /outfit-slots/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) { return server.Reviewer.SlotImage(ctx, r.PathValue("id")) })
		}
	})
	handle("GET /outfit-rebuilds/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rebuild, err := server.Store.GetOutfitRebuild(r.PathValue("id"))
			respond(w, http.StatusOK, rebuild, err)
		}
	})
	handle("POST /outfit-rebuilds/{id}/undo", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			rebuild, err := server.Store.UndoOutfitRebuild(r.Context(), r.PathValue("id"), body.Actor)
			respond(w, http.StatusOK, rebuild, err)
		}
	})
	handle("GET /wearable-groups", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			groups, err := server.Store.ListWearableGroups()
			respond(w, http.StatusOK, groups, err)
		}
	})
	handle("PUT /wearable-groups/{group}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body WearableGroupChange
			if !decodeBody(w, r, &body) {
				return
			}
			group, err := server.Store.ChangeWearableGroup(r.Context(), r.PathValue("group"), body)
			respond(w, http.StatusOK, group, err)
		}
	})
	handle("POST /outfits/{id}/combine", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("GET /outfit-merges", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			merges, err := server.Store.ListOutfitMerges(r.URL.Query().Get("outfit_id"), limit, offset)
			respond(w, http.StatusOK, merges, err)
		}
	})
	handle("GET /outfit-merges/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			merge, err := server.Store.GetOutfitMerge(r.PathValue("id"))
			respond(w, http.StatusOK, merge, err)
		}
	})
	handle("POST /outfit-merges/{id}/undo", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			merge, err := server.Store.UndoOutfitMerge(r.Context(), r.PathValue("id"), body.Actor)
			respond(w, http.StatusOK, merge, err)
		}
	})
	handle("DELETE /possessions/{id}/photos/{photo_id}", func(server *Server) http.HandlerFunc { return server.unlinkPhoto })

	for _, kind := range KindTypes {
		kind := kind
		handle("GET /"+kind.Route, func(server *Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				filter, err := possessionFilter(r)
				if err != nil {
					writeError(w, err)
					return
				}
				filter.Kind = kind.Name
				possessions, err := server.Store.ListPossessions(filter)
				respond(w, http.StatusOK, possessions, err)
			}
		})
		handle("GET /"+kind.Route+"/{id}", func(server *Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				possession, err := server.kindRecord(kind, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
				respond(w, http.StatusOK, possession, err)
			}
		})
		handle("GET /"+kind.Route+"/{id}/image", func(server *Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) { return server.kindImage(ctx, kind, r.PathValue("id")) })
			}
		})
	}
	handle("GET /harness-machines/{machine_id}/computer", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			possession, err := server.Store.ComputerForHarnessMachine(r.PathValue("machine_id"))
			respond(w, http.StatusOK, possession, err)
		}
	})
	handle("POST /vehicles/{id}/odometer-readings", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})

	handle("GET /outfits", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("GET /outfit-tags", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counts, err := server.Store.OutfitTagCounts()
			respond(w, http.StatusOK, counts, err)
		}
	})
	handle("POST /outfit-covers/read-wearer-boxes", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			result, err := server.Reviewer.ReadMissingCoverWearerBoxes(r.Context(), body.Actor)
			respond(w, http.StatusOK, result, err)
		}
	})
	handle("POST /outfits", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("GET /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			outfit, err := server.outfitWithCoverBox(r.Context(), r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
			respond(w, http.StatusOK, outfit, err)
		}
	})
	handle("PATCH /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("DELETE /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("PUT /outfits/{id}/cover", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("GET /outfits/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.OutfitCoverImage(ctx, r.PathValue("id"))
			})
		}
	})
	handle("POST /outfits/{id}/wearings", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				WornOn string `json:"worn_on"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			outfit, err := server.Store.RecordWearing(r.PathValue("id"), body.WornOn)
			respond(w, http.StatusCreated, outfit, err)
		}
	})

	handle("GET /purchases", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			purchases, err := server.Store.ListPurchases(r.URL.Query().Get("marketplace"), limit, offset)
			respond(w, http.StatusOK, purchases, err)
		}
	})
	handle("POST /purchases", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var purchase Purchase
			if !decodeBody(w, r, &purchase) {
				return
			}
			created, err := server.Store.CreatePurchase(r.Context(), purchase)
			respond(w, http.StatusCreated, created, err)
		}
	})
	handle("DELETE /purchases/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			withdrawn, err := server.Store.DeletePurchase(r.Context(), r.PathValue("id"), r.URL.Query().Get("actor"), r.URL.Query().Get("reason"))
			respond(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id"), "withdrawn_question_ids": withdrawn}, err)
		}
	})
	handle("GET /purchases/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			purchase, err := server.Store.GetPurchase(r.PathValue("id"))
			respond(w, http.StatusOK, purchase, err)
		}
	})
	handle("POST /marketplace-orders/import-ebay-mail", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request EbayImportRequest
			if !decodeBody(w, r, &request) {
				return
			}
			run, err := server.Reviewer.ImportEbayOrders(r.Context(), request)
			server.writeRun(w, run, err)
		}
	})
	handle("POST /marketplace-order-lines/match", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor              string `json:"actor"`
				MarketplaceOrderID string `json:"marketplace_order_id"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			run, err := server.Reviewer.MatchOrderLines(r.Context(), body.Actor, body.MarketplaceOrderID)
			server.writeRun(w, run, err)
		}
	})
	handle("GET /products", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			products, err := server.Store.ListProducts(r.URL.Query().Get("q"), limit, offset)
			respond(w, http.StatusOK, products, err)
		}
	})
	handle("POST /products", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request ProductRequest
			if !decodeBody(w, r, &request) {
				return
			}
			product, err := server.Reviewer.CreateProduct(r.Context(), request)
			respond(w, http.StatusCreated, product, err)
		}
	})
	handle("GET /products/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			product, err := server.Store.GetProduct(r.PathValue("id"))
			respond(w, http.StatusOK, product, err)
		}
	})
	handle("PATCH /products/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})
	handle("PATCH /purchase-lines/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
		}
	})

	handle("GET /photo-reviews", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			reviews, err := server.Store.ListPhotoReviews(r.URL.Query().Get("status"), limit, offset)
			respond(w, http.StatusOK, reviews, err)
		}
	})
	handle("POST /photo-reviews", func(server *Server) http.HandlerFunc {
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
		}
	})
	handle("GET /photo-reviews/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			review, err := server.Store.GetPhotoReview(r.PathValue("id"))
			respond(w, http.StatusOK, review, err)
		}
	})
	handle("POST /photo-reviews/{id}/retry", func(server *Server) http.HandlerFunc {
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
			review, err := server.Store.RetryPhotoReview(r.PathValue("id"), body.Actor)
			if err == nil {
				server.Reviewer.Wake()
			}
			respond(w, http.StatusOK, review, err)
		}
	})
	handle("POST /sightings/record-agreed", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			result, err := server.Store.RecordAgreedSightings(r.Context(), body.Actor)
			respond(w, http.StatusOK, result, err)
		}
	})
	handle("GET /sightings/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sighting, err := server.Store.GetSighting(r.PathValue("id"))
			respond(w, http.StatusOK, sighting, err)
		}
	})
	handle("GET /sightings/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.SightingImage(ctx, r.PathValue("id"))
			})
		}
	})
	handle("GET /owner-questions", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := paging(r)
			if err != nil {
				writeError(w, err)
				return
			}
			questions, err := server.Store.ListQuestions(r.URL.Query().Get("include_answered") == "true", r.URL.Query().Get("photo_review_id"), r.URL.Query().Get("purpose"), limit, offset)
			respond(w, http.StatusOK, questions, err)
		}
	})
	handle("POST /owner-questions/triage-outfit-doubts", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !decodeBody(w, r, &body) {
				return
			}
			result, err := server.Reviewer.TriageOutfitDoubts(r.Context(), body.Actor)
			respond(w, http.StatusOK, result, err)
		}
	})
	handle("GET /owner-questions/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			question, err := server.Store.GetQuestion(r.PathValue("id"))
			respond(w, http.StatusOK, question, err)
		}
	})
	handle("POST /owner-questions/{id}/answer", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var answer Answer
			if !decodeBody(w, r, &answer) {
				return
			}
			question, err := server.Reviewer.AnswerQuestion(r.Context(), r.PathValue("id"), answer)
			respond(w, http.StatusOK, question, err)
		}
	})
	handle("GET /owner-questions/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			question, err := server.Store.GetQuestion(r.PathValue("id"))
			if err != nil {
				writeError(w, err)
				return
			}
			server.writeImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.SightingImage(ctx, question.SightingID)
			})
		}
	})
	return patterns
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
		"photo_reviews_queued":    `SELECT COUNT(*) FROM photo_reviews WHERE status IN ('queued', 'reading', 'reconciling') AND reading != 'look'`,
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

// keepOriginal is PUT /originals/{sha256}?extension=.jpg&added_by=…: the body
// is the original's bytes, kept for the owner the request acts for. It
// answers the original and where photo-store should say it is: this server's
// machine, and a path naming the kept file.
func (server *Server) keepOriginal(w http.ResponseWriter, r *http.Request) {
	content, err := io.ReadAll(io.LimitReader(r.Body, maximumOriginalBytes+1))
	if err != nil {
		writeError(w, fmt.Errorf("%w: reading the original: %v", ErrInvalid, err))
		return
	}
	if len(content) > maximumOriginalBytes {
		writeError(w, fmt.Errorf("%w: an original is at most %d bytes", ErrInvalid, maximumOriginalBytes))
		return
	}
	original, created, err := server.Store.KeepOriginal(r.Context(), r.PathValue("sha256"), r.URL.Query().Get("extension"), content, r.URL.Query().Get("added_by"))
	if err != nil {
		writeError(w, err)
		return
	}
	machine, err := server.LocalMachine(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("the original is kept, but this server's machine could not be read from harness-store: %w", err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"original": original, "created": created,
		"source_machine": machine, "source_path": OriginalSourcePath(original)})
}

// maximumOriginalBytes bounds one original: a phone's full-size JPEG is a few
// megabytes.
const maximumOriginalBytes = 64 << 20

// OriginalSourcePath is the path photo-store keeps for a photo whose
// original is kept here: it names the original by its sha256, as the file
// in originals/ was named before the store moved into this server.
func OriginalSourcePath(original Original) string {
	return "possession-store/originals/" + original.Sha256 + original.Extension
}
