package outfits

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

// Server serves the outfits' routes: outfits, their merges and covers,
// looks and slots, rebuilds, and the passes that read and identify them.
// Reviewer reaches no records until a request narrows it to its scope
// (possessions/server_scope.go).
type Server struct {
	Reviewer *Reviewer
}

// RegisterHandlers adds the outfits' routes to mux, and answers their
// patterns.
func (server *Server) RegisterHandlers(mux *http.ServeMux) []string {
	var patterns []string
	register := possessions.HandlerOn(mux, server.narrowedTo)
	handle := func(pattern string, build func(*Server) http.HandlerFunc) {
		patterns = append(patterns, pattern)
		register(pattern, build)
	}
	handle("POST /outfits/consolidate", func(server *Server) http.HandlerFunc {
		return review.AuditPass(server.Reviewer.ConsolidateOutfits)
	})
	handle("POST /outfit-looks/read", func(server *Server) http.HandlerFunc { return review.AuditPass(server.Reviewer.ReadLooks) })
	handle("POST /outfit-slots/identify", func(server *Server) http.HandlerFunc {
		return review.AuditPass(server.Reviewer.IdentifySlots)
	})
	handle("POST /outfits/rebuild-from-looks", func(server *Server) http.HandlerFunc {
		return review.AuditPass(server.Reviewer.RebuildOutfitsFromLooks)
	})
	handle("GET /outfit-looks", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := possessions.Paging(r)
			if err != nil {
				possessions.WriteError(w, err)
				return
			}
			looks, err := server.Reviewer.Store.ListLooks(r.URL.Query().Get("worn_on"), r.URL.Query().Get("outfit_id"), limit, offset)
			possessions.Respond(w, http.StatusOK, looks, err)
		}
	})
	handle("GET /outfit-looks/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			look, err := server.Reviewer.Store.GetLook(r.PathValue("id"))
			possessions.Respond(w, http.StatusOK, look, err)
		}
	})
	handle("GET /outfit-slots/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			slot, err := server.Reviewer.Store.GetSlot(r.PathValue("id"))
			possessions.Respond(w, http.StatusOK, slot, err)
		}
	})
	handle("GET /outfit-slots/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			possessions.WriteImage(w, r.Context(), func(ctx context.Context) ([]byte, error) { return server.Reviewer.SlotImage(ctx, r.PathValue("id")) })
		}
	})
	handle("GET /outfit-rebuilds/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rebuild, err := server.Reviewer.Store.GetOutfitRebuild(r.PathValue("id"))
			possessions.Respond(w, http.StatusOK, rebuild, err)
		}
	})
	handle("POST /outfit-rebuilds/{id}/undo", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			rebuild, err := server.Reviewer.Store.UndoOutfitRebuild(r.Context(), r.PathValue("id"), body.Actor)
			possessions.Respond(w, http.StatusOK, rebuild, err)
		}
	})
	handle("POST /outfits/{id}/combine", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Into   string `json:"into"`
				Actor  string `json:"actor"`
				Reason string `json:"reason"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			merge, err := server.Reviewer.Store.CombineOutfits(r.Context(), r.PathValue("id"), body.Into, body.Actor, body.Reason)
			possessions.Respond(w, http.StatusOK, merge, err)
		}
	})
	handle("GET /outfit-merges", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := possessions.Paging(r)
			if err != nil {
				possessions.WriteError(w, err)
				return
			}
			merges, err := server.Reviewer.Store.ListOutfitMerges(r.URL.Query().Get("outfit_id"), limit, offset)
			possessions.Respond(w, http.StatusOK, merges, err)
		}
	})
	handle("GET /outfit-merges/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			merge, err := server.Reviewer.Store.GetOutfitMerge(r.PathValue("id"))
			possessions.Respond(w, http.StatusOK, merge, err)
		}
	})
	handle("POST /outfit-merges/{id}/undo", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			merge, err := server.Reviewer.Store.UndoOutfitMerge(r.Context(), r.PathValue("id"), body.Actor)
			possessions.Respond(w, http.StatusOK, merge, err)
		}
	})
	handle("GET /outfits", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			limit, offset, err := possessions.Paging(r)
			if err != nil {
				possessions.WriteError(w, err)
				return
			}
			query := r.URL.Query()
			favoriteOnly := false
			switch query.Get("favorite") {
			case "", "false":
			case "true":
				favoriteOnly = true
			default:
				possessions.WriteError(w, fmt.Errorf("%w: favorite is true or false", possessions.ErrInvalid))
				return
			}
			outfits, err := server.Reviewer.Store.FindOutfits(OutfitFilter{Status: query.Get("status"), PossessionID: query.Get("possession_id"),
				FavoriteOnly: favoriteOnly, Tag: query.Get("tag"), Limit: limit, Offset: offset})
			if err == nil {
				err = server.Reviewer.FillOutfitCoverBoxes(r.Context(), outfits)
			}
			if err == nil {
				server.Reviewer.WarmOutfitCovers(outfits)
				server.Reviewer.QueueCoverWearerReads(outfits)
			}
			possessions.Respond(w, http.StatusOK, outfits, err)
		}
	})
	handle("GET /outfit-tags", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counts, err := server.Reviewer.Store.OutfitTagCounts()
			possessions.Respond(w, http.StatusOK, counts, err)
		}
	})
	handle("POST /outfit-covers/read-wearer-boxes", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			result, err := server.Reviewer.ReadMissingCoverWearerBoxes(r.Context(), body.Actor)
			possessions.Respond(w, http.StatusOK, result, err)
		}
	})
	handle("POST /outfits", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				OutfitInput
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			outfit, err := server.Reviewer.Store.CreateOutfit(r.Context(), body.OutfitInput, body.Actor)
			if err == nil {
				err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
			}
			possessions.Respond(w, http.StatusCreated, outfit, err)
		}
	})
	handle("GET /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			outfit, err := server.outfitWithCoverBox(r.Context(), r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
			possessions.Respond(w, http.StatusOK, outfit, err)
		}
	})
	handle("PATCH /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				OutfitInput
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			outfit, err := server.Reviewer.Store.PatchOutfit(r.Context(), r.PathValue("id"), body.OutfitInput, body.Actor)
			if err == nil {
				err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
			}
			possessions.Respond(w, http.StatusOK, outfit, err)
		}
	})
	handle("DELETE /outfits/{id}", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			actor := r.URL.Query().Get("actor")
			if actor == "" {
				possessions.WriteError(w, fmt.Errorf("%w: ?actor= is required", possessions.ErrInvalid))
				return
			}
			if err := server.Reviewer.Store.DeleteOutfit(r.PathValue("id"), actor); err != nil {
				possessions.WriteError(w, err)
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
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			if body.PhotoID == nil {
				possessions.WriteError(w, fmt.Errorf("%w: photo_id is required (\"\" takes the owner's pick away)", possessions.ErrInvalid))
				return
			}
			outfit, err := server.Reviewer.Store.SetOutfitCover(r.Context(), r.PathValue("id"), *body.PhotoID, body.Actor)
			if err == nil {
				// The pick is kept even when the reader fails; the error says so.
				if readErr := server.Reviewer.EnsureCoverWearerBox(r.Context(), outfit, body.Actor); readErr != nil {
					err = fmt.Errorf("the cover is now %s, but its wearer box could not be read, so it is cut to the piece boxes: %w", possessions.ValueOrEmpty(outfit.CoverPhotoID), readErr)
				}
			}
			if err == nil {
				err = server.Reviewer.FillOutfitCoverBox(r.Context(), &outfit)
			}
			possessions.Respond(w, http.StatusOK, outfit, err)
		}
	})
	handle("GET /outfits/{id}/image", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			possessions.WriteImage(w, r.Context(), func(ctx context.Context) ([]byte, error) {
				return server.Reviewer.OutfitCoverImage(ctx, r.PathValue("id"))
			})
		}
	})
	handle("POST /outfits/{id}/wearings", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				WornOn string `json:"worn_on"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			outfit, err := server.Reviewer.Store.RecordWearing(r.PathValue("id"), body.WornOn)
			possessions.Respond(w, http.StatusCreated, outfit, err)
		}
	})

	handle("POST /owner-questions/triage-outfit-doubts", func(server *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Actor string `json:"actor"`
			}
			if !possessions.DecodeBody(w, r, &body) {
				return
			}
			result, err := server.Reviewer.TriageOutfitDoubts(r.Context(), body.Actor)
			possessions.Respond(w, http.StatusOK, result, err)
		}
	})
	return patterns
}

func (server *Server) narrowedTo(ctx context.Context, scope ownership.Scope) (*Server, error) {
	reviewer, err := server.Reviewer.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &Server{Reviewer: reviewer}, nil
}
