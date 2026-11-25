package facegrouping

import (
	"fmt"
	"math"
	"testing"

	"github.com/kayushkin/photo-store/internal/peoplematching"
)

// near is a normalized embedding close to axis, tilted a little by wobble
// along the next axis.
func near(axis int, wobble float64) []float32 {
	values := make([]float32, 4)
	values[axis] = float32(math.Cos(wobble))
	values[(axis+1)%4] = float32(math.Sin(wobble))
	return values
}

func faceOn(photo string, axis int, wobble float64) Face {
	return Face{Face: peoplematching.Face{ID: "face_" + photo + fmt.Sprint(axis), PhotoID: photo, Embedding: near(axis, wobble)}, DetectionScore: 0.9}
}

func TestTwoPeopleMakeTwoGroupsAndALoneFaceWaits(t *testing.T) {
	free := []Face{
		faceOn("p1", 0, 0.1), faceOn("p2", 0, -0.1), faceOn("p3", 0, 0.2),
		faceOn("p1", 2, 0.1), faceOn("p4", 2, 0), faceOn("p5", 2, -0.2),
		faceOn("p6", 3, 0),
	}
	result := Assign(free, nil, DefaultSettings)
	if len(result.Created) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(result.Created), result.Created)
	}
	for _, group := range result.Created {
		axis := -1
		for _, member := range group.Members {
			got := 0
			for index, value := range member.Face.Embedding {
				if value > 0.9 {
					got = index
				}
			}
			if axis >= 0 && got != axis {
				t.Errorf("a group mixes two people: %+v", group)
			}
			axis = got
		}
	}
}

func TestTwoFacesOnOnePhotoNeverShareAGroup(t *testing.T) {
	// Twins: the same face twice on each photo.
	free := []Face{faceOn("p1", 0, 0), faceOn("p1", 0, 0.05), faceOn("p2", 0, 0), faceOn("p2", 0, 0.05), faceOn("p3", 0, 0), faceOn("p3", 0, 0.05)}
	free[1].ID, free[3].ID, free[5].ID = "twin1", "twin2", "twin3"
	for _, group := range Assign(free, nil, DefaultSettings).Created {
		photos := map[string]bool{}
		for _, member := range group.Members {
			if photos[member.Face.PhotoID] {
				t.Errorf("a group holds two faces of %s: %+v", member.Face.PhotoID, group)
			}
			photos[member.Face.PhotoID] = true
		}
	}
}

func TestAFaceJoinsTheOpenGroupItClearlyBelongsTo(t *testing.T) {
	open := []Group{
		{ID: "face_group_000001", Members: []Member{{Face: faceOn("p1", 0, 0)}, {Face: faceOn("p2", 0, 0.1)}}},
		{ID: "face_group_000002", Members: []Member{{Face: faceOn("p3", 2, 0)}}},
	}
	result := Assign([]Face{faceOn("p4", 0, 0.05), faceOn("p1", 0, 0.02), faceOn("p5", 1, 0)}, open, DefaultSettings)
	if joined := result.Joined["face_group_000001"]; len(joined) != 1 || joined[0].Face.PhotoID != "p4" {
		t.Errorf("joined = %+v, want only p4's face in face_group_000001: p1 is already in it", result.Joined)
	}
	if len(result.Joined["face_group_000002"]) != 0 || len(result.Created) != 0 {
		t.Errorf("result = %+v, want nothing else: the other faces are alone", result)
	}
}

func TestTwoGroupsOfOnePersonMergeUnlessTheyShareAPhoto(t *testing.T) {
	// A strict join splits one person into two clusters a merge brings back.
	settings := Settings{JoinSimilarity: 0.97, MinimumMargin: 0.05, MinimumPhotos: 3, MergeSimilarity: 0.5}
	free := []Face{faceOn("p1", 0, 0), faceOn("p2", 0, 0.01), faceOn("p3", 0, 0.5), faceOn("p4", 0, 0.51)}
	result := Assign(free, nil, settings)
	if len(result.Created) != 1 || len(result.Created[0].Members) != 4 {
		t.Fatalf("groups = %+v, want one group of four faces", result.Created)
	}

	// The same, but a face of each cluster on one photo: two people.
	free = []Face{faceOn("p1", 0, 0), faceOn("p2", 0, 0.01), faceOn("p5", 0, 0.02), faceOn("p1", 0, 0.5), faceOn("p3", 0, 0.51), faceOn("p4", 0, 0.52)}
	free[3].ID = "other-face-on-p1"
	if result := Assign(free, nil, settings); len(result.Created) != 2 {
		t.Errorf("groups = %d, want 2: a face of each on one photo means two people", len(result.Created))
	}

	// A new cluster merges into an open group close to it.
	open := []Group{{ID: "face_group_000001", Members: []Member{{Face: faceOn("p1", 0, 0)}, {Face: faceOn("p2", 0, 0.01)}}}}
	result = Assign([]Face{faceOn("p3", 0, 0.5), faceOn("p4", 0, 0.51)}, open, settings)
	if len(result.Joined["face_group_000001"]) != 2 || len(result.Created) != 0 {
		t.Errorf("result = %+v, want both faces merged into face_group_000001", result)
	}
}
