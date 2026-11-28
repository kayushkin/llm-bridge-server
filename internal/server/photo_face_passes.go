package server

// photo-store's face passes, run in this process when their settings say so
// (internal/config/photos_and_locations_settings.go). Each is off by
// default: grouping writes groups of faces, matching writes who is on a
// photo, and the two model passes spend money on model calls and write
// guesses about who is in someone's photos.
//
// A round runs every pass that is on once for each owner named in
// photos.face_passes.owner_principal_ids, as that owner: the pass reads
// and writes photo-store, people-store, journal-store and message-store
// through their own handlers with a caller that is the owner and sees no one
// else's records, so two owners' faces are never grouped or matched together,
// even when the owner is an administrator. Model calls and model-store go
// through this server's own routes with its service token. multichat is
// outside this process and is called over HTTP.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/people"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos/facepasses"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// Hosts of the in-process services the face passes call. A request to any
// other host leaves the process.
const (
	facePassPhotoStoreHost   = "photo-store.in-process"
	facePassPeopleStoreHost  = "people-store.in-process"
	facePassJournalStoreHost = "journal-store.in-process"
	facePassMessageStoreHost = "message-store.in-process"
	facePassBridgeHost       = "bridge.in-process"
)

// photoFacePassSettings is what the face passes read at start.
type photoFacePassSettings struct {
	owners            []string
	interval          time.Duration
	multichatURL      string
	grouping          bool
	matching          bool
	modelCheckLimit   int
	modelCheckRole    string
	guessing          bool
	guessModels       []string
	contextOnlyModels []string
	guessLimit        int
}

// readPhotoFacePassSettings reads and checks the settings, refusing a
// combination that could not run.
func (s *Server) readPhotoFacePassSettings() (photoFacePassSettings, error) {
	settings := photoFacePassSettings{
		owners:            s.settings.StringList(config.SettingPhotosFacePassesOwnerPrincipalIDs),
		interval:          s.settings.Duration(config.SettingPhotosFacePassesInterval),
		multichatURL:      strings.TrimRight(s.cfg.MultichatURL, "/"),
		grouping:          s.settings.Boolean(config.SettingPhotosFaceGroupingEnabled),
		matching:          s.settings.Boolean(config.SettingPhotosPeopleMatchingEnabled),
		modelCheckLimit:   s.settings.Integer(config.SettingPhotosPeopleMatchingModelCheckLimit),
		modelCheckRole:    s.settings.String(config.SettingPhotosPeopleMatchingModelCheckRole),
		guessing:          s.settings.Boolean(config.SettingPhotosFaceGroupGuessingEnabled),
		guessModels:       s.settings.StringList(config.SettingPhotosFaceGroupGuessingModels),
		contextOnlyModels: s.settings.StringList(config.SettingPhotosFaceGroupGuessingContextModels),
		guessLimit:        s.settings.Integer(config.SettingPhotosFaceGroupGuessingLimit),
	}
	return settings, checkPhotoFacePassSettings(settings)
}

// checkPhotoFacePassSettings refuses a combination of face pass settings that
// could not run. With every pass off nothing is checked.
func checkPhotoFacePassSettings(settings photoFacePassSettings) error {
	if !settings.grouping && !settings.matching && !settings.guessing {
		return nil
	}
	var problems []string
	for _, owner := range settings.owners {
		if err := ownership.CheckPrincipalID(config.SettingPhotosFacePassesOwnerPrincipalIDs, owner); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if settings.interval <= 0 {
		problems = append(problems, config.SettingPhotosFacePassesInterval+" must be longer than nothing")
	}
	if settings.matching || settings.guessing {
		if settings.multichatURL == "" {
			problems = append(problems, "multichat.url is empty, and people matching and face group guessing read the chat a photo was sent in from multichat")
		}
	}
	if settings.matching {
		if settings.modelCheckLimit < 0 {
			problems = append(problems, config.SettingPhotosPeopleMatchingModelCheckLimit+" is below 0")
		}
		if settings.modelCheckLimit > 0 && settings.modelCheckRole == "" {
			problems = append(problems, config.SettingPhotosPeopleMatchingModelCheckRole+" is empty, and the model check asks it")
		}
	}
	if settings.guessing {
		if len(settings.guessModels) == 0 {
			problems = append(problems, config.SettingPhotosFaceGroupGuessingModels+" names no model")
		}
		for _, model := range settings.contextOnlyModels {
			if !slices.Contains(settings.guessModels, model) {
				problems = append(problems, fmt.Sprintf("%s names %s, which %s does not", config.SettingPhotosFaceGroupGuessingContextModels, model, config.SettingPhotosFaceGroupGuessingModels))
			}
		}
		if settings.guessLimit <= 0 {
			problems = append(problems, config.SettingPhotosFaceGroupGuessingLimit+" must be at least 1")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("photo-store face passes: %s", strings.Join(problems, "; "))
	}
	return nil
}

// StartPhotoFacePasses checks the face pass settings and, when any pass is
// on, runs rounds until ctx ends. It refuses settings that could not run, so
// the server does not start with a pass that would only fail. main calls it
// after EnableMessagesOnlineActivityAndTimeLedger and EnablePhotosAndLocations.
func (s *Server) StartPhotoFacePasses(ctx context.Context) error {
	settings, err := s.readPhotoFacePassSettings()
	if err != nil {
		return err
	}
	if !settings.grouping && !settings.matching && !settings.guessing {
		log.Printf("photo-store: no face pass runs here (%s, %s and %s are off)",
			config.SettingPhotosFaceGroupingEnabled, config.SettingPhotosPeopleMatchingEnabled, config.SettingPhotosFaceGroupGuessingEnabled)
		return nil
	}
	if s.photosAndLocations.Load() == nil || s.connectedRecords.Load() == nil {
		return errors.New("photo-store face passes: EnablePhotosAndLocations must run first")
	}
	if (settings.matching || settings.guessing) && s.messageActivityAndTimeLedger.Load() == nil {
		return errors.New("photo-store face passes: people matching and face group guessing read message-store, so EnableMessagesOnlineActivityAndTimeLedger must run first")
	}
	if len(settings.owners) == 0 {
		log.Printf("photo-store: face passes are on, and %s names no owner, so they run for nobody", config.SettingPhotosFacePassesOwnerPrincipalIDs)
		return nil
	}
	log.Printf("photo-store: face passes (grouping %t, people matching %t with %d model checks, group guessing %t) run every %s for %d owners",
		settings.grouping, settings.matching, settings.modelCheckLimit, settings.guessing, settings.interval, len(settings.owners))
	go func() {
		for {
			s.runPhotoFacePassRound(ctx, settings)
			select {
			case <-ctx.Done():
				return
			case <-time.After(settings.interval):
			}
		}
	}()
	return nil
}

// runPhotoFacePassRound runs every pass that is on for every owner. A pass
// that fails is logged and the round goes on with the next one: one owner's
// failure must not stop another's work, and the next round tries again.
func (s *Server) runPhotoFacePassRound(ctx context.Context, settings photoFacePassSettings) {
	for _, owner := range settings.owners {
		if ctx.Err() != nil {
			return
		}
		if err := s.runPhotoFacePassesForOwner(ctx, settings, owner); err != nil {
			log.Printf("[photo-face-passes] ERROR owner %s: %v", owner, err)
		}
	}
}

// photoFacePassLog writes a pass's report to the server log, one line per
// line, naming the owner and the pass.
type photoFacePassLog struct {
	prefix string
}

func (writer photoFacePassLog) Write(content []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
		if line != "" {
			log.Printf("%s %s", writer.prefix, line)
		}
	}
	return len(content), nil
}

func (s *Server) runPhotoFacePassesForOwner(ctx context.Context, settings photoFacePassSettings, owner string) error {
	client, err := s.photoFacePassClient(owner)
	if err != nil {
		return err
	}
	photoStoreURL := "http://" + facePassPhotoStoreHost
	var errs []error
	if settings.grouping {
		options := facepasses.DefaultGroupFacesOptions()
		options.PhotoStoreURL = photoStoreURL
		options.HTTPClient = client
		options.Output = photoFacePassLog{prefix: "[photo-face-passes] " + owner + " grouping:"}
		options.DiagnosticOutput = options.Output
		if err := facepasses.GroupFaces(ctx, options); err != nil {
			errs = append(errs, fmt.Errorf("face grouping: %w", err))
		}
	}
	if !settings.matching && !settings.guessing {
		return errors.Join(errs...)
	}
	operator, err := s.photoFacePassOperator(ctx, owner)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	multichatToken, err := s.photoFacePassMultichatToken(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if settings.matching {
		options := facepasses.DefaultMatchPeopleOptions()
		options.Operator = operator
		options.PhotoStoreURL = photoStoreURL
		options.ModelCheck = settings.modelCheckLimit
		options.ModelCheckRole = settings.modelCheckRole
		options.MultichatURL, options.MultichatToken = settings.multichatURL, multichatToken
		options.JournalStoreURL = "http://" + facePassJournalStoreHost
		options.MessageStoreURL = "http://" + facePassMessageStoreHost
		options.PeopleStoreURL = "http://" + facePassPeopleStoreHost
		options.BridgeURL, options.BridgeServiceToken = "http://"+facePassBridgeHost, s.cfg.ServiceToken
		options.HTTPClient = client
		options.Output = photoFacePassLog{prefix: "[photo-face-passes] " + owner + " people matching:"}
		options.DiagnosticOutput = options.Output
		if err := facepasses.MatchPeople(ctx, options); err != nil {
			errs = append(errs, fmt.Errorf("people matching: %w", err))
		}
	}
	if settings.guessing {
		options := facepasses.DefaultGuessFaceGroupsOptions()
		options.Operator = operator
		options.Models = settings.guessModels
		options.ContextOnlyModels = settings.contextOnlyModels
		options.Limit = settings.guessLimit
		options.PhotoStoreURL = photoStoreURL
		options.BridgeURL, options.BridgeServiceToken = "http://"+facePassBridgeHost, s.cfg.ServiceToken
		options.ModelStoreURL = "http://" + facePassBridgeHost + "/models"
		options.PeopleStoreURL = "http://" + facePassPeopleStoreHost
		options.MessageStoreURL = "http://" + facePassMessageStoreHost
		options.MultichatURL, options.MultichatToken = settings.multichatURL, multichatToken
		options.JournalStoreURL = "http://" + facePassJournalStoreHost
		options.HTTPClient = client
		options.Output = photoFacePassLog{prefix: "[photo-face-passes] " + owner + " group guessing:"}
		options.DiagnosticOutput = options.Output
		if err := facepasses.GuessFaceGroups(ctx, options); err != nil {
			errs = append(errs, fmt.Errorf("face group guessing: %w", err))
		}
	}
	return errors.Join(errs...)
}

// photoFacePassOperator is the owner's own person in their people-store
// records: the person whose principal_id is the owner. The passes count that
// person in every chat, so an owner with none, or with several, is refused
// rather than guessed.
func (s *Server) photoFacePassOperator(ctx context.Context, owner string) (string, error) {
	connected := s.connectedRecords.Load()
	if connected == nil {
		return "", errConnectedRecordsOff
	}
	persons, err := connected.people.ListPersons(ctx, ownership.OwnedBy(owner), people.PersonFilter{PrincipalID: owner})
	if err != nil {
		return "", fmt.Errorf("people-store, finding the owner's own person: %w", err)
	}
	switch len(persons) {
	case 0:
		return "", fmt.Errorf("the owner's people-store records hold no person whose principal_id is %s, and people matching and group guessing need one; skipped", owner)
	case 1:
		return persons[0].ID, nil
	}
	return "", fmt.Errorf("the owner's people-store records hold %d people whose principal_id is %s, so the passes cannot tell which is the owner; skipped", len(persons), owner)
}

// photoFacePassMultichatToken is multichat's token from auth-store, read for
// each round, as chat links read it.
func (s *Server) photoFacePassMultichatToken(ctx context.Context) (string, error) {
	resolved, err := s.authClient.ResolveByProvider(ctx, chatlinks.MultichatProvider, "", "", "read the chats a photo was sent in, for photo-store's face passes")
	if err != nil {
		return "", fmt.Errorf("auth-store, multichat's token: %w", err)
	}
	return resolved.Secret(), nil
}

// photoFacePassClient is the one client a pass makes every request with: the
// in-process hosts go to their handlers as owner, the bridge host to this
// server with its service token, and every other host over the network.
// message-store's host answers only once message-store is open; grouping,
// which never reads it, runs without it.
func (s *Server) photoFacePassClient(owner string) (*http.Client, error) {
	stores := s.photosAndLocations.Load()
	connected := s.connectedRecords.Load()
	if stores == nil || connected == nil {
		return nil, errors.New("photo-store or people-store is not open on this server")
	}
	caller := ownership.Caller{PrincipalID: owner}
	byHost := map[string]http.Handler{
		facePassPhotoStoreHost:   asOwner(caller, stores.photoHandler),
		facePassPeopleStoreHost:  asOwner(caller, connected.peopleHandler),
		facePassJournalStoreHost: asOwner(caller, connected.journalHandler),
		facePassBridgeHost:       s.asTheInternalService(),
	}
	if messages := s.messageActivityAndTimeLedger.Load(); messages != nil {
		byHost[facePassMessageStoreHost] = asOwner(caller, messages.messageHandler)
	}
	return &http.Client{
		Transport: photoFacePassTransport{
			byHost:  byHost,
			network: http.DefaultTransport,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// asOwner serves a store's handler with caller in the request context and no
// credential headers.
func asOwner(caller ownership.Caller, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served := r.Clone(ownership.ContextWithCaller(r.Context(), caller))
		served.Header = http.Header{}
		for _, kept := range []string{"Content-Type", "Accept"} {
			if value := r.Header.Get(kept); value != "" {
				served.Header.Set(kept, value)
			}
		}
		handler.ServeHTTP(w, served)
	})
}

// asTheInternalService serves this server's own routes as its internal
// service: model calls, GET /settings and model-store.
func (s *Server) asTheInternalService() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served := r.Clone(r.Context())
		served.Header = http.Header{}
		for _, kept := range []string{"Content-Type", "Accept"} {
			if value := r.Header.Get(kept); value != "" {
				served.Header.Set(kept, value)
			}
		}
		served.Header.Set(requestcaller.ServiceTokenHeader, s.cfg.ServiceToken)
		s.ServeHTTP(w, served)
	})
}

// photoFacePassTransport routes a request by its host.
type photoFacePassTransport struct {
	byHost  map[string]http.Handler
	network http.RoundTripper
}

func (transport photoFacePassTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if handler, inProcess := transport.byHost[request.URL.Host]; inProcess {
		return inprocesshttp.Transport{Handler: handler}.RoundTrip(request)
	}
	if strings.HasSuffix(request.URL.Host, ".in-process") {
		return nil, fmt.Errorf("no in-process service answers %s", request.URL.Host)
	}
	return transport.network.RoundTrip(request)
}
