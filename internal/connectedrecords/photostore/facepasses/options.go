// Package facepasses runs photo-store's three face passes as Go functions:
// GroupFaces gathers the faces nobody has named into face groups,
// MatchPeople proposes who is in each photo (and, with ModelCheck, asks a
// model that can see about its suggestions), and GuessFaceGroups asks models
// who each open face group is. The commands photo-group-faces,
// photo-match-people and photo-guess-face-groups are thin wrappers over
// them; a server can call them the same way.
//
// A pass talks to photo-store and the other services only over HTTP,
// through Options.HTTPClient, and stops when its context ends. It never
// exits the process or reads the environment: it returns its errors, writes
// what the commands print on standard output to Output, and writes what they
// print on standard error while they carry on to DiagnosticOutput.
package facepasses

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	photostore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore/internal/facegrouping"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore/internal/photocontext"
)

// Defaults the commands' flags and a server's settings share.
const (
	// DefaultFaceModel is the face model whose faces the passes read.
	DefaultFaceModel = "insightface-buffalo_l"
	// DefaultPhotoStoreURL is where the commands find photo-store when
	// -store-url says nothing.
	DefaultPhotoStoreURL = "http://" + photostore.DefaultListenAddress
	// DefaultModelCheckRole is the model-store role MatchPeople's model check
	// asks; its models must see images.
	DefaultModelCheckRole = "default"
	// DefaultGuessLimit is how many groups GuessFaceGroups asks about at
	// most, the most photos first.
	DefaultGuessLimit = 20
)

// DefaultGuessModels are the model-store ids GuessFaceGroups asks, each on
// its own.
func DefaultGuessModels() []string { return []string{"claude-opus-5-5", "gpt-6-astra"} }

// DefaultContextOnlyModels are the models of DefaultGuessModels asked to
// judge from the records and whole photos, with no faces to compare.
// gpt-6-astra answered on 2026-11-06 that it cannot identify someone from
// their face or confirm a face matches a contact, while reading the records
// fine; asked without faces, it judges from the records.
func DefaultContextOnlyModels() []string { return []string{"gpt-6-astra"} }

// HTTP time limits the commands give their clients: a face pass reads tens
// of thousands of faces in pages, and a model call with images can take
// minutes.
const (
	GroupFacesHTTPTimeout      = 2 * time.Minute
	MatchPeopleHTTPTimeout     = 2 * time.Minute
	GuessFaceGroupsHTTPTimeout = 7 * time.Minute
)

// SplitModelList turns a comma-separated list of model ids, as the -models
// flags take them, into ids, dropping blanks and spaces.
func SplitModelList(list string) []string {
	models := []string{}
	for _, model := range strings.Split(list, ",") {
		if model = strings.TrimSpace(model); model != "" {
			models = append(models, model)
		}
	}
	return models
}

// GroupFacesOptions are what one GroupFaces run reads and writes.
type GroupFacesOptions struct {
	// FaceModel is the face model whose faces to group (-model).
	FaceModel string
	// PhotoStoreURL is photo-store's base URL (-store-url).
	PhotoStoreURL string
	// DryRun decides and reports, and writes nothing (-dry-run).
	DryRun bool
	// Evaluate measures how pure groups are against the confirmed faces and
	// writes nothing (-evaluate).
	Evaluate bool
	// JoinSimilarity is how close a face must be to a group's average face
	// to join it (-join-similarity).
	JoinSimilarity float64

	HTTPClient       *http.Client
	Output           io.Writer
	DiagnosticOutput io.Writer
}

// DefaultGroupFacesOptions are photo-group-faces' flag defaults, with a
// client and no writers.
func DefaultGroupFacesOptions() GroupFacesOptions {
	return GroupFacesOptions{FaceModel: DefaultFaceModel, PhotoStoreURL: DefaultPhotoStoreURL,
		JoinSimilarity: facegrouping.DefaultSettings.JoinSimilarity, HTTPClient: &http.Client{Timeout: GroupFacesHTTPTimeout}}
}

// MatchPeopleOptions are what one MatchPeople run reads and writes.
type MatchPeopleOptions struct {
	// Operator is the person_ id whose photos these are, who is in every
	// chat (-operator).
	Operator string
	// SeedPerson is a person_ id to confirm on SeedPhotoIDs (-seed-person).
	SeedPerson string
	// SeedPhotoIDs are the photos SeedPerson said they are in (-seed-photos,
	// split on commas).
	SeedPhotoIDs []string
	// SeedDecidedBy is who said so, recorded as decided_by (-seed-decided-by).
	SeedDecidedBy string
	// FaceModel is the face model whose faces to match (-model).
	FaceModel string
	// PhotoStoreURL is photo-store's base URL (-store-url).
	PhotoStoreURL string
	// DryRun decides and reports, and writes nothing (-dry-run).
	DryRun bool
	// ModelCheck asks a model that can see about up to this many
	// suggestions after matching; 0 asks none (-model-check).
	ModelCheck int
	// ModelCheckRole is the model-store role the model check asks
	// (-model-role).
	ModelCheckRole string

	MultichatURL    string
	MultichatToken  string
	JournalStoreURL string
	MessageStoreURL string
	PeopleStoreURL  string
	// BridgeURL and BridgeServiceToken reach llm-bridge-server's
	// POST /oneshot; the model check needs both.
	BridgeURL          string
	BridgeServiceToken string

	HTTPClient       *http.Client
	Output           io.Writer
	DiagnosticOutput io.Writer
}

// DefaultMatchPeopleOptions are photo-match-people's flag defaults, with a
// client, no service addresses and no writers.
func DefaultMatchPeopleOptions() MatchPeopleOptions {
	return MatchPeopleOptions{FaceModel: DefaultFaceModel, PhotoStoreURL: DefaultPhotoStoreURL, ModelCheckRole: DefaultModelCheckRole,
		HTTPClient: &http.Client{Timeout: MatchPeopleHTTPTimeout}}
}

// GuessFaceGroupsOptions are what one GuessFaceGroups run reads and writes.
type GuessFaceGroupsOptions struct {
	// Operator is the person_ id whose photos these are (-operator); required.
	Operator string
	// Models are the model-store ids to ask, each on its own (-models).
	Models []string
	// ContextOnlyModels are the models of Models asked with no faces to
	// compare (-context-only-models).
	ContextOnlyModels []string
	// AskAgain asks about groups a model already answered as well
	// (-ask-again).
	AskAgain bool
	// Limit is how many groups to ask about at most, the most photos first
	// (-limit).
	Limit int
	// GroupID asks about this one face group only (-group).
	GroupID string
	// PhotoStoreURL is photo-store's base URL (-store-url).
	PhotoStoreURL string
	// DryRun asks and prints, and writes nothing (-dry-run).
	DryRun bool

	// BridgeURL and BridgeServiceToken reach llm-bridge-server's
	// GET /settings and POST /instances/{id}/oneshot; the token is required.
	BridgeURL          string
	BridgeServiceToken string
	ModelStoreURL      string
	PeopleStoreURL     string
	MessageStoreURL    string
	MultichatURL       string
	MultichatToken     string
	JournalStoreURL    string

	HTTPClient       *http.Client
	Output           io.Writer
	DiagnosticOutput io.Writer
}

// DefaultGuessFaceGroupsOptions are photo-guess-face-groups' flag defaults,
// with a client, no service addresses and no writers.
func DefaultGuessFaceGroupsOptions() GuessFaceGroupsOptions {
	return GuessFaceGroupsOptions{Models: DefaultGuessModels(), ContextOnlyModels: DefaultContextOnlyModels(), Limit: DefaultGuessLimit,
		PhotoStoreURL: DefaultPhotoStoreURL, HTTPClient: &http.Client{Timeout: GuessFaceGroupsHTTPTimeout}}
}

// checkPlumbing refuses options that leave a pass no client or nowhere to
// write.
func checkPlumbing(client *http.Client, output, diagnosticOutput io.Writer) error {
	switch {
	case client == nil:
		return fmt.Errorf("facepasses: HTTPClient is required")
	case output == nil:
		return fmt.Errorf("facepasses: Output is required")
	case diagnosticOutput == nil:
		return fmt.Errorf("facepasses: DiagnosticOutput is required")
	}
	return nil
}

// newClient is the one photocontext client a run makes every request with.
// Diagnostics are written under a lock, since GuessFaceGroups asks its
// models at once.
func newClient(client *http.Client, diagnosticOutput io.Writer) photocontext.Client {
	return photocontext.Client{HTTP: client, Diagnostics: &lockedWriter{writer: diagnosticOutput}}
}

type lockedWriter struct {
	mutex  sync.Mutex
	writer io.Writer
}

func (l *lockedWriter) Write(data []byte) (int, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.writer.Write(data)
}

func trimURL(base string) string { return strings.TrimRight(base, "/") }
