package peoplematching

import (
	"math"
	"testing"
)

// unit is a normalized two-number embedding at an angle, so similarities are
// easy to reason about: cos(angle difference).
func unit(id, photoID string, degrees float64) Face {
	radians := degrees * math.Pi / 180
	return Face{ID: id, PhotoID: photoID, Embedding: []float32{float32(math.Cos(radians)), float32(math.Sin(radians))}}
}

func TestAFaceIsConfirmedOnlyWhenTheContextAgrees(t *testing.T) {
	references := map[string][]Face{
		"person_000001": {unit("face_1", "photo_1", 0), unit("face_2", "photo_2", 5)},
		"person_000002": {unit("face_3", "photo_3", 90)},
	}
	face := unit("face_9", "photo_9", 10)
	scores := ScorePeople(face, references)
	if scores[0].PersonID != "person_000001" {
		t.Fatalf("best = %+v, want person_000001", scores[0])
	}
	if verdict := Decide(scores, map[string]bool{"person_000001": true}); verdict.Status != "confirmed" {
		t.Errorf("a close face named by the context = %+v, want confirmed", verdict)
	}
	if verdict := Decide(scores, map[string]bool{}); verdict.Status != "suggested" {
		t.Errorf("a close face with no context = %+v, want only suggested", verdict)
	}
	if verdict := Decide(ScorePeople(unit("face_8", "photo_8", 225), references), map[string]bool{"person_000001": true}); verdict.Status != "" {
		t.Errorf("a face far from everyone = %+v, want nothing", verdict)
	}
}

func TestTwoPeopleTooAlikeAreOnlySuggested(t *testing.T) {
	references := map[string][]Face{
		"person_000001": {unit("face_1", "photo_1", 0)},
		"person_000002": {unit("face_2", "photo_2", 4)},
	}
	verdict := Decide(ScorePeople(unit("face_9", "photo_9", 2), references), map[string]bool{"person_000001": true, "person_000002": true})
	if verdict.Status != "" {
		t.Errorf("a face as close to two people = %+v, want nothing: neither leads by the margin", verdict)
	}
}

func TestTheFaceThatRecursAcrossAChatIsItsCluster(t *testing.T) {
	faces := []Face{
		unit("face_1", "photo_1", 0), unit("face_2", "photo_1", 120),
		unit("face_3", "photo_2", 4), unit("face_4", "photo_3", 6),
		unit("face_5", "photo_4", 200), unit("face_6", "photo_4", 2),
	}
	cluster := ClusterInContext(faces)
	if len(cluster) != 4 {
		t.Fatalf("cluster = %v, want the four faces near 0 degrees, one per photo", cluster)
	}
	if ClusterInContext(faces[:3]) != nil {
		t.Error("a face on only two photos was answered as a cluster")
	}
}

func TestSeedFacesPicksThePersonOnEachPhoto(t *testing.T) {
	seeds := SeedFaces(map[string][]Face{
		"photo_1": {unit("face_1", "photo_1", 0), unit("face_2", "photo_1", 100)},
		"photo_2": {unit("face_3", "photo_2", 170), unit("face_4", "photo_2", 3)},
		"photo_3": {unit("face_5", "photo_3", 5)},
	})
	var ids []string
	for _, seed := range seeds {
		ids = append(ids, seed.ID)
	}
	if len(ids) != 3 || ids[0] != "face_1" || ids[1] != "face_4" || ids[2] != "face_5" {
		t.Errorf("seeds = %v, want face_1, face_4, face_5", ids)
	}
}
