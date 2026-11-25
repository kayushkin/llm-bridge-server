package facepasses

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	photostore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore"
)

// countingTransport counts every request a pass sends through its client.
type countingTransport struct {
	requests atomic.Int64
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return http.DefaultTransport.RoundTrip(request)
}

// fakeServices plays every service a pass calls, each on its own server,
// and counts the requests they receive, so a test can tell whether all of
// them came through the pass's client.
type fakeServices struct {
	t        *testing.T
	received atomic.Int64
	mutex    sync.Mutex
	writes   []recordedWrite
	servers  []*httptest.Server
}

type recordedWrite struct {
	method, path string
	body         []byte
}

func newFakeServices(t *testing.T) *fakeServices {
	return &fakeServices{t: t}
}

// serve starts one fake service and answers its base URL. A request the
// handler does not answer fails the test.
func (f *fakeServices) serve(routes map[string]http.HandlerFunc) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.received.Add(1)
		if r.Method != http.MethodGet {
			body, _ := io.ReadAll(r.Body)
			f.mutex.Lock()
			f.writes = append(f.writes, recordedWrite{r.Method, r.URL.Path, body})
			f.mutex.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler, known := routes[r.Method+" "+r.URL.Path]
		if !known {
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "not faked", http.StatusNotImplemented)
			return
		}
		handler(w, r)
	}))
	f.servers = append(f.servers, server)
	f.t.Cleanup(server.Close)
	return server.URL
}

func (f *fakeServices) writesTo(method, path string) []recordedWrite {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var found []recordedWrite
	for _, write := range f.writes {
		if write.method == method && write.path == path {
			found = append(found, write)
		}
	}
	return found
}

func answerJSON(value any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			panic(err)
		}
	}
}

func answerBytes(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.Write(data) }
}

// embedding is a normalized embedding near axis, tilted by wobble toward
// the next axis, base64 little-endian float32s as photo-store sends them.
func embedding(axis int, wobble float64) string {
	values := make([]float32, 8)
	values[axis] = float32(math.Cos(wobble))
	values[(axis+1)%len(values)] = float32(math.Sin(wobble))
	raw := make([]byte, 4*len(values))
	for index, value := range values {
		binary.LittleEndian.PutUint32(raw[index*4:], math.Float32bits(value))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func face(id, photoID string, axis int, wobble float64) photostore.Face {
	return photostore.Face{ID: id, PhotoID: photoID, Model: DefaultFaceModel, DetectionScore: 0.9,
		Box: photostore.FaceBox{Left: 0.3, Top: 0.3, Right: 0.6, Bottom: 0.6}, EmbeddingFloat32Base64: embedding(axis, wobble)}
}

func testJPEG(t *testing.T) []byte {
	picture := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			picture.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 128, 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// checkEveryRequestWentThroughTheClient compares what the client sent with
// what the fake services received.
func checkEveryRequestWentThroughTheClient(t *testing.T, transport *countingTransport, services *fakeServices) {
	t.Helper()
	sent, received := transport.requests.Load(), services.received.Load()
	if sent == 0 || sent != received {
		t.Fatalf("the client sent %d requests and the services received %d; every request must go through HTTPClient", sent, received)
	}
}

func TestGroupFacesPostsANewGroupAndGuessesItFromADecidedOne(t *testing.T) {
	services := newFakeServices(t)
	faces := []photostore.Face{
		// person_000001's decided group.
		face("face_000001", "photo_000001", 0, 0.05), face("face_000002", "photo_000002", 0, -0.05), face("face_000003", "photo_000003", 0, 0.1),
		// Three free faces of the same look, on three other photos.
		face("face_000004", "photo_000004", 0, 0.08), face("face_000005", "photo_000005", 0, -0.08), face("face_000006", "photo_000006", 0, 0.02),
	}
	named := photostore.FaceGroup{ID: "face_group_000001", Model: DefaultFaceModel, Status: photostore.FaceGroupNamed, PersonID: "person_000001"}
	for _, each := range faces[:3] {
		named.Members = append(named.Members, photostore.FaceGroupMember{FaceID: each.ID, PhotoID: each.PhotoID, Similarity: 0.9})
	}
	var groupsMutex sync.Mutex
	groups := []photostore.FaceGroup{named}
	routes := map[string]http.HandlerFunc{
		"GET /faces":        answerJSON(faces),
		"GET /photo-people": answerJSON([]photostore.PhotoPerson{}),
		"GET /face-groups": func(w http.ResponseWriter, r *http.Request) {
			groupsMutex.Lock()
			defer groupsMutex.Unlock()
			listed := []photostore.FaceGroup{}
			for _, group := range groups {
				group.Members = nil
				listed = append(listed, group)
			}
			answerJSON(listed)(w, r)
		},
		"POST /face-groups": func(w http.ResponseWriter, r *http.Request) {
			var body photostore.NewFaceGroup
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			groupsMutex.Lock()
			created := photostore.FaceGroup{ID: "face_group_000002", Model: body.Model, Status: photostore.FaceGroupOpen, MadeBy: body.MadeBy, UpdatedAt: 100}
			for _, member := range body.Members {
				created.Members = append(created.Members, photostore.FaceGroupMember{FaceID: member.FaceID, Similarity: member.Similarity})
			}
			groups = append(groups, created)
			groupsMutex.Unlock()
			w.WriteHeader(http.StatusCreated)
			answerJSON(created)(w, r)
		},
		"POST /face-groups/face_group_000002/guesses": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		},
	}
	for _, id := range []string{"face_group_000001", "face_group_000002"} {
		id := id
		routes["GET /face-groups/"+id] = func(w http.ResponseWriter, r *http.Request) {
			groupsMutex.Lock()
			defer groupsMutex.Unlock()
			for _, group := range groups {
				if group.ID == id {
					answerJSON(group)(w, r)
					return
				}
			}
			http.NotFound(w, r)
		}
	}
	storeURL := services.serve(routes)

	transport := &countingTransport{}
	var output, diagnostics bytes.Buffer
	options := DefaultGroupFacesOptions()
	options.PhotoStoreURL = storeURL + "/"
	options.HTTPClient = &http.Client{Transport: transport}
	options.Output, options.DiagnosticOutput = &output, &diagnostics
	if err := GroupFaces(context.Background(), options); err != nil {
		t.Fatalf("GroupFaces: %v\noutput:\n%s", err, output.String())
	}

	created := services.writesTo(http.MethodPost, "/face-groups")
	if len(created) != 1 {
		t.Fatalf("POST /face-groups %d times, want once", len(created))
	}
	var body photostore.NewFaceGroup
	if err := json.Unmarshal(created[0].body, &body); err != nil {
		t.Fatal(err)
	}
	memberIDs := []string{}
	for _, member := range body.Members {
		memberIDs = append(memberIDs, member.FaceID)
	}
	sort.Strings(memberIDs)
	if body.MadeBy != grouperName || body.Model != DefaultFaceModel || strings.Join(memberIDs, ",") != "face_000004,face_000005,face_000006" {
		t.Fatalf("new group = %+v, want the three free faces made by %s", body, grouperName)
	}
	guesses := services.writesTo(http.MethodPost, "/face-groups/face_group_000002/guesses")
	if len(guesses) != 1 {
		t.Fatalf("POST guesses %d times, want once", len(guesses))
	}
	var guess photostore.FaceGroupGuessProposal
	if err := json.Unmarshal(guesses[0].body, &guess); err != nil {
		t.Fatal(err)
	}
	if guess.PersonID != "person_000001" || guess.ProposedBy != grouperName {
		t.Fatalf("guess = %+v, want person_000001 proposed by %s", guess, grouperName)
	}
	wantOutput := "6 faces; 3 free to group; wrote 0 faces into 0 open groups and 1 new groups of 3 faces\nguessed 1 open groups from the groups already decided\n"
	if output.String() != wantOutput {
		t.Fatalf("output = %q, want %q", output.String(), wantOutput)
	}
	checkEveryRequestWentThroughTheClient(t, transport, services)
}

func TestMatchPeopleWithModelCheckPatchesTheModelsVerdict(t *testing.T) {
	services := newFakeServices(t)
	thumbnail := testJPEG(t)
	faces := []photostore.Face{
		face("face_000001", "photo_000001", 0, 0.05), face("face_000002", "photo_000002", 0, -0.05), face("face_000003", "photo_000003", 0, 0.1),
		face("face_000004", "photo_000004", 0, 0.08),
	}
	photos := []photostore.Photo{{ID: "photo_000001"}, {ID: "photo_000002"}, {ID: "photo_000003"},
		// The fourth came in a chat, so its context is read from message-store.
		{ID: "photo_000004", SourcePath: "chat_message_000001", TakenAt: 1262304000}}
	var linksMutex sync.Mutex
	links := []photostore.PhotoPerson{}
	for index, each := range faces[:3] {
		links = append(links, photostore.PhotoPerson{ID: fmt.Sprintf("photo_person_%06d", index+1), PhotoID: each.PhotoID, PersonID: "person_000001",
			FaceID: each.ID, Status: photostore.PhotoPersonConfirmed, DecidedBy: "a person"})
	}
	photoRoutes := map[string]http.HandlerFunc{
		"GET /faces": answerJSON(faces),
		"GET /photo-people": func(w http.ResponseWriter, r *http.Request) {
			linksMutex.Lock()
			defer linksMutex.Unlock()
			answerJSON(links)(w, r)
		},
		"GET /photos": answerJSON(photos),
		"POST /photo-people": func(w http.ResponseWriter, r *http.Request) {
			var proposal photostore.PhotoPersonProposal
			if err := json.NewDecoder(r.Body).Decode(&proposal); err != nil {
				t.Error(err)
			}
			linksMutex.Lock()
			links = append(links, photostore.PhotoPerson{ID: fmt.Sprintf("photo_person_%06d", len(links)+1), PhotoID: proposal.PhotoID, PersonID: proposal.PersonID,
				FaceID: proposal.FaceID, Status: proposal.Status, Confidence: proposal.Confidence, Evidence: proposal.Evidence, ProposedBy: proposal.ProposedBy})
			linksMutex.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		},
		"PATCH /photo-people/photo_person_000004": answerJSON(map[string]string{}),
	}
	for _, photo := range photos {
		photoRoutes["GET /photos/"+photo.ID] = answerJSON(photo)
		photoRoutes["GET /photos/"+photo.ID+"/thumbnail"] = answerBytes(thumbnail)
	}
	storeURL := services.serve(photoRoutes)
	multichatURL := services.serve(map[string]http.HandlerFunc{
		"GET /api/conversations": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-multichat-token" {
				t.Errorf("multichat Authorization = %q", r.Header.Get("Authorization"))
			}
			answerJSON([]any{})(w, r)
		},
	})
	messageStoreURL := services.serve(map[string]http.HandlerFunc{
		"GET /messages/chat_message_000001":      answerJSON(map[string]string{"id": "chat_message_000001", "conversation_id": "conversation_000001"}),
		"GET /conversations/conversation_000001": answerJSON(map[string]any{"id": "conversation_000001", "title": "Test chat", "participants": []any{}}),
	})
	bridgeURL := services.serve(map[string]http.HandlerFunc{
		"POST /oneshot": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-LLM-Bridge-Service-Token") != "test-service-token" {
				t.Errorf("oneshot token = %q", r.Header.Get("X-LLM-Bridge-Service-Token"))
			}
			var request struct {
				ModelRole string `json:"model_role"`
				Images    []any  `json:"images"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.ModelRole != "test-role" || len(request.Images) != 4 {
				t.Errorf("oneshot asked role %q with %d images, want test-role with the face and three known faces", request.ModelRole, len(request.Images))
			}
			answerJSON(map[string]any{"model": "test-model", "parsed": map[string]string{"verdict": "same", "reason": "Same eyes and nose."}})(w, r)
		},
	})

	transport := &countingTransport{}
	var output, diagnostics bytes.Buffer
	options := DefaultMatchPeopleOptions()
	options.Operator = "person_000009"
	options.PhotoStoreURL = storeURL
	options.ModelCheck = 5
	options.ModelCheckRole = "test-role"
	options.MultichatURL, options.MultichatToken = multichatURL, "test-multichat-token"
	options.MessageStoreURL = messageStoreURL
	options.BridgeURL, options.BridgeServiceToken = bridgeURL, "test-service-token"
	options.HTTPClient = &http.Client{Transport: transport}
	options.Output, options.DiagnosticOutput = &output, &diagnostics
	if err := MatchPeople(context.Background(), options); err != nil {
		t.Fatalf("MatchPeople: %v\noutput:\n%s\ndiagnostics:\n%s", err, output.String(), diagnostics.String())
	}

	proposals := services.writesTo(http.MethodPost, "/photo-people")
	if len(proposals) != 1 {
		t.Fatalf("POST /photo-people %d times, want once", len(proposals))
	}
	var proposal photostore.PhotoPersonProposal
	if err := json.Unmarshal(proposals[0].body, &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.FaceID != "face_000004" || proposal.PersonID != "person_000001" || proposal.Status != photostore.PhotoPersonSuggested {
		t.Fatalf("proposal = %+v, want face_000004 suggested as person_000001", proposal)
	}
	patches := services.writesTo(http.MethodPatch, "/photo-people/photo_person_000004")
	if len(patches) != 1 {
		t.Fatalf("PATCH the suggestion %d times, want once", len(patches))
	}
	var decision photostore.PhotoPersonDecision
	if err := json.Unmarshal(patches[0].body, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Status == nil || *decision.Status != photostore.PhotoPersonConfirmed || decision.DecidedBy == nil ||
		*decision.DecidedBy != "photo-match-people: face match and test-model agree" || decision.Evidence == nil || !strings.Contains(*decision.Evidence, modelVerdictMarker) {
		t.Fatalf("decision = %s, want confirmed by the model's verdict", patches[0].body)
	}
	if !strings.Contains(output.String(), "model check: asked 1; same 1, different 0, unsure 0, failed 0; 0 wait for a first confirmed face\n") {
		t.Fatalf("output = %q, want the model check's tally", output.String())
	}
	checkEveryRequestWentThroughTheClient(t, transport, services)
}

func TestMatchPeopleModelCheckNeedsTheServiceToken(t *testing.T) {
	services := newFakeServices(t)
	storeURL := services.serve(map[string]http.HandlerFunc{
		"GET /faces":        answerJSON([]photostore.Face{}),
		"GET /photo-people": answerJSON([]photostore.PhotoPerson{}),
		"GET /photos":       answerJSON([]photostore.Photo{}),
	})
	multichatURL := services.serve(map[string]http.HandlerFunc{"GET /api/conversations": answerJSON([]any{})})
	options := DefaultMatchPeopleOptions()
	options.PhotoStoreURL, options.MultichatURL, options.ModelCheck = storeURL, multichatURL, 1
	options.Output, options.DiagnosticOutput = io.Discard, io.Discard
	err := MatchPeople(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "-model-check needs LLMBRIDGE_SERVICE_TOKEN") {
		t.Fatalf("MatchPeople with no service token = %v, want the command's refusal", err)
	}
}

func TestGuessFaceGroupsPostsTheModelsGuess(t *testing.T) {
	services := newFakeServices(t)
	thumbnail := testJPEG(t)
	members := []photostore.Face{face("face_000001", "photo_000001", 0, 0), face("face_000002", "photo_000002", 0, 0.05), face("face_000003", "photo_000003", 0, -0.05)}
	group := photostore.FaceGroup{ID: "face_group_000001", Model: DefaultFaceModel, Status: photostore.FaceGroupOpen, FaceCount: 3, PhotoCount: 3,
		SampleFaceIDs: []string{"face_000001", "face_000002", "face_000003"}, UpdatedAt: 100}
	full := group
	for _, each := range members {
		full.Members = append(full.Members, photostore.FaceGroupMember{FaceID: each.ID, PhotoID: each.PhotoID, Similarity: 0.9})
	}
	links := []photostore.PhotoPerson{{ID: "photo_person_000001", PhotoID: "photo_000001", PersonID: "person_000001", FaceID: "face_000001",
		Status: photostore.PhotoPersonSuggested, Confidence: 0.5}}
	photos := []photostore.Photo{{ID: "photo_000001", TakenAt: 1262304000}, {ID: "photo_000002"}, {ID: "photo_000003"}}
	photoRoutes := map[string]http.HandlerFunc{
		"GET /photo-people":                  answerJSON(links),
		"GET /photos":                        answerJSON(photos),
		"GET /face-groups":                   answerJSON([]photostore.FaceGroup{group}),
		"GET /face-groups/face_group_000001": answerJSON(full),
		"POST /face-groups/face_group_000001/guesses": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		},
	}
	for _, each := range members {
		photoRoutes["GET /faces/"+each.ID+"/image"] = answerBytes(thumbnail)
		photoRoutes["GET /photos/"+each.PhotoID+"/thumbnail"] = answerBytes(thumbnail)
		photoRoutes["GET /photos/"+each.PhotoID+"/faces"] = answerJSON([]photostore.Face{each})
	}
	storeURL := services.serve(photoRoutes)
	multichatURL := services.serve(map[string]http.HandlerFunc{"GET /api/conversations": answerJSON([]any{})})
	peopleURL := services.serve(map[string]http.HandlerFunc{
		"GET /persons/person_000001": answerJSON(map[string]string{"id": "person_000001", "display_name": "Alice"}),
		"GET /persons/person_000009": answerJSON(map[string]string{"id": "person_000009", "display_name": "Bob"}),
	})
	modelStoreURL := services.serve(map[string]http.HandlerFunc{
		"GET /api/models": answerJSON([]map[string]string{{"id": "test-model", "provider": "anthropic"}}),
	})
	bridgeURL := services.serve(map[string]http.HandlerFunc{
		"GET /settings": answerJSON(map[string]any{"settings": []map[string]string{{"key": "oneshot.instance_by_provider", "value": "anthropic:inst-test"}}}),
		"POST /instances/inst-test/oneshot": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-LLM-Bridge-Service-Token") != "test-service-token" {
				t.Errorf("oneshot token = %q", r.Header.Get("X-LLM-Bridge-Service-Token"))
			}
			var request struct {
				Model  string `json:"model"`
				Prompt string `json:"prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Model != "test-model" || !strings.Contains(request.Prompt, "person_000001, Alice") {
				t.Errorf("oneshot asked %q with a prompt that does not list the candidate", request.Model)
			}
			answerJSON(map[string]any{"model": "test-model", "parsed": map[string]any{"person_id": "person_000001", "confidence": 0.8, "one_person": true, "reason": "The face matches."}})(w, r)
		},
	})

	transport := &countingTransport{}
	var output, diagnostics bytes.Buffer
	options := DefaultGuessFaceGroupsOptions()
	options.Operator = "person_000009"
	options.Models, options.ContextOnlyModels = []string{"test-model"}, nil
	options.PhotoStoreURL = storeURL
	options.BridgeURL, options.BridgeServiceToken = bridgeURL, "test-service-token"
	options.ModelStoreURL, options.PeopleStoreURL, options.MultichatURL = modelStoreURL, peopleURL, multichatURL
	options.HTTPClient = &http.Client{Transport: transport}
	options.Output, options.DiagnosticOutput = &output, &diagnostics
	if err := GuessFaceGroups(context.Background(), options); err != nil {
		t.Fatalf("GuessFaceGroups: %v\noutput:\n%s\ndiagnostics:\n%s", err, output.String(), diagnostics.String())
	}

	guesses := services.writesTo(http.MethodPost, "/face-groups/face_group_000001/guesses")
	if len(guesses) != 1 {
		t.Fatalf("POST guesses %d times, want once", len(guesses))
	}
	var guess photostore.FaceGroupGuessProposal
	if err := json.Unmarshal(guesses[0].body, &guess); err != nil {
		t.Fatal(err)
	}
	if guess.PersonID != "person_000001" || guess.ProposedBy != "test-model" || guess.Confidence != 0.8 {
		t.Fatalf("guess = %+v, want person_000001 at 0.8 proposed by test-model", guess)
	}
	if !strings.HasSuffix(output.String(), "asked about 1 groups; wrote 1 guesses\n") {
		t.Fatalf("output = %q, want the run's tally", output.String())
	}
	checkEveryRequestWentThroughTheClient(t, transport, services)
}

func TestGuessFaceGroupsRefusesWhatTheCommandRefuses(t *testing.T) {
	options := DefaultGuessFaceGroupsOptions()
	options.Output, options.DiagnosticOutput = io.Discard, io.Discard
	if err := GuessFaceGroups(context.Background(), options); err == nil || !strings.Contains(err.Error(), "-operator is required") {
		t.Fatalf("no operator = %v, want refused", err)
	}
	options.Operator = "person_000009"
	if err := GuessFaceGroups(context.Background(), options); err == nil || !strings.Contains(err.Error(), "LLMBRIDGE_SERVICE_TOKEN is required") {
		t.Fatalf("no service token = %v, want refused", err)
	}
}

func TestAPassStopsWhenItsContextEnds(t *testing.T) {
	services := newFakeServices(t)
	storeURL := services.serve(map[string]http.HandlerFunc{"GET /faces": answerJSON([]photostore.Face{})})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	options := DefaultGroupFacesOptions()
	options.PhotoStoreURL = storeURL
	options.Output, options.DiagnosticOutput = io.Discard, io.Discard
	if err := GroupFaces(ctx, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("GroupFaces with an ended context = %v, want context.Canceled", err)
	}
}

func TestAPassRefusesMissingPlumbing(t *testing.T) {
	options := DefaultGroupFacesOptions()
	if err := GroupFaces(context.Background(), options); err == nil || !strings.Contains(err.Error(), "Output is required") {
		t.Fatalf("no Output = %v, want refused", err)
	}
}

func TestSplitModelListDropsBlanks(t *testing.T) {
	if got := strings.Join(SplitModelList(" model-a, ,model-b,"), "|"); got != "model-a|model-b" {
		t.Fatalf("SplitModelList = %q", got)
	}
}
