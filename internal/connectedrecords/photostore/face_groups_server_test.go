package photostore

import (
	"net/http"
	"testing"
)

// threePhotosWithFaces imports three photos and scans one face on each.
func threePhotosWithFaces(t *testing.T, service *testService) {
	t.Helper()
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("a", 100), photoOf("b", 200), photoOf("c", 300)}}, nil)
	for _, photoID := range []string{"photo_000001", "photo_000002", "photo_000003"} {
		service.call(http.MethodPost, "/face-scans", FaceScan{PhotoID: photoID, Model: "test-model", Faces: []ScannedFace{
			{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}, nil)
	}
}

func TestNamingAFaceGroupConfirmsItsFacesAndKeepsARejection(t *testing.T) {
	service := newTestService(t)
	threePhotosWithFaces(t, service)
	// A person said person_000001 is not on the third photo, and a matcher
	// guessed the first face is someone else.
	var link PhotoPerson
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000003", PersonID: "person_000001", Status: "suggested", ProposedBy: "matcher"}, &link)
	rejected, operator := "rejected", "operator"
	service.call(http.MethodPatch, "/photo-people/"+link.ID, PhotoPersonDecision{Status: &rejected, DecidedBy: &operator}, nil)
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000002", FaceID: "face_000001",
		Status: "suggested", ProposedBy: "matcher"}, nil)

	var group FaceGroup
	status, body := service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{
		{FaceID: "face_000001", Similarity: 0.9}, {FaceID: "face_000002", Similarity: 0.8}, {FaceID: "face_000003", Similarity: 0.7}}}, &group)
	if status != http.StatusCreated || group.ID != "face_group_000001" || group.Status != "open" || group.PhotoCount != 3 || len(group.SampleFaceIDs) != 3 {
		t.Fatalf("POST /face-groups = %d %s", status, body)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper",
		Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 0.9}}}, nil); status != http.StatusConflict {
		t.Errorf("a face put in a second group = %d, want 409", status)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/guesses", FaceGroupGuessProposal{PersonID: "person_000001",
		Confidence: 0.8, Evidence: "posted by them", ProposedBy: "a model"}, nil); status != http.StatusCreated {
		t.Errorf("POST guesses = %d, want 201", status)
	}

	var result FaceGroupDecisionResult
	status, body = service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "named", PersonID: "person_000001",
		DecidedBy: "operator", ExcludedFaceIDs: []string{"face_000002"}}, &result)
	if status != http.StatusOK || result.Confirmed != 1 || result.KeptRejected != 1 || result.Excluded != 1 || result.OthersRejected != 1 ||
		result.Group.Status != "named" || result.Group.PersonID != "person_000001" || result.Group.FaceCount != 2 || len(result.Group.Guesses) != 1 {
		t.Fatalf("naming the group = %d %s", status, body)
	}
	var links []PhotoPerson
	service.call(http.MethodGet, "/photo-people?photo_id=photo_000001", nil, &links)
	for _, each := range links {
		want := map[string]string{"person_000001": "confirmed", "person_000002": "rejected"}[each.PersonID]
		if each.Status != want {
			t.Errorf("after naming, %s on photo_000001 is %s, want %s", each.PersonID, each.Status, want)
		}
	}
	service.call(http.MethodGet, "/photo-people?photo_id=photo_000002", nil, &links)
	if len(links) != 0 {
		t.Errorf("the excluded face's photo got links %+v, want none", links)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper",
		Members: []NewFaceGroupMember{{FaceID: "face_000002", Similarity: 0.9}}}, nil); status != http.StatusCreated {
		t.Errorf("the excluded face in a new group = %d, want 201: it left the old one", status)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "not_known", DecidedBy: "operator"}, nil); status != http.StatusConflict {
		t.Errorf("deciding a named group again = %d, want 409", status)
	}
	var open []FaceGroup
	service.call(http.MethodGet, "/face-groups?status=open", nil, &open)
	if len(open) != 1 || open[0].ID != "face_group_000002" {
		t.Errorf("open groups = %+v, want only face_group_000002", open)
	}
}

func TestAFaceGroupRefusesBadDecisionsAndOnlyAnOpenOneTakesFaces(t *testing.T) {
	service := newTestService(t)
	threePhotosWithFaces(t, service)
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, nil)
	for _, bad := range []FaceGroupDecision{
		{Status: "named", DecidedBy: "operator"},
		{Status: "named", PersonID: "Mary", DecidedBy: "operator"},
		{Status: "not_known", PersonID: "person_000001", DecidedBy: "operator"},
		{Status: "open", DecidedBy: "operator"},
		{Status: "mixed"},
		{Status: "mixed", DecidedBy: "operator", ExcludedFaceIDs: []string{"face_000003"}},
	} {
		if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", bad, nil); status != http.StatusBadRequest {
			t.Errorf("decision %+v = %d, want 400", bad, status)
		}
	}
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/members", FaceGroupMembersAddition{Members: []NewFaceGroupMember{{FaceID: "face_000002", Similarity: 0.6}}}, nil); status != http.StatusOK {
		t.Errorf("adding a face to an open group = %d, want 200", status)
	}
	service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "not_known", DecidedBy: "operator"}, nil)
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/members", FaceGroupMembersAddition{Members: []NewFaceGroupMember{{FaceID: "face_000003", Similarity: 0.6}}}, nil); status != http.StatusConflict {
		t.Errorf("adding a face to a decided group = %d, want 409", status)
	}
	var result FaceGroupDecisionResult
	if status, body := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "named", PersonID: "person_000003", DecidedBy: "operator"}, &result); status != http.StatusOK || result.Confirmed != 2 {
		t.Errorf("naming a not_known group later = %d %s, want 2 confirmed", status, body)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "other-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000003", Similarity: 1}}}, nil); status != http.StatusBadRequest {
		t.Errorf("a group of another model's faces = %d, want 400", status)
	}
}

func TestANamedGroupNamedAsSomeoneElseMovesItsFaces(t *testing.T) {
	service := newTestService(t)
	threePhotosWithFaces(t, service)
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{
		{FaceID: "face_000001", Similarity: 1}, {FaceID: "face_000002", Similarity: 1}}}, nil)
	service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "named", PersonID: "person_000002", DecidedBy: "operator"}, nil)

	for _, refused := range []FaceGroupDecision{
		{Status: "named", PersonID: "person_000002", DecidedBy: "operator"},
		{Status: "not_known", DecidedBy: "operator"},
		{Status: "named", PersonID: "person_000001", DecidedBy: "operator", ExcludedFaceIDs: []string{"face_000001"}},
	} {
		if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", refused, nil); status != http.StatusConflict {
			t.Errorf("decision %+v on a named group = %d, want 409", refused, status)
		}
	}
	var result FaceGroupDecisionResult
	status, body := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "named", PersonID: "person_000001", DecidedBy: "operator"}, &result)
	if status != http.StatusOK || result.Confirmed != 2 || result.PreviousPersonRejected != 2 || result.Group.PersonID != "person_000001" {
		t.Fatalf("renaming the group = %d %s", status, body)
	}
	var links []PhotoPerson
	service.call(http.MethodGet, "/photo-people?photo_id=photo_000001", nil, &links)
	for _, link := range links {
		want := map[string]string{"person_000001": "confirmed", "person_000002": "rejected"}[link.PersonID]
		if link.Status != want {
			t.Errorf("%s on photo_000001 is %s, want %s", link.PersonID, link.Status, want)
		}
	}
}
