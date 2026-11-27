package server

import (
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
)

// The rules and descriptions of the possession-store routes
// (possessions_routes.go). They are kept here and added to routeAccessRules,
// routeDescriptions and the README's group order at start, so the shared
// tables gain them in one place.

// possessionStoreRouteRule is the rule of every possession-store route.
var possessionStoreRouteRule = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "possession-store as the caller's principal: every possession, outfit, product, purchase and review record has an owner, and the store narrows every read to the caller's own, or every owner's for an administrator and for the internal service acting as itself, and every write and pass to one owner"}

const routeGroupPossessionStore routeGroup = "possession-store, mounted in this process"

var possessionStoreFixedRouteDescriptions = map[string]string{
	"GET /possessions/health":                                 "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own records",
	"GET /possessions/vocabulary":                             "Every vocabulary a field takes its words from",
	"GET /possessions/kinds":                                  "The kinds a possession may have, each with its fields and id prefix",
	"GET /possessions/possessions":                            "The caller's possessions by `q`, `status`, `kind`, `place_id`, `include_deleted`, `limit` and `offset`, newest first",
	"POST /possessions/possessions":                           "Make a possession owned by the caller, with at most one kind and its photos; 201",
	"GET /possessions/possessions/{id}":                       "One possession with its kind, photos, outfits and product",
	"PATCH /possessions/possessions/{id}":                     "Change a possession's fields or give it a kind",
	"DELETE /possessions/possessions/{id}":                    "Delete a possession, reversibly; 204",
	"POST /possessions/possessions/{id}/restore":              "Undo a delete",
	"GET /possessions/possessions/{id}/revisions":             "A possession's rows as they were before each change, newest first",
	"GET /possessions/possessions/{id}/image":                 "A possession's cover cut from its photo, as a JPEG",
	"POST /possessions/possessions/{id}/photos":               "Link a photo of the owner's to a possession, with its box",
	"DELETE /possessions/possessions/{id}/photos/{photo_id}":  "Unlink a photo from a possession",
	"PUT /possessions/originals/{sha256}":                     "Keep an original's bytes in file-store for the owner the request acts for; answers where photo-store should say it is",
	"POST /possessions/possessions/classify-kindless":         "Give one owner's possessions with no kind one, by the first reader; asks when unsure",
	"PUT /possessions/possessions/{id}/kind":                  "Move a possession to another kind",
	"POST /possessions/possessions/{id}/merge":                "Merge a duplicate possession into another of the same owner's, undoably",
	"GET /possessions/possession-merges":                      "The caller's possession merges, by `possession_id`, newest first",
	"GET /possessions/possession-merges/{id}":                 "One possession merge and every row it moved",
	"POST /possessions/possession-merges/{id}/undo":           "Undo a possession merge",
	"POST /possessions/wearables/check-links":                 "Both readers check one owner's wearables' photo links, unlinking what both doubt",
	"POST /possessions/wearables/check-duplicates":            "Both readers look for one owner's wearables recorded twice, merging what both see as one",
	"POST /possessions/outfits/consolidate":                   "Both readers look for one owner's outfits recorded twice, combining what both see as one",
	"POST /possessions/outfit-pieces/rematch":                 "Match one owner's stuck outfit pieces by picture with both readers",
	"POST /possessions/outfit-looks/read":                     "Both readers read each day's photos of one owner into looks and slots",
	"POST /possessions/outfit-slots/identify":                 "Both readers sort one owner's slots into pieces and link each to its best guess",
	"POST /possessions/outfits/rebuild-from-looks":            "Place every look of one owner into outfits again, undoably",
	"GET /possessions/outfit-looks":                           "The caller's looks by `worn_on` and `outfit_id`",
	"GET /possessions/outfit-looks/{id}":                      "One look with its photos and slots",
	"GET /possessions/outfit-slots/{id}":                      "One slot with its boxes and candidates",
	"GET /possessions/outfit-slots/{id}/image":                "One slot cut from its photo, as a JPEG",
	"GET /possessions/outfit-rebuilds/{id}":                   "One rebuild of outfits from looks",
	"POST /possessions/outfit-rebuilds/{id}/undo":             "Undo a rebuild",
	"GET /possessions/wearable-groups":                        "The caller's wearable groups with their members by status",
	"PUT /possessions/wearable-groups/{group}":                "Close or open a wearable group, or say how many pieces it holds",
	"POST /possessions/outfits/{id}/combine":                  "Combine a duplicate outfit into another of the same owner's, undoably",
	"GET /possessions/outfit-merges":                          "The caller's outfit merges, by `outfit_id`, newest first",
	"GET /possessions/outfit-merges/{id}":                     "One outfit merge and every row it moved",
	"POST /possessions/outfit-merges/{id}/undo":               "Undo an outfit merge",
	"GET /possessions/harness-machines/{machine_id}/computer": "The caller's computer a harness-store machine is",
	"POST /possessions/vehicles/{id}/odometer-readings":       "Record a vehicle's odometer; 201",
	"GET /possessions/outfits":                                "The caller's outfits by `status`, `possession_id`, `favorite`, `tag`, `limit` and `offset`, with their covers",
	"GET /possessions/outfit-tags":                            "Every tag on the caller's live outfits, the most used first",
	"POST /possessions/outfit-covers/read-wearer-boxes":       "Read the wearer box of every cover photo of one owner's outfits that has none",
	"POST /possessions/outfits":                               "Make an outfit owned by the caller of its wearables; 201",
	"GET /possessions/outfits/{id}":                           "One outfit with its cover",
	"PATCH /possessions/outfits/{id}":                         "Change an outfit, its pieces, favorite or tags",
	"DELETE /possessions/outfits/{id}":                        "Delete an outfit; 204",
	"PUT /possessions/outfits/{id}/cover":                     "Pick an outfit's cover photo, or take the pick away",
	"GET /possessions/outfits/{id}/image":                     "An outfit's cover cut to the wearer, as a JPEG",
	"POST /possessions/outfits/{id}/wearings":                 "Record a day an outfit was worn; 201",
	"GET /possessions/purchases":                              "The caller's purchases by `marketplace`, newest first",
	"POST /possessions/purchases":                             "Record a purchase owned by the caller, line by line; 201",
	"DELETE /possessions/purchases/{id}":                      "Delete a purchase, withdrawing its questions",
	"GET /possessions/purchases/{id}":                         "One purchase with its lines",
	"POST /possessions/marketplace-orders/import-ebay-mail":   "Read one owner's eBay order mail with the first reader and record each order in budget-store",
	"POST /possessions/marketplace-order-lines/match":         "Both readers match one owner's open order lines to possessions",
	"GET /possessions/products":                               "The caller's products by `q`, newest first",
	"POST /possessions/products":                              "Make a product owned by the caller from its listing in article-store; 201",
	"GET /possessions/products/{id}":                          "One product with its possessions",
	"PATCH /possessions/products/{id}":                        "Change a product, reading its listing again when it moves",
	"PATCH /possessions/purchase-lines/{id}":                  "Say what a purchase line became; 204",
	"GET /possessions/photo-reviews":                          "The caller's photo reviews by `status`, newest first",
	"POST /possessions/photo-reviews":                         "Queue one of the caller's photos for review; 201, or 200 with the review already queued",
	"GET /possessions/photo-reviews/{id}":                     "One review with its readings, sightings and questions",
	"POST /possessions/photo-reviews/{id}/retry":              "Queue a failed review again",
	"POST /possessions/sightings/record-agreed":               "Record the sightings of one owner both readers agreed on that earlier reviews held back",
	"GET /possessions/sightings/{id}":                         "One sighting",
	"GET /possessions/sightings/{id}/image":                   "One sighting cut from its photo, as a JPEG",
	"GET /possessions/owner-questions":                        "The caller's questions, open ones by default, by `photo_review_id`, `purpose`, `include_answered`",
	"POST /possessions/owner-questions/triage-outfit-doubts":  "Sort one owner's outfit questions into decisions and details",
	"GET /possessions/owner-questions/{id}":                   "One question with its choices",
	"POST /possessions/owner-questions/{id}/answer":           "Answer a question by a choice or in words, applying what it decides",
	"GET /possessions/owner-questions/{id}/image":             "The picture a question asks about, as a JPEG",
}

// possessionStoreRouteDescriptions are the fixed routes and, for every kind,
// its list, its records and their pictures.
func possessionStoreRouteDescriptions() map[string]routeDescription {
	descriptions := map[string]routeDescription{}
	for pattern, description := range possessionStoreFixedRouteDescriptions {
		descriptions[pattern] = routeDescription{routeGroupPossessionStore, description}
	}
	for _, kind := range possessions.KindTypes {
		name := strings.ReplaceAll(kind.Name, "_", " ")
		descriptions["GET /possessions/"+kind.Route] = routeDescription{routeGroupPossessionStore, fmt.Sprintf("The caller's possessions of kind %s, by the possession filters", name)}
		descriptions["GET /possessions/"+kind.Route+"/{id}"] = routeDescription{routeGroupPossessionStore, fmt.Sprintf("The possession a %s record (`%s_…`) belongs to", name, kind.Prefix)}
		descriptions["GET /possessions/"+kind.Route+"/{id}/image"] = routeDescription{routeGroupPossessionStore, fmt.Sprintf("The cover of the possession a %s record belongs to, as a JPEG", name)}
	}
	return descriptions
}

func init() {
	for pattern, description := range possessionStoreRouteDescriptions() {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = possessionStoreRouteRule
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupPossessionStore)
}
