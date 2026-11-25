package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/peoplematching"
	"github.com/kayushkin/photo-store/internal/photocontext"
)

// modelChecker asks a model that can see whether a suggested face is the
// person suggested, by showing it the face beside faces already known to be
// them, and confirms or rejects the suggestion on its answer.
type modelChecker struct {
	web          photocontext.Client
	storeURL     string
	bridgeURL    string
	serviceToken string
	role         string
	faces        map[string]photostore.Face // id → face with its box
	embeddings   map[string][]float32
	years        map[string]string // photo id → the year it was taken, "unknown year" when not known
	thumbnails   map[string]image.Image
}

// modelVerdictMarker starts the sentence a model check adds to a link's
// evidence, and marks a link as already asked.
const modelVerdictMarker = "A model compared the face with"

const modelCheckSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["same","different","unsure"]},"reason":{"type":"string"}},"required":["verdict","reason"]}`

const modelCheckSystemPrompt = `You compare faces in a private family photo archive, for its owner. ` +
	`The first image is one face cut from a photo. The others are faces already confirmed as one person. ` +
	`Each image comes with the year its photo was taken. The archive spans many years, so the person may be ` +
	`a child or teenager in one photo and an adult in another, with or without glasses, a beard or a moustache. ` +
	`Judge by what lasts: eye shape and spacing, nose, brows, ears, and the structure of the face. ` +
	`Say "same" when those clearly match allowing for age, "different" when they clearly do not, and "unsure" only ` +
	`when the face is too small, blurred or turned away to see them. Give a one-sentence reason.`

func (c *modelChecker) thumbnail(photoID string) (image.Image, error) {
	if cached, seen := c.thumbnails[photoID]; seen {
		return cached, nil
	}
	response, err := c.web.HTTP.Get(c.storeURL + "/photos/" + url.PathEscape(photoID) + "/thumbnail")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnail of %s: %d", photoID, response.StatusCode)
	}
	decoded, err := jpeg.Decode(response.Body)
	if err != nil {
		return nil, fmt.Errorf("thumbnail of %s: %w", photoID, err)
	}
	if len(c.thumbnails) > 200 {
		c.thumbnails = map[string]image.Image{}
	}
	c.thumbnails[photoID] = decoded
	return decoded, nil
}

// faceCrop cuts a face from its photo's thumbnail with some room around it,
// as a JPEG for the model.
func (c *modelChecker) faceCrop(faceID string) (msg.OneShotImage, error) {
	face, known := c.faces[faceID]
	if !known {
		return msg.OneShotImage{}, fmt.Errorf("no face %s", faceID)
	}
	picture, err := c.thumbnail(face.PhotoID)
	if err != nil {
		return msg.OneShotImage{}, err
	}
	bounds := picture.Bounds()
	width, height := float64(bounds.Dx()), float64(bounds.Dy())
	marginX := (face.Box.Right - face.Box.Left) * 0.3
	marginY := (face.Box.Bottom - face.Box.Top) * 0.3
	crop := image.Rect(int((face.Box.Left-marginX)*width), int((face.Box.Top-marginY)*height),
		int((face.Box.Right+marginX)*width), int((face.Box.Bottom+marginY)*height)).Intersect(bounds)
	if crop.Empty() {
		return msg.OneShotImage{}, fmt.Errorf("%s's box lies outside its thumbnail", faceID)
	}
	cropper, ok := picture.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return msg.OneShotImage{}, fmt.Errorf("thumbnail of %s cannot be cropped", face.PhotoID)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, cropper.SubImage(crop), &jpeg.Options{Quality: 88}); err != nil {
		return msg.OneShotImage{}, err
	}
	return msg.OneShotImage{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(encoded.Bytes())}, nil
}

type modelVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Model   string `json:"-"`
}

// ask shows the model one face and up to three known faces of a person.
func (c *modelChecker) ask(faceID string, knownFaceIDs []string, context string) (modelVerdict, error) {
	images := []msg.OneShotImage{}
	crop, err := c.faceCrop(faceID)
	if err != nil {
		return modelVerdict{}, err
	}
	images = append(images, crop)
	for _, known := range knownFaceIDs {
		crop, err := c.faceCrop(known)
		if err != nil {
			return modelVerdict{}, err
		}
		images = append(images, crop)
	}
	years := make([]string, 0, len(images))
	for _, id := range append([]string{faceID}, knownFaceIDs...) {
		years = append(years, c.yearOf(c.faces[id].PhotoID))
	}
	prompt := fmt.Sprintf("Image 1 is the face to judge. Images 2 to %d are the person it may be. The photos were taken in, in order: %s.",
		len(images), strings.Join(years, ", "))
	if context != "" {
		prompt += " What the archive knows about where the photo came from: " + context
	}
	body, err := json.Marshal(msg.OneShotRequest{Prompt: prompt, SystemPrompt: modelCheckSystemPrompt, ModelRole: c.role,
		Caller: matcherName, Schema: json.RawMessage(modelCheckSchema), MaxTokens: 400, Images: images})
	if err != nil {
		return modelVerdict{}, err
	}
	request, err := http.NewRequest(http.MethodPost, c.bridgeURL+"/oneshot", bytes.NewReader(body))
	if err != nil {
		return modelVerdict{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-LLM-Bridge-Service-Token", c.serviceToken)
	response, err := c.web.Do(request)
	if err != nil {
		return modelVerdict{}, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return modelVerdict{}, err
	}
	if response.StatusCode != http.StatusOK {
		return modelVerdict{}, fmt.Errorf("POST /oneshot (role %s): %d %s", c.role, response.StatusCode, bytes.TrimSpace(answer))
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(answer, &reply); err != nil {
		return modelVerdict{}, err
	}
	var verdict modelVerdict
	if err := json.Unmarshal(reply.Parsed, &verdict); err != nil {
		return modelVerdict{}, fmt.Errorf("the model's answer is not a verdict: %s", reply.Parsed)
	}
	verdict.Model = reply.Model
	return verdict, nil
}

var personReference = regexp.MustCompile(`ref:(person_[0-9]{6,})`)

// markdownToPlain drops the ref chips from evidence, leaving their text, for
// a prompt.
func markdownToPlain(markdown string) string {
	return personReference.ReplaceAllString(strings.NewReplacer("[", "", "]", "").Replace(markdown), "")
}

// checkSuggestions asks the model about up to limit suggested links with a
// face, and confirms the ones it says are the person and rejects the ones it
// says are not. A link to a person with no confirmed face is left for a
// person to confirm their first face. It answers how many it asked, how they
// came out, and how many wait.
func (c *modelChecker) checkSuggestions(links []photostore.PhotoPerson, limit int, dryRun bool) (map[string]int, error) {
	confirmedFaces := map[string][]string{}
	var suggestions []photostore.PhotoPerson
	for _, link := range links {
		if link.FaceID == "" {
			continue
		}
		switch link.Status {
		case photostore.PhotoPersonConfirmed:
			confirmedFaces[link.PersonID] = append(confirmedFaces[link.PersonID], link.FaceID)
		case photostore.PhotoPersonSuggested:
			suggestions = append(suggestions, link)
		}
	}
	// The surest suggestions first, so a capped run spends on the likeliest.
	sort.SliceStable(suggestions, func(i, j int) bool { return suggestions[i].Confidence > suggestions[j].Confidence })
	outcome := map[string]int{}
	failedInARow := 0
	for _, link := range suggestions {
		if outcome["asked"] >= limit {
			break
		}
		// A suggestion the model already answered "unsure" keeps that
		// answer in its evidence; asking again would spend a call for it.
		if strings.Contains(link.Evidence, modelVerdictMarker) {
			continue
		}
		// Only faces a person confirmed, or confirmed from one they did, say
		// who someone is. Faces merely suggested as a person agree with each
		// other whoever they are: the face that recurs in a chat with someone
		// is often their child. So a person with no confirmed face waits for
		// one, confirmed by hand.
		known := confirmedFaces[link.PersonID]
		comparedWith := "faces confirmed as them"
		if len(known) == 0 {
			outcome["waiting"]++
			continue
		}
		known = c.mostSimilar(link.FaceID, known, 3)
		outcome["asked"]++
		verdict, err := c.ask(link.FaceID, known, contextWithoutScores(link.Evidence))
		if err != nil {
			// One failed call (an overloaded model, a passing limit) skips
			// this link, loudly; several in a row mean the route is down.
			outcome["failed"]++
			failedInARow++
			fmt.Fprintf(os.Stderr, "%s: model check of %s failed: %v\n", matcherName, link.ID, err)
			if failedInARow >= 5 {
				return outcome, fmt.Errorf("five model calls failed in a row; the last: %w", err)
			}
			continue
		}
		failedInARow = 0
		evidence := fmt.Sprintf("%s %s %d %s and said %s: %s", link.Evidence, modelVerdictMarker, len(known), comparedWith, verdict.Verdict, verdict.Reason)
		var change photostore.PhotoPersonDecision
		decidedBy := fmt.Sprintf("%s: face match and %s agree", matcherName, verdict.Model)
		switch verdict.Verdict {
		case "same":
			status := photostore.PhotoPersonConfirmed
			change = photostore.PhotoPersonDecision{Status: &status, DecidedBy: &decidedBy, Evidence: &evidence}
		case "different":
			status := photostore.PhotoPersonRejected
			decidedBy = fmt.Sprintf("%s: %s says this is someone else", matcherName, verdict.Model)
			change = photostore.PhotoPersonDecision{Status: &status, DecidedBy: &decidedBy, Evidence: &evidence}
		default:
			change = photostore.PhotoPersonDecision{Evidence: &evidence}
		}
		outcome[verdict.Verdict]++
		if dryRun {
			fmt.Printf("%s %s %s: %s (%s)\n", link.ID, link.PhotoID, link.PersonID, verdict.Verdict, verdict.Reason)
			continue
		}
		if err := c.patch(link.ID, change); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}

func (c *modelChecker) patch(id string, change photostore.PhotoPersonDecision) error {
	return patchLink(c.web, c.storeURL, id, change)
}

// patchLink sends one PATCH /photo-people/{id}.
func patchLink(web photocontext.Client, storeURL, id string, change photostore.PhotoPersonDecision) error {
	body, err := json.Marshal(change)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPatch, storeURL+"/photo-people/"+url.PathEscape(id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := web.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(response.Body)
		return fmt.Errorf("PATCH /photo-people/%s: %d %s", id, response.StatusCode, bytes.TrimSpace(answer))
	}
	return nil
}

// loadFaceBoxes reads every face's box, which the crops need, and its
// embedding, which picks the known faces most like it.
func (c *modelChecker) loadFaceBoxes(model string) error {
	after := ""
	for {
		query := url.Values{"model": {model}, "limit": {"5000"}, "include_embeddings": {"true"}}
		if after != "" {
			query.Set("after", after)
		}
		var page []photostore.Face
		if err := c.web.GetJSON(c.storeURL+"/faces?"+query.Encode(), nil, &page); err != nil {
			return fmt.Errorf("faces: %w", err)
		}
		for _, face := range page {
			embedding, err := peoplematching.DecodeEmbedding(face.EmbeddingFloat32Base64)
			if err != nil {
				return fmt.Errorf("%s: %w", face.ID, err)
			}
			c.embeddings[face.ID] = embedding
			face.EmbeddingFloat32Base64 = ""
			c.faces[face.ID] = face
		}
		if len(page) < 5000 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// mostSimilar picks the known faces closest to a face, so the model compares
// it with the person as they looked nearest to it, not their first photos.
func (c *modelChecker) mostSimilar(faceID string, known []string, count int) []string {
	ranked := append([]string(nil), known...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return peoplematching.Similarity(c.embeddings[faceID], c.embeddings[ranked[i]]) >
			peoplematching.Similarity(c.embeddings[faceID], c.embeddings[ranked[j]])
	})
	return ranked[:min(count, len(ranked))]
}

// yearOf answers the year a photo was taken, read once from photo-store.
func (c *modelChecker) yearOf(photoID string) string {
	if year, seen := c.years[photoID]; seen {
		return year
	}
	year := "unknown year"
	var photo photostore.Photo
	if err := c.web.GetJSON(c.storeURL+"/photos/"+url.PathEscape(photoID), nil, &photo); err == nil && photo.TakenAt > 0 {
		year = time.Unix(photo.TakenAt, 0).UTC().Format("2006")
	}
	c.years[photoID] = year
	return year
}

var similarityInEvidence = regexp.MustCompile(` ?\(similarity [0-9.]+\)`)

// contextWithoutScores is a link's evidence for the prompt: plain text, no
// chips, and no similarity score, which a model reads as a verdict of its own.
func contextWithoutScores(evidence string) string {
	return similarityInEvidence.ReplaceAllString(markdownToPlain(evidence), "")
}
