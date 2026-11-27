package photos

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// twoOwnersWithAPhotoEach gives Alice and Bob one scanned photo each, of
// different originals, and answers their ids: Alice's photo_000001 with
// face_000001, Bob's photo_000002 with face_000002.
func twoOwnersWithAPhotoEach(t *testing.T, service *testService) {
	t.Helper()
	service.people.keep(bobPrincipalID, "person_000101")
	for _, each := range []struct {
		caller ownership.Caller
		hash   string
		photo  string
	}{{alice, "alice-photo", "photo_000001"}, {bob, "bob-photo", "photo_000002"}} {
		var result ImportResult
		if status, body := service.as(each.caller).call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf(each.hash, 100)}}, &result); status != http.StatusOK || result.IDs[0] != each.photo {
			t.Fatalf("import as %s = %d %s", each.caller.PrincipalID, status, body)
		}
		if status, body := service.as(each.caller).call(http.MethodPost, "/face-scans", FaceScan{PhotoID: each.photo, Model: "test-model", Faces: []ScannedFace{
			{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}, nil); status != http.StatusCreated {
			t.Fatalf("scan as %s = %d %s", each.caller.PrincipalID, status, body)
		}
	}
}

func TestEveryRecordCarriesItsOwner(t *testing.T) {
	service := newTestService(t)
	twoOwnersWithAPhotoEach(t, service)
	var photo Photo
	service.call(http.MethodGet, "/photos/photo_000001", nil, &photo)
	var faces []Face
	service.call(http.MethodGet, "/faces", nil, &faces)
	var link PhotoPerson
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001", FaceID: "face_000001",
		Status: "suggested", ProposedBy: "matcher"}, &link)
	var group FaceGroup
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, &group)
	var guess FaceGroupGuess
	service.call(http.MethodPost, "/face-groups/"+group.ID+"/guesses", FaceGroupGuessProposal{PersonID: "person_000001", Confidence: 0.5, ProposedBy: "a model"}, &guess)
	service.call(http.MethodPatch, "/photos/photo_000001", map[string]any{"tags": []string{"beach"}}, nil)
	var revisions []Revision
	service.call(http.MethodGet, "/photos/photo_000001/revisions", nil, &revisions)
	if photo.OwnerPrincipalID != alicePrincipalID || len(faces) != 1 || faces[0].OwnerPrincipalID != alicePrincipalID ||
		link.OwnerPrincipalID != alicePrincipalID || group.OwnerPrincipalID != alicePrincipalID || guess.OwnerPrincipalID != alicePrincipalID ||
		len(revisions) != 1 || revisions[0].Photo.OwnerPrincipalID != alicePrincipalID {
		t.Fatalf("owners: photo=%q faces=%d link=%q group=%q guess=%q revisions=%d, want %s on each",
			photo.OwnerPrincipalID, len(faces), link.OwnerPrincipalID, group.OwnerPrincipalID, guess.OwnerPrincipalID, len(revisions), alicePrincipalID)
	}
}

func TestANonOwnerReachesNoneOfAnotherOwnersRecords(t *testing.T) {
	service := newTestService(t)
	twoOwnersWithAPhotoEach(t, service)
	var link PhotoPerson
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001", FaceID: "face_000001",
		Status: "suggested", ProposedBy: "matcher"}, &link)
	var group FaceGroup
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, &group)
	confirmed, operator := "confirmed", "operator"

	asBob := service.as(bob)
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/photos/photo_000001", nil},
		{http.MethodGet, "/photos/photo_000001?include_deleted=true", nil},
		{http.MethodPatch, "/photos/photo_000001", map[string]any{"description": "x", "described_by": "bob"}},
		{http.MethodDelete, "/photos/photo_000001", nil},
		{http.MethodPost, "/photos/photo_000001/restore", nil},
		{http.MethodPost, "/photos/photo_000001/purge", nil},
		{http.MethodGet, "/photos/photo_000001/revisions", nil},
		{http.MethodGet, "/photos/photo_000001/thumbnail", nil},
		{http.MethodGet, "/faces/face_000001/image", nil},
		{http.MethodPost, "/face-scans", FaceScan{PhotoID: "photo_000001", Model: "other-model"}},
		{http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000101", Status: "suggested", ProposedBy: "bob"}},
		{http.MethodPatch, "/photo-people/" + link.ID, PhotoPersonDecision{Status: &confirmed, DecidedBy: &operator}},
		{http.MethodGet, "/face-groups/" + group.ID, nil},
		{http.MethodPost, "/face-groups/" + group.ID + "/members", FaceGroupMembersAddition{Members: []NewFaceGroupMember{{FaceID: "face_000002", Similarity: 1}}}},
		{http.MethodPost, "/face-groups/" + group.ID + "/guesses", FaceGroupGuessProposal{Confidence: 0.5, ProposedBy: "bob"}},
		{http.MethodPost, "/face-groups/" + group.ID + "/decision", FaceGroupDecision{Status: "mixed", DecidedBy: "bob"}},
	} {
		if status, body := asBob.call(request.method, request.path, request.body, nil); status != http.StatusNotFound {
			t.Errorf("%s %s as Bob = %d %s, want 404 as for a missing record", request.method, request.path, status, body)
		}
	}
	// Alice's records are as they were.
	var photo Photo
	if status, _ := service.call(http.MethodGet, "/photos/photo_000001", nil, &photo); status != http.StatusOK || photo.Description != "" || photo.DeletedAt != 0 {
		t.Errorf("Alice's photo after Bob's tries = %d %+v", status, photo)
	}
	// A face of Alice's in Bob's group, or Alice's face named on Bob's photo,
	// is a face Bob cannot reach.
	if status, _ := asBob.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "bob",
		Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, nil); status != http.StatusBadRequest {
		t.Errorf("Bob grouping Alice's face = %d, want 400 as for a missing face", status)
	}
	if status, _ := asBob.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000002", PersonID: "person_000101",
		FaceID: "face_000001", Status: "suggested", ProposedBy: "bob"}, nil); status != http.StatusBadRequest {
		t.Errorf("Bob naming Alice's face on his photo = %d, want 400", status)
	}
}

func TestListsHoldOnlyTheCallersOwnAndAnAdministratorSeesAndNarrowsEveryOwner(t *testing.T) {
	service := newTestService(t)
	twoOwnersWithAPhotoEach(t, service)
	service.call(http.MethodPatch, "/photos/photo_000001", map[string]any{"tags": []string{"beach"}}, nil)
	service.as(bob).call(http.MethodPatch, "/photos/photo_000002", map[string]any{"tags": []string{"snow"}}, nil)
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001", Status: "suggested", ProposedBy: "matcher"}, nil)
	service.as(bob).call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000002", PersonID: "person_000101", Status: "suggested", ProposedBy: "matcher"}, nil)
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, nil)
	service.as(bob).call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000002", Similarity: 1}}}, nil)

	// listed names each item of a list answer by its id, or what stands for
	// one: a tag, a person, a sighting's photo, a span's sample photos.
	listed := func(caller *testService, path string) string {
		t.Helper()
		var answer []any
		if status, body := caller.call(http.MethodGet, path, nil, &answer); status != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
		keys := []string{}
		for _, item := range answer {
			switch typed := item.(type) {
			case string:
				keys = append(keys, typed)
			case map[string]any:
				for _, key := range []string{"id", "tag", "person_id", "photo_id", "sample_photo_ids"} {
					if value, ok := typed[key]; ok {
						keys = append(keys, fmt.Sprint(value))
						break
					}
				}
			}
		}
		return strings.Join(keys, ",")
	}
	for _, check := range []struct {
		path, alice, bob, administrator, administratorNarrowedToBob string
	}{
		{"/photos", "photo_000001", "photo_000002", "photo_000001,photo_000002", "photo_000002"},
		{"/faces", "face_000001", "face_000002", "face_000001,face_000002", "face_000002"},
		{"/photo-people", "photo_person_000001", "photo_person_000002", "photo_person_000001,photo_person_000002", "photo_person_000002"},
		{"/people-counts", "person_000001", "person_000101", "person_000001,person_000101", "person_000101"},
		{"/tags", "beach", "snow", "beach,snow", "snow"},
		{"/sightings", "photo_000001", "photo_000002", "photo_000001,photo_000002", "photo_000002"},
		{"/face-groups", "face_group_000001", "face_group_000002", "face_group_000001,face_group_000002", "face_group_000002"},
		{"/taken-activity?interval=year&time_zone=UTC&samples=5", "[photo_000001]", "[photo_000002]", "[photo_000001 photo_000002]", "[photo_000002]"},
		{"/face-scans/pending?model=other-model", "photo_000001", "photo_000002", "photo_000001,photo_000002", "photo_000002"},
	} {
		separator := "?"
		if strings.Contains(check.path, "?") {
			separator = "&"
		}
		for label, got := range map[string][2]string{
			"Alice":                     {listed(service, check.path), check.alice},
			"Bob":                       {listed(service.as(bob), check.path), check.bob},
			"an administrator":          {listed(service.as(administrator), check.path), check.administrator},
			"an administrator narrowed": {listed(service.as(administrator), check.path+separator+"owner_principal_id="+bobPrincipalID), check.administratorNarrowedToBob},
			"Alice naming herself":      {listed(service, check.path+separator+"owner_principal_id="+alicePrincipalID), check.alice},
		} {
			if got[0] != got[1] {
				t.Errorf("GET %s as %s = %q, want %q", check.path, label, got[0], got[1])
			}
		}
	}
	if status, _ := service.call(http.MethodGet, "/photos?owner_principal_id="+bobPrincipalID, nil, nil); status != http.StatusForbidden {
		t.Errorf("Alice naming Bob as owner = %d, want 403", status)
	}
	var health Health
	service.call(http.MethodGet, "/health", nil, &health)
	var everyone Health
	service.as(administrator).call(http.MethodGet, "/health", nil, &everyone)
	if health.Counts.Photos != 1 || health.Counts.Revisions != 1 || everyone.Counts.Photos != 2 || everyone.Counts.Revisions != 2 {
		t.Errorf("health counts: Alice %+v, administrator %+v; want 1 photo and 1 revision for Alice, 2 and 2 for every owner", health.Counts, everyone.Counts)
	}
	// An administrator reads any one owner's record by its id.
	if status, _ := service.as(administrator).call(http.MethodGet, "/photos/photo_000002", nil, nil); status != http.StatusOK {
		t.Errorf("an administrator reading Bob's photo = %d, want 200", status)
	}
}

func TestTwoOwnersKeepTheSameOriginalApartAndOnesPurgeLeavesTheOthersThumbnail(t *testing.T) {
	service := newTestService(t)
	photo := photoOf("shared", 100)
	var aliceResult, bobResult ImportResult
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &aliceResult)
	service.as(bob).call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &bobResult)
	if aliceResult.Created != 1 || bobResult.Created != 1 || aliceResult.IDs[0] == bobResult.IDs[0] {
		t.Fatalf("Alice's import %+v and Bob's %+v: want each to create a photo of their own", aliceResult, bobResult)
	}
	if len(service.thumbnails.filesOf(alicePrincipalID, aliceResult.IDs[0])) != 1 || len(service.thumbnails.filesOf(bobPrincipalID, bobResult.IDs[0])) != 1 {
		t.Fatalf("want one thumbnail file each, owned by each owner")
	}
	var bobsBySha []Photo
	service.as(bob).call(http.MethodGet, "/photos?sha256="+photo.Sha256, nil, &bobsBySha)
	var everyBySha []Photo
	service.as(administrator).call(http.MethodGet, "/photos?sha256="+photo.Sha256, nil, &everyBySha)
	if len(bobsBySha) != 1 || bobsBySha[0].ID != bobResult.IDs[0] || len(everyBySha) != 2 {
		t.Errorf("by sha256: Bob sees %d, an administrator %d; want Bob's own one, and both", len(bobsBySha), len(everyBySha))
	}

	if status, body := service.as(bob).call(http.MethodPost, "/photos/"+bobResult.IDs[0]+"/purge", nil, nil); status != http.StatusOK {
		t.Fatalf("Bob's purge = %d %s", status, body)
	}
	if status, body := service.call(http.MethodGet, "/photos/"+aliceResult.IDs[0]+"/thumbnail", nil, nil); status != http.StatusOK || !bytes.Equal(body, jpeg) {
		t.Errorf("Alice's thumbnail after Bob purged his copy = %d, %d bytes; want it served", status, len(body))
	}
	if len(service.thumbnails.filesOf(bobPrincipalID, bobResult.IDs[0])) != 0 {
		t.Errorf("Bob's thumbnail file is still kept after his purge")
	}
	// Bob's purge makes his own later imports skip the original, not Alice's.
	service.as(bob).call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &bobResult)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &aliceResult)
	if bobResult.Skipped != 1 || aliceResult.Skipped != 1 || aliceResult.Created != 0 {
		t.Errorf("re-imports: Bob %+v, Alice %+v; want each to find their own photo", bobResult, aliceResult)
	}
}

func TestAJoinAcrossOwnersIsRefused(t *testing.T) {
	service := newTestService(t)
	twoOwnersWithAPhotoEach(t, service)
	asAdministrator := service.as(administrator)
	status, body := asAdministrator.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper",
		Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}, {FaceID: "face_000002", Similarity: 1}}}, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "one owner") {
		t.Errorf("an administrator grouping two owners' faces = %d %s, want 400", status, body)
	}
	var group FaceGroup
	if status, body := asAdministrator.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper",
		Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, &group); status != http.StatusCreated || group.OwnerPrincipalID != alicePrincipalID {
		t.Fatalf("an administrator grouping Alice's face = %d %s, want a group of Alice's", status, body)
	}
	if status, _ := asAdministrator.call(http.MethodPost, "/face-groups/"+group.ID+"/members", FaceGroupMembersAddition{
		Members: []NewFaceGroupMember{{FaceID: "face_000002", Similarity: 1}}}, nil); status != http.StatusBadRequest {
		t.Errorf("an administrator adding Bob's face to Alice's group = %d, want 400", status)
	}
	if status, _ := asAdministrator.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001",
		FaceID: "face_000002", Status: "suggested", ProposedBy: "matcher"}, nil); status != http.StatusBadRequest {
		t.Errorf("an administrator naming Bob's face on Alice's photo = %d, want 400", status)
	}
	// The person must be one the photo's owner keeps, whoever asks.
	if status, _ := asAdministrator.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000101",
		Status: "suggested", ProposedBy: "matcher"}, nil); status != http.StatusBadRequest {
		t.Errorf("an administrator linking Bob's person on Alice's photo = %d, want 400", status)
	}
}

func TestAPersonMustBeOneThePhotosOwnerKeeps(t *testing.T) {
	service := newTestService(t)
	threePhotosWithFaces(t, service)
	if status, body := service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000042",
		Status: "suggested", ProposedBy: "matcher"}, nil); status != http.StatusBadRequest || !strings.Contains(string(body), "person_000042") {
		t.Errorf("a person Alice does not keep = %d %s, want 400 naming it", status, body)
	}
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000001", Similarity: 1}}}, nil)
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/guesses", FaceGroupGuessProposal{PersonID: "person_000042",
		Confidence: 0.5, ProposedBy: "a model"}, nil); status != http.StatusBadRequest {
		t.Errorf("a guess naming a person Alice does not keep = %d, want 400", status)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/guesses", FaceGroupGuessProposal{PersonID: "",
		Confidence: 0.5, ProposedBy: "a model"}, nil); status != http.StatusCreated {
		t.Errorf("a guess of nobody in people-store = %d, want 201", status)
	}
	if status, _ := service.call(http.MethodPost, "/face-groups/face_group_000001/decision", FaceGroupDecision{Status: "named",
		PersonID: "person_000042", DecidedBy: "operator"}, nil); status != http.StatusBadRequest {
		t.Errorf("naming a group as a person Alice does not keep = %d, want 400", status)
	}
	service.people.down = true
	if status, _ := service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001",
		Status: "suggested", ProposedBy: "matcher"}, nil); status != http.StatusBadGateway {
		t.Errorf("a proposal while people-store is down = %d, want 502", status)
	}
	var links []PhotoPerson
	service.people.down = false
	service.call(http.MethodGet, "/photo-people", nil, &links)
	if len(links) != 0 {
		t.Errorf("refused proposals wrote %d links", len(links))
	}
}

func TestARequestWithNoCallerIsRefusedAndAVocabularyAnswersAnyCaller(t *testing.T) {
	service := newTestService(t)
	for _, path := range []string{"/health", "/photos", "/date-sources", "/face-group-statuses", "/photo-person-statuses"} {
		if status, _ := service.withoutCaller().call(http.MethodGet, path, nil, nil); status != http.StatusUnauthorized {
			t.Errorf("GET %s with no caller = %d, want 401", path, status)
		}
	}
	nobody := service.as(ownership.Caller{SeesEveryOwner: true})
	for _, path := range []string{"/date-sources", "/face-group-statuses", "/photo-person-statuses"} {
		if status, _ := nobody.call(http.MethodGet, path, nil, nil); status != http.StatusOK {
			t.Errorf("GET %s as the server acting as nobody = %d, want 200", path, status)
		}
	}
	if status, _ := nobody.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("a", 1)}}, nil); status != http.StatusForbidden {
		t.Errorf("an import acting as nobody = %d, want 403: a new photo needs an owner", status)
	}
	if status, _ := nobody.call(http.MethodGet, "/photos", nil, nil); status != http.StatusOK {
		t.Errorf("GET /photos as the server acting as nobody = %d, want 200 over every owner", status)
	}
}
