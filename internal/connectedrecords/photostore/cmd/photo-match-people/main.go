// Command photo-match-people proposes who is in each photo. It reads every
// face photo-store holds, the people already confirmed on photos, and each
// photo's context — the chat a photo was sent in (multichat's room members,
// or the sender in a big channel) and the people a transcript citing it links
// — and writes links through POST /photo-people:
//
//   - confirmed, when a face is close to a person's confirmed faces, clearly
//     closer than to anyone else, and the context names that person;
//   - suggested, when a face is close but the context does not name them, or
//     when a face recurs across one person's chat photos (how a person with
//     no confirmed photo gets a first set).
//
// It never changes a confirmed or rejected link; photo-store refuses that.
// -seed-person with -seed-photos confirms, on photos a person said they are
// in, the face that recurs across them, so matching has a start.
//
//	MULTICHAT_TOKEN=… photo-match-people -operator person_000001 -dry-run
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/peoplematching"
	"github.com/kayushkin/photo-store/internal/photocontext"
)

const matcherName = "photo-match-people"

// Faces smaller than this share of the thumbnail's width, or found with less
// certainty, are too unsure to match.
const (
	minimumFaceWidth      = 0.035
	minimumDetectionScore = 0.6
)

func main() {
	operator := flag.String("operator", "", "the person_ id whose photos these are, who is in every chat")
	seedPerson := flag.String("seed-person", "", "a person_ id to confirm on -seed-photos")
	seedPhotos := flag.String("seed-photos", "", "comma-separated photo ids the -seed-person said they are in")
	seedDecidedBy := flag.String("seed-decided-by", "", "who said so, recorded as decided_by")
	model := flag.String("model", "insightface-buffalo_l", "the face model whose faces to match")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	dryRun := flag.Bool("dry-run", false, "decide and report, write nothing")
	modelCheck := flag.Int("model-check", 0, "ask a model that can see about up to this many suggestions after matching, confirming or rejecting them; 0 = none")
	modelRole := flag.String("model-role", "default", "the model-store role the model check asks; its models must see images")
	flag.Parse()
	registry, err := photostore.NewMatchPeopleSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = registry.CheckRequired()
	}
	if err != nil {
		fail(err)
	}
	config := settings{storeURL: strings.TrimRight(*storeURL, "/"), sources: photocontext.SourcesFromSettings(registry)}
	web := photocontext.Client{HTTP: &http.Client{Timeout: 2 * time.Minute}}
	if err := run(web, config, *operator, *model, *seedPerson, *seedPhotos, *seedDecidedBy, *dryRun); err != nil {
		fail(err)
	}
	if *modelCheck > 0 {
		token := registry.String(photostore.SettingBridgeServiceToken)
		if token == "" {
			fail(fmt.Errorf("-model-check needs LLMBRIDGE_SERVICE_TOKEN: POST /oneshot answers nothing without it"))
		}
		checker := &modelChecker{web: web, storeURL: config.storeURL, bridgeURL: strings.TrimRight(registry.String(photostore.SettingBridgeURL), "/"),
			serviceToken: token, role: *modelRole, faces: map[string]photostore.Face{}, embeddings: map[string][]float32{},
			years: map[string]string{}, thumbnails: map[string]image.Image{}}
		if err := checker.loadFaceBoxes(*model); err != nil {
			fail(err)
		}
		links, err := photocontext.LoadLinks(web, config.storeURL)
		if err != nil {
			fail(err)
		}
		outcome, err := checker.checkSuggestions(links, *modelCheck, *dryRun)
		fmt.Printf("model check: asked %d; same %d, different %d, unsure %d, failed %d; %d wait for a first confirmed face\n", outcome["asked"], outcome["same"], outcome["different"], outcome["unsure"], outcome["failed"], outcome["waiting"])
		if err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", matcherName, err)
	os.Exit(1)
}

// settings are where the matcher reads and writes.
type settings struct {
	storeURL string
	sources  photocontext.Sources
}

func run(web photocontext.Client, config settings, operator, model, seedPerson, seedPhotos, seedDecidedBy string, dryRun bool) error {
	faces, err := loadFaces(web, config.storeURL, model)
	if err != nil {
		return err
	}
	facesByPhoto := map[string][]peoplematching.Face{}
	faceByID := map[string]peoplematching.Face{}
	for _, face := range faces {
		facesByPhoto[face.PhotoID] = append(facesByPhoto[face.PhotoID], face)
		faceByID[face.ID] = face
	}
	links, err := photocontext.LoadLinks(web, config.storeURL)
	if err != nil {
		return err
	}
	linked := map[string]photostore.PhotoPerson{} // photo|person
	withdrawn := 0
	for _, link := range links {
		// This matcher's own confirmation rested on the face it named; when a
		// rescan retired that face the confirmation has nothing under it, so
		// it goes back to suggested. A person's confirmation stays.
		if link.Status == photostore.PhotoPersonConfirmed && link.FaceID == "" && strings.HasPrefix(link.DecidedBy, matcherName) {
			if !dryRun {
				suggested, decidedBy := photostore.PhotoPersonSuggested, matcherName+": the face it confirmed was retired"
				if err := patchLink(web, config.storeURL, link.ID, photostore.PhotoPersonDecision{Status: &suggested, DecidedBy: &decidedBy}); err != nil {
					return err
				}
			}
			link.Status, link.DecidedBy = photostore.PhotoPersonSuggested, ""
			withdrawn++
		}
		linked[link.PhotoID+"|"+link.PersonID] = link
	}
	if withdrawn > 0 {
		fmt.Printf("returned %d of its own confirmations to suggested: their faces were retired\n", withdrawn)
	}
	written := map[string]int{}
	propose := func(proposal photostore.PhotoPersonProposal) error {
		existing, seen := linked[proposal.PhotoID+"|"+proposal.PersonID]
		// A confirmed link that lost its face to a rescan takes the face found
		// for the same person, and stays confirmed.
		reattach := seen && existing.Status == photostore.PhotoPersonConfirmed && existing.FaceID == "" && proposal.FaceID != ""
		if seen && !reattach && (existing.Status != photostore.PhotoPersonSuggested || proposal.Status == photostore.PhotoPersonSuggested) {
			return nil
		}
		if reattach {
			written["reattached"]++
			existing.FaceID = proposal.FaceID
			linked[proposal.PhotoID+"|"+proposal.PersonID] = existing
		} else {
			written[proposal.Status]++
			linked[proposal.PhotoID+"|"+proposal.PersonID] = photostore.PhotoPerson{PhotoID: proposal.PhotoID, PersonID: proposal.PersonID, FaceID: proposal.FaceID, Status: proposal.Status}
		}
		if dryRun {
			return nil
		}
		status, body, err := web.PostJSON(config.storeURL+"/photo-people", proposal)
		if err != nil {
			return err
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return fmt.Errorf("POST /photo-people %s %s: %d %s", proposal.PhotoID, proposal.PersonID, status, bytes.TrimSpace(body))
		}
		return nil
	}

	if seedPerson != "" {
		if seedDecidedBy == "" {
			return fmt.Errorf("-seed-person needs -seed-decided-by: who said these photos are them")
		}
		chosen := map[string][]peoplematching.Face{}
		for _, photoID := range strings.Split(seedPhotos, ",") {
			if photoFaces := facesByPhoto[strings.TrimSpace(photoID)]; len(photoFaces) > 0 {
				chosen[strings.TrimSpace(photoID)] = photoFaces
			}
		}
		for _, seed := range peoplematching.SeedFaces(chosen) {
			if err := propose(photostore.PhotoPersonProposal{PhotoID: seed.PhotoID, PersonID: seedPerson, FaceID: seed.ID, Status: photostore.PhotoPersonConfirmed,
				Confidence: 1, Evidence: "The person said this photo is them; this face recurs across the photos they named.", ProposedBy: matcherName, DecidedBy: seedDecidedBy}); err != nil {
				return err
			}
		}
	}

	photos, err := photocontext.LoadPhotos(web, config.storeURL)
	if err != nil {
		return err
	}
	reader, err := photocontext.NewReader(web, config.sources, operator)
	if err != nil {
		return err
	}
	contextOf := map[string]map[string]string{}
	for _, photo := range photos {
		if len(facesByPhoto[photo.ID]) == 0 {
			continue
		}
		named, err := reader.PeopleOf(photo)
		if err != nil {
			return err
		}
		contextOf[photo.ID] = named
	}

	references := func() map[string][]peoplematching.Face {
		byPerson := map[string][]peoplematching.Face{}
		for _, link := range linked {
			if link.Status == photostore.PhotoPersonConfirmed && link.FaceID != "" {
				if face, known := faceByID[link.FaceID]; known {
					byPerson[link.PersonID] = append(byPerson[link.PersonID], face)
				}
			}
		}
		return byPerson
	}

	// First: match every face against the people already confirmed.
	known := references()
	for _, face := range faces {
		context := map[string]bool{}
		for personID := range contextOf[face.PhotoID] {
			context[personID] = true
		}
		verdict := peoplematching.Decide(peoplematching.ScorePeople(face, known), context)
		if verdict.Status == "" {
			continue
		}
		evidence := fmt.Sprintf("Face matches %d confirmed photos of [them](ref:%s) (similarity %.2f).", len(known[verdict.PersonID]), verdict.PersonID, verdict.Similarity)
		if reason, named := contextOf[face.PhotoID][verdict.PersonID]; named {
			evidence += " The photo was " + reason + ", which names them."
		}
		proposal := photostore.PhotoPersonProposal{PhotoID: face.PhotoID, PersonID: verdict.PersonID, FaceID: face.ID, Status: verdict.Status,
			Confidence: verdict.Similarity, Evidence: evidence, ProposedBy: matcherName}
		if verdict.Status == photostore.PhotoPersonConfirmed {
			proposal.DecidedBy = matcherName + ": face match and context agree"
		}
		if err := propose(proposal); err != nil {
			return err
		}
	}

	// Then: for each person the context names who has no confirmed face, the
	// face that recurs across their one-to-one photos, leaving out faces of
	// people already known and faces already linked to anyone.
	// Only photos whose context names one person besides the operator: in a
	// group chat the face that recurs is one member's, and nothing here says
	// whose, so a cluster there would be suggested as every member.
	photosOfPerson := map[string][]string{}
	for photoID, named := range contextOf {
		var others []string
		for personID := range named {
			if personID != operator {
				others = append(others, personID)
			}
		}
		if len(others) == 1 {
			photosOfPerson[others[0]] = append(photosOfPerson[others[0]], photoID)
		}
	}
	// A face already linked to someone, any status, is not offered again.
	claimedFaces := map[string]bool{}
	for _, link := range linked {
		if link.FaceID != "" {
			claimedFaces[link.FaceID] = true
		}
	}
	people := make([]string, 0, len(photosOfPerson))
	for personID := range photosOfPerson {
		people = append(people, personID)
	}
	sort.Strings(people)
	for _, personID := range people {
		if len(known[personID]) > 0 {
			continue
		}
		var candidates []peoplematching.Face
		for _, photoID := range photosOfPerson[personID] {
			for _, face := range facesByPhoto[photoID] {
				if claimedFaces[face.ID] {
					continue
				}
				if scores := peoplematching.ScorePeople(face, known); len(scores) > 0 && scores[0].Similarity >= peoplematching.SuggestSimilarity {
					continue
				}
				candidates = append(candidates, face)
			}
		}
		cluster := peoplematching.ClusterInContext(candidates)
		for _, face := range cluster {
			claimedFaces[face.ID] = true
			if err := propose(photostore.PhotoPersonProposal{PhotoID: face.PhotoID, PersonID: personID, FaceID: face.ID, Status: photostore.PhotoPersonSuggested,
				Confidence: 0.5, ProposedBy: matcherName,
				Evidence: fmt.Sprintf("The same face recurs on %d photos whose chats name [them](ref:%s); this photo was %s.", len(cluster), personID, contextOf[face.PhotoID][personID])}); err != nil {
				return err
			}
		}
	}
	mode := "wrote"
	if dryRun {
		mode = "would write"
	}
	fmt.Printf("%d faces on %d photos; %d photos with context; %s %d confirmed and %d suggested links, and gave %d confirmed links their face again\n",
		len(faces), len(facesByPhoto), len(contextOf), mode, written[photostore.PhotoPersonConfirmed], written[photostore.PhotoPersonSuggested], written["reattached"])
	return nil
}

func loadFaces(web photocontext.Client, storeURL, model string) ([]peoplematching.Face, error) {
	var faces []peoplematching.Face
	after := ""
	for {
		query := url.Values{"model": {model}, "include_embeddings": {"true"}, "limit": {"5000"}}
		if after != "" {
			query.Set("after", after)
		}
		var page []photostore.Face
		if err := web.GetJSON(storeURL+"/faces?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("faces: %w", err)
		}
		for _, face := range page {
			if face.DetectionScore < minimumDetectionScore || face.Box.Right-face.Box.Left < minimumFaceWidth {
				continue
			}
			embedding, err := peoplematching.DecodeEmbedding(face.EmbeddingFloat32Base64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", face.ID, err)
			}
			faces = append(faces, peoplematching.Face{ID: face.ID, PhotoID: face.PhotoID, Embedding: embedding})
		}
		if len(page) < 5000 {
			return faces, nil
		}
		after = page[len(page)-1].ID
	}
}
