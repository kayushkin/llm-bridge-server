package possessionstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutfitsTakeAFavoriteAndTagsAndFilterOnThem(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	shirt, err := store.CreatePossession(ctx, fields(t, `{"name":"plaid shirt","garment":{"category":"top","garment_type":"flannel_shirt","primary_color":"red"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	status := "kept"
	first, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{shirt["id"].(string)}}, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if first.Favorite || first.Tags == nil || len(first.Tags) != 0 {
		t.Fatalf("a new outfit is no favorite and has no tags: %+v", first)
	}
	tags := []string{"  Date   Night ", "work", "date night"}
	favorite := true
	second, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{shirt["id"].(string)}, Favorite: &favorite, Tags: &tags}, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Favorite || strings.Join(second.Tags, "|") != "date night|work" {
		t.Fatalf("tags are trimmed, lower case and each once: %+v", second.Tags)
	}
	workOnly := []string{"work"}
	if first, err = store.PatchOutfit(ctx, first.ID, OutfitInput{Tags: &workOnly}, "principal_000001"); err != nil || strings.Join(first.Tags, "|") != "work" || first.Favorite {
		t.Fatalf("patch tags = %+v, %v", first, err)
	}
	for _, bad := range [][]string{{" "}, {strings.Repeat("x", outfitTagLongest+1)}} {
		if _, err := store.PatchOutfit(ctx, first.ID, OutfitInput{Tags: &bad}, "principal_000001"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("tags %q should be refused, got %v", bad, err)
		}
	}
	revisions, err := store.Revisions(first.ID)
	if err != nil || len(revisions) != 1 {
		t.Fatalf("a patch keeps the outfit as it was: %v %v", revisions, err)
	}

	mux := http.NewServeMux()
	(&Server{Store: store, Reviewer: &Reviewer{Store: store, Photos: portraitPhotos{}, CoverImagesDirectory: t.TempDir()}}).RegisterHandlers(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		asTestOwner(mux).ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	for path, want := range map[string]string{
		"/outfits?favorite=true":  `"id":"` + second.ID + `"`,
		"/outfits?tag=Date+Night": `"id":"` + second.ID + `"`,
		"/outfits?tag=work":       `"id":"` + first.ID + `"`,
		"/outfit-tags":            `[{"tag":"work","outfits":2},{"tag":"date night","outfits":1}]`,
	} {
		response := call("GET", path, "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("GET %s = %d %s, want %s", path, response.Code, response.Body, want)
		}
	}
	if response := call("GET", "/outfits?favorite=true", ""); strings.Contains(response.Body.String(), first.ID) {
		t.Fatalf("favorite=true should leave out %s: %s", first.ID, response.Body)
	}
	if response := call("GET", "/outfits?favorite=yes", ""); response.Code != 400 {
		t.Fatalf("favorite=yes = %d", response.Code)
	}
	patched := call("PATCH", "/outfits/"+first.ID, `{"actor":"principal_000001","favorite":true,"tags":[]}`)
	if patched.Code != 200 || !strings.Contains(patched.Body.String(), `"favorite":true,"tags":[]`) {
		t.Fatalf("PATCH favorite and clear tags = %d %s", patched.Code, patched.Body)
	}

	// Combining keeps the owner's marks of both, and undo takes back only
	// what the combine gave.
	unfavorite := false
	keptTags := []string{"work"}
	if _, err := store.PatchOutfit(ctx, first.ID, OutfitInput{Favorite: &unfavorite, Tags: &keptTags}, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	merge, err := store.CombineOutfits(ctx, second.ID, first.ID, "principal_000001", "the same clothes")
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := store.GetOutfit(first.ID, false)
	if !kept.Favorite || strings.Join(kept.Tags, "|") != "work|date night" || !merge.Moved.FavoriteSet || strings.Join(merge.Moved.TagsAdded, "|") != "date night" {
		t.Fatalf("the kept outfit should take the favorite and the tags it lacked: %+v %+v", kept, merge.Moved)
	}
	if _, err := store.UndoOutfitMerge(ctx, merge.ID, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	kept, _ = store.GetOutfit(first.ID, false)
	back, _ := store.GetOutfit(second.ID, false)
	if kept.Favorite || strings.Join(kept.Tags, "|") != "work" || !back.Favorite || strings.Join(back.Tags, "|") != "date night|work" {
		t.Fatalf("undo should put both back: kept %+v, back %+v", kept, back)
	}
}
