// Command photo-guess-face-groups asks models that can see who each open
// face group is, and keeps each answer as a guess on the group for a person
// to decide. A guess leans on context first: who posted each photo or was
// in the small chat it was sent in, the caption and the messages sent around
// it, the photo's description, and the people a matcher already suggested on
// its faces. The model sees a sample of the group's faces, a few whole photos
// with the face boxed, and a confirmed face of each candidate who has one.
//
// Each model named with -models answers on its own, so two models that agree
// are worth more than one. A model is called through llm-bridge-server's
// POST /instances/{id}/oneshot on the instance the bridge's
// oneshot.instance_by_provider setting names for the model's provider in
// model-store. A group is asked again only when it gained faces after the
// model's last guess.
//
//	LLMBRIDGE_SERVICE_TOKEN=… MULTICHAT_TOKEN=… photo-guess-face-groups -operator person_000001 -limit 20
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/photocontext"
)

const guesserName = "photo-guess-face-groups"

// Limits on what one question shows a model.
const (
	groupFacesShown       = 8
	wholePhotosShown      = 3
	candidatesWithFaces   = 6
	photosDescribed       = 14
	photosReadForContext  = 60
	candidatesListed      = 10
	messagesAroundAPhoto  = 6
	messageWindowMillis   = 20 * 60 * 1000
	wholePhotoLongestSide = 768
)

const guessSchema = `{"type":"object","properties":{` +
	`"person_id":{"type":"string","description":"a candidate's person id, or empty when none of them fits"},` +
	`"confidence":{"type":"number","description":"0 to 1: how likely the answer is right"},` +
	`"one_person":{"type":"boolean","description":"false when the faces are clearly more than one person"},` +
	`"reason":{"type":"string","description":"two to four sentences: the evidence, and when person_id is empty, who it might be"}},` +
	`"required":["person_id","confidence","one_person","reason"]}`

const systemPrompt = `You help the owner of a private photo archive work out who someone is. ` +
	`A face-recognition model grouped the faces you are shown as one person. You also see a few whole photos they ` +
	`come from, with that person inside a red box, and what the archive knows about the photos: where each was ` +
	`posted or sent and by whom, who was in that chat, the caption and messages sent around it, and a description. ` +
	`Candidates are people in the owner's contacts, each with an id, a name, and how many of the group's photos ` +
	`their context names them on; some come with a face already confirmed as them. ` +
	`Lean on the context: a person who posts photos of an event is often not in them, a small private chat names ` +
	`its few members, a caption or reply may name who is shown, and the setting, era and company say a lot. ` +
	`Compare with a candidate's confirmed face when there is one, allowing for age, hair, glasses and beards. The owner's own rejections are facts: a matcher once mistook a stranger for a contact from chat context alone, and the owner rejected it. ` +
	`Answer with a candidate's id only when the evidence points to them; otherwise leave person_id empty and say ` +
	`who it might be, for example a chat member the archive has no person for. Say one_person false when the ` +
	`faces are clearly several people. Be calibrated: a confidence of 0.9 should be right nine times in ten.`

const contextOnlySystemPrompt = `You help the owner of a private photo archive keep its records straight. ` +
	`A face-recognition model grouped one person's appearances across the owner's photos. You see a few of those ` +
	`photos, that person inside a red box, and what the archive's own records say about the photos: where each was ` +
	`posted or sent and by whom, who was in that chat, the caption and messages sent around it, and a description ` +
	`written earlier. Candidates are people in the owner's contacts, each with an id, a name, and how many of the ` +
	`photos their records name them on: their chats, or a description of the photo that links them. Do not judge by facial features: decide from the records and the scene ` +
	`which candidate the records point to as the boxed person — a caption or description naming who is shown, the ` +
	`few members of a private chat, a recurring role such as the bride at a wedding, the sender of selfies. A person ` +
	`who posts photos of an event is often not in them. When the records point to nobody, leave person_id empty and ` +
	`say who it might be. Say one_person false when the boxed people are clearly different people. Be calibrated: ` +
	`a confidence of 0.9 should be right nine times in ten.`

type settings struct {
	storeURL, bridgeURL, bridgeToken, modelStoreURL, peopleURL, messageStoreURL string
	multichatURL, multichatToken                                                string
	sources                                                                     photocontext.Sources
}

type guesser struct {
	web       photocontext.Client
	config    settings
	reader    *photocontext.Reader
	operator  string
	instances map[string]string // model id → bridge instance id
	links     []photostore.PhotoPerson
	photos    map[string]photostore.Photo
	persons   map[string]person
	messages  map[string]photoMessage // photo id → its message
	// contextOnly are models asked without faces to compare.
	contextOnly map[string]bool
	mutex       sync.Mutex
}

type person struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	HowMet      string `json:"how_met"`
}

func main() {
	operator := flag.String("operator", "", "the person_ id whose photos these are")
	models := flag.String("models", "claude-opus-5-5,gpt-6-astra", "comma-separated model-store ids to ask, each on its own")
	// gpt-6-astra answered on 2026-11-06 that it cannot identify someone from
	// their face or confirm a face matches a contact, while reading the
	// records fine; asked without faces, it judges from the records.
	contextOnlyModels := flag.String("context-only-models", "gpt-6-astra", "comma-separated models of -models asked to judge from the records and whole photos, with no faces to compare")
	askAgain := flag.Bool("ask-again", false, "ask about groups a model already answered, as well")
	limit := flag.Int("limit", 20, "ask about at most this many groups, the most photos first")
	groupID := flag.String("group", "", "ask about this one face group only")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	dryRun := flag.Bool("dry-run", false, "ask and print, write nothing")
	flag.Parse()
	registry, err := photostore.NewGuessFaceGroupsSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = registry.CheckRequired()
	}
	if err != nil {
		fail(err)
	}
	if *operator == "" {
		fail(fmt.Errorf("-operator is required: the person_ id whose photos these are"))
	}
	config := settings{storeURL: strings.TrimRight(*storeURL, "/"),
		bridgeURL: strings.TrimRight(registry.String(photostore.SettingBridgeURL), "/"), bridgeToken: registry.String(photostore.SettingBridgeServiceToken),
		modelStoreURL: strings.TrimRight(registry.String(photostore.SettingModelStoreURL), "/"), peopleURL: strings.TrimRight(registry.String(photostore.SettingPeopleStoreURL), "/"),
		messageStoreURL: strings.TrimRight(registry.String(photostore.SettingMessageStoreURL), "/"),
		multichatURL:    strings.TrimRight(registry.String(photostore.SettingMultichatURL), "/"), multichatToken: registry.String(photostore.SettingMultichatToken),
		sources: photocontext.SourcesFromSettings(registry)}
	if config.bridgeToken == "" {
		fail(fmt.Errorf("LLMBRIDGE_SERVICE_TOKEN is required: the bridge answers no model call without it"))
	}
	web := photocontext.Client{HTTP: &http.Client{Timeout: 7 * time.Minute}}
	modelIDs := []string{}
	for _, model := range strings.Split(*models, ",") {
		if model = strings.TrimSpace(model); model != "" {
			modelIDs = append(modelIDs, model)
		}
	}
	contextOnly := map[string]bool{}
	for _, model := range strings.Split(*contextOnlyModels, ",") {
		if model = strings.TrimSpace(model); model != "" {
			contextOnly[model] = true
		}
	}
	if err := run(web, config, *operator, modelIDs, contextOnly, *askAgain, *limit, *groupID, *dryRun); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", guesserName, err)
	os.Exit(1)
}

// instancesFor finds the bridge instance that calls each model: the model's
// provider in model-store, then that provider's instance in the bridge's
// oneshot.instance_by_provider setting.
func instancesFor(web photocontext.Client, config settings, models []string) (map[string]string, error) {
	var catalog []struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	}
	if err := web.GetJSON(config.modelStoreURL+"/api/models", nil, &catalog); err != nil {
		return nil, fmt.Errorf("model-store models: %w", err)
	}
	providerOf := map[string]string{}
	for _, model := range catalog {
		providerOf[model.ID] = model.Provider
	}
	var bridgeSettings struct {
		Settings []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"settings"`
	}
	if err := web.GetJSON(config.bridgeURL+"/settings", map[string]string{"X-LLM-Bridge-Service-Token": config.bridgeToken}, &bridgeSettings); err != nil {
		return nil, fmt.Errorf("llm-bridge-server settings: %w", err)
	}
	instanceOf := map[string]string{}
	for _, setting := range bridgeSettings.Settings {
		if setting.Key != "oneshot.instance_by_provider" {
			continue
		}
		for _, pair := range strings.Split(setting.Value, ",") {
			if provider, instance, found := strings.Cut(strings.TrimSpace(pair), ":"); found {
				instanceOf[provider] = instance
			}
		}
	}
	instances := map[string]string{}
	for _, model := range models {
		provider, known := providerOf[model]
		if !known {
			return nil, fmt.Errorf("model-store has no model %q", model)
		}
		instance, configured := instanceOf[provider]
		if !configured {
			return nil, fmt.Errorf("the bridge's oneshot.instance_by_provider names no instance for %s, the provider of %s", provider, model)
		}
		instances[model] = instance
	}
	return instances, nil
}

func run(web photocontext.Client, config settings, operator string, models []string, contextOnly map[string]bool, askAgain bool, limit int, onlyGroup string, dryRun bool) error {
	instances, err := instancesFor(web, config, models)
	if err != nil {
		return err
	}
	reader, err := photocontext.NewReader(web, config.sources, operator)
	if err != nil {
		return err
	}
	links, err := photocontext.LoadLinks(web, config.storeURL)
	if err != nil {
		return err
	}
	photoList, err := photocontext.LoadPhotos(web, config.storeURL)
	if err != nil {
		return err
	}
	g := &guesser{web: web, config: config, reader: reader, operator: operator, instances: instances, links: links,
		photos: map[string]photostore.Photo{}, persons: map[string]person{},
		messages: map[string]photoMessage{}, contextOnly: contextOnly}
	for _, photo := range photoList {
		g.photos[photo.ID] = photo
	}
	var groups []photostore.FaceGroup
	if onlyGroup != "" {
		groups = []photostore.FaceGroup{{ID: onlyGroup}}
	} else if err := web.GetJSON(fmt.Sprintf("%s/face-groups?status=open&limit=%d", config.storeURL, 1000), nil, &groups); err != nil {
		return fmt.Errorf("open face groups: %w", err)
	}
	asked, written, failedInARow := 0, 0, 0
	for _, listed := range groups {
		if asked >= limit {
			break
		}
		var group photostore.FaceGroup
		if err := web.GetJSON(config.storeURL+"/face-groups/"+url.PathEscape(listed.ID), nil, &group); err != nil {
			return fmt.Errorf("face group %s: %w", listed.ID, err)
		}
		waiting := []string{}
		for _, model := range models {
			if askAgain || !answeredSinceLastChange(group, model) {
				waiting = append(waiting, model)
			}
		}
		if len(waiting) == 0 {
			continue
		}
		asked++
		questions := map[bool]question{}
		for _, model := range waiting {
			withFaces := !g.contextOnly[model]
			if _, built := questions[withFaces]; built {
				continue
			}
			built, err := g.question(group, withFaces)
			if err != nil {
				return fmt.Errorf("%s: %w", group.ID, err)
			}
			questions[withFaces] = built
		}
		type answer struct {
			model string
			guess photostore.FaceGroupGuessProposal
			err   error
		}
		answers := make(chan answer, len(waiting))
		for _, model := range waiting {
			go func(model string) {
				guess, err := g.ask(model, questions[!g.contextOnly[model]])
				answers <- answer{model, guess, err}
			}(model)
		}
		for range waiting {
			each := <-answers
			if each.err != nil {
				fmt.Fprintf(os.Stderr, "%s: %s asking %s: %v\n", guesserName, group.ID, each.model, each.err)
				failedInARow++
				if failedInARow >= 5 {
					return fmt.Errorf("five model calls failed in a row; the last: %w", each.err)
				}
				continue
			}
			failedInARow = 0
			fmt.Printf("%s (%d photos): %s says %q at %.2f: %s\n", group.ID, group.PhotoCount, each.model, each.guess.PersonID, each.guess.Confidence, firstLine(each.guess.Evidence))
			if dryRun {
				continue
			}
			status, body, err := web.PostJSON(config.storeURL+"/face-groups/"+url.PathEscape(group.ID)+"/guesses", each.guess)
			if err != nil {
				return err
			}
			if status != http.StatusCreated {
				return fmt.Errorf("POST guess on %s: %d %s", group.ID, status, bytes.TrimSpace(body))
			}
			written++
		}
	}
	fmt.Printf("asked about %d groups; wrote %d guesses\n", asked, written)
	return nil
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return line
}

// answeredSinceLastChange says whether the model guessed after the group
// last gained faces.
func answeredSinceLastChange(group photostore.FaceGroup, model string) bool {
	for _, guess := range group.Guesses {
		if guess.ProposedBy == model && guess.CreatedAt >= group.UpdatedAt {
			return true
		}
	}
	return false
}

// question is everything one group's prompt shows, the same for each model.
type question struct {
	groupID    string
	prompt     string
	images     []msg.OneShotImage
	candidates map[string]bool
	tally      string
	withFaces  bool
}

func (g *guesser) personByID(id string) (person, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if known, cached := g.persons[id]; cached {
		return known, nil
	}
	var found person
	if err := g.web.GetJSON(g.config.peopleURL+"/persons/"+url.PathEscape(id), nil, &found); err != nil {
		return person{}, fmt.Errorf("people-store %s: %w", id, err)
	}
	g.persons[id] = found
	return found, nil
}

// photoMessage is the chat message a photo came in and the messages sent
// around it, read from message-store.
type photoMessage struct {
	sender, caption string
	around          []string
}

var personReference = regexp.MustCompile(`ref:(person_[0-9]{6,})`)

var fileNameLike = regexp.MustCompile(`(?i)^[^\s]+\.(jpe?g|png|gif|webp|heic|mp4|mov)$`)

func (g *guesser) messageOf(photo photostore.Photo) (photoMessage, error) {
	g.mutex.Lock()
	cached, seen := g.messages[photo.ID]
	g.mutex.Unlock()
	if seen {
		return cached, nil
	}
	found, err := g.readMessageOf(photo)
	if err != nil {
		return photoMessage{}, err
	}
	g.mutex.Lock()
	g.messages[photo.ID] = found
	g.mutex.Unlock()
	return found, nil
}

func (g *guesser) readMessageOf(photo photostore.Photo) (photoMessage, error) {
	messageID := ""
	switch {
	case strings.HasPrefix(photo.SourcePath, "chat_message_"):
		messageID = photo.SourcePath
	case strings.HasPrefix(photo.SourcePath, "message_"):
		var logged struct {
			Messages []struct {
				DiscordMessageID string `json:"discord_message_id"`
			} `json:"messages"`
		}
		err := g.web.GetJSON(g.config.multichatURL+"/api/message-log/"+url.PathEscape(photo.SourcePath), map[string]string{"Authorization": "Bearer " + g.config.multichatToken}, &logged)
		if err == photocontext.ErrNotFound || (err == nil && (len(logged.Messages) == 0 || logged.Messages[0].DiscordMessageID == "")) {
			return photoMessage{}, nil
		}
		if err != nil {
			return photoMessage{}, fmt.Errorf("multichat %s: %w", photo.SourcePath, err)
		}
		var found struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := g.web.GetJSON(g.config.messageStoreURL+"/messages?"+url.Values{"source": {"discord"}, "source_ref": {logged.Messages[0].DiscordMessageID}}.Encode(), nil, &found); err != nil {
			return photoMessage{}, fmt.Errorf("message-store discord %s: %w", logged.Messages[0].DiscordMessageID, err)
		}
		if len(found.Messages) == 0 {
			return photoMessage{}, nil
		}
		messageID = found.Messages[0].ID
	default:
		return photoMessage{}, nil
	}
	type stored struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		Body           string `json:"body"`
		SentAt         int64  `json:"sent_at"`
		SenderName     string `json:"sender_name"`
		SenderPersonID string `json:"sender_person_id"`
	}
	var message stored
	if err := g.web.GetJSON(g.config.messageStoreURL+"/messages/"+url.PathEscape(messageID), nil, &message); err == photocontext.ErrNotFound {
		return photoMessage{}, nil
	} else if err != nil {
		return photoMessage{}, fmt.Errorf("message-store %s: %w", messageID, err)
	}
	result := photoMessage{sender: message.SenderName}
	if message.SenderPersonID != "" {
		result.sender = fmt.Sprintf("%s (%s)", message.SenderName, message.SenderPersonID)
	}
	if body := strings.TrimSpace(message.Body); body != "" && !fileNameLike.MatchString(body) {
		result.caption = body
	}
	var around struct {
		Messages []stored `json:"messages"`
	}
	query := url.Values{"conversation": {message.ConversationID}, "after": {fmt.Sprint(message.SentAt - messageWindowMillis)},
		"before": {fmt.Sprint(message.SentAt + messageWindowMillis)}, "order": {"oldest"}, "limit": {"40"}}
	if err := g.web.GetJSON(g.config.messageStoreURL+"/messages?"+query.Encode(), nil, &around); err != nil {
		return photoMessage{}, fmt.Errorf("message-store around %s: %w", messageID, err)
	}
	// The messages closest in time to the photo's, its own left out.
	sort.SliceStable(around.Messages, func(i, j int) bool {
		return abs(around.Messages[i].SentAt-message.SentAt) < abs(around.Messages[j].SentAt-message.SentAt)
	})
	for _, each := range around.Messages {
		if len(result.around) >= messagesAroundAPhoto {
			break
		}
		body := strings.TrimSpace(each.Body)
		if each.ID == message.ID || body == "" || fileNameLike.MatchString(body) {
			continue
		}
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		result.around = append(result.around, fmt.Sprintf("%s: %s", each.SenderName, body))
	}
	return result, nil
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// spread picks up to count items evenly from a list.
func spread[T any](items []T, count int) []T {
	if len(items) <= count {
		return items
	}
	picked := make([]T, 0, count)
	for index := 0; index < count; index++ {
		picked = append(picked, items[index*len(items)/count])
	}
	return picked
}

// question builds one group's prompt. withFaces shows the group's faces and
// candidates' confirmed faces and asks for a comparison; without, the model
// sees only the whole photos, the person boxed, and judges from the records.
func (g *guesser) question(group photostore.FaceGroup, withFaces bool) (question, error) {
	q := question{groupID: group.ID, candidates: map[string]bool{}, withFaces: withFaces}
	memberPhotos := map[string]bool{}
	memberFaces := map[string]bool{}
	byTime := append([]photostore.FaceGroupMember(nil), group.Members...)
	sort.SliceStable(byTime, func(i, j int) bool { return byTime[i].TakenAt < byTime[j].TakenAt })
	for _, member := range group.Members {
		memberPhotos[member.PhotoID] = true
		memberFaces[member.FaceID] = true
	}

	// Who the context of each photo names, counted over the group.
	named := map[string]int{}
	reasons := map[string][]string{}
	readPhotos := spread(byTime, photosReadForContext)
	for _, member := range readPhotos {
		photo, known := g.photos[member.PhotoID]
		if !known {
			continue
		}
		people, err := g.reader.PeopleOf(photo)
		if err != nil {
			return question{}, err
		}
		for personID, reason := range people {
			named[personID]++
			if len(reasons[personID]) < 2 {
				reasons[personID] = append(reasons[personID], reason)
			}
		}
	}
	// People a photo's description links ([Vika](ref:person_…)): whoever
	// described it knew who was there.
	described := map[string]int{}
	for _, member := range readPhotos {
		seen := map[string]bool{}
		for _, match := range personReference.FindAllStringSubmatch(g.photos[member.PhotoID].Description, -1) {
			if !seen[match[1]] {
				seen[match[1]] = true
				described[match[1]]++
			}
		}
	}
	suggested := map[string]int{}
	// rejected counts the group's photos on which a person said someone is
	// not there: a decision the models must not argue with.
	rejected := map[string]int{}
	for _, link := range g.links {
		if memberFaces[link.FaceID] && link.Status == photostore.PhotoPersonSuggested {
			suggested[link.PersonID]++
		}
		if memberPhotos[link.PhotoID] && link.Status == photostore.PhotoPersonRejected {
			rejected[link.PersonID]++
		}
	}
	scores := map[string]int{}
	for personID, count := range named {
		scores[personID] += 2 * count
	}
	for personID, count := range suggested {
		scores[personID] += count
	}
	for personID, count := range described {
		scores[personID] += 2 * count
	}
	for personID, count := range rejected {
		if _, candidate := scores[personID]; candidate {
			scores[personID] -= 3 * count
		}
	}
	ranked := make([]string, 0, len(scores))
	for personID := range scores {
		ranked = append(ranked, personID)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if scores[ranked[i]] != scores[ranked[j]] {
			return scores[ranked[i]] > scores[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	if len(ranked) > candidatesListed {
		ranked = ranked[:candidatesListed]
	}
	confirmedFace := map[string]string{}
	for _, link := range g.links {
		if link.Status == photostore.PhotoPersonConfirmed && link.FaceID != "" && !memberPhotos[link.PhotoID] && confirmedFace[link.PersonID] == "" {
			confirmedFace[link.PersonID] = link.FaceID
		}
	}

	var prompt strings.Builder
	images := []msg.OneShotImage{}
	addImage := func(data []byte) {
		images = append(images, msg.OneShotImage{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(data)})
	}
	fmt.Fprintf(&prompt, "Face group %s: %d faces on %d photos, taken %s to %s.\n\n", group.ID, group.FaceCount, group.PhotoCount,
		dateOf(group.FirstTakenAt), dateOf(group.LastTakenAt))

	if withFaces {
		fmt.Fprintf(&prompt, "Images 1 to %d are faces from the group.\n", min(groupFacesShown, len(group.SampleFaceIDs)))
		for _, faceID := range spread(group.SampleFaceIDs, groupFacesShown) {
			data, err := g.get(g.config.storeURL + "/faces/" + url.PathEscape(faceID) + "/image")
			if err != nil {
				return question{}, err
			}
			addImage(data)
		}
	}
	wholeStart := len(images) + 1
	wholeMembers := spread(byTime, wholePhotosShown)
	for _, member := range wholeMembers {
		data, err := g.boxedPhoto(member)
		if err != nil {
			return question{}, err
		}
		addImage(data)
	}
	fmt.Fprintf(&prompt, "Images %d to %d are whole photos from the group, the person in the red box: ", wholeStart, len(images))
	for index, member := range wholeMembers {
		if index > 0 {
			prompt.WriteString(", ")
		}
		fmt.Fprintf(&prompt, "%s (%s)", member.PhotoID, dateOf(member.TakenAt))
	}
	prompt.WriteString(".\n\nCandidates:\n")
	shownFaces := 0
	operatorListed := false
	for _, personID := range append(ranked, g.operator) {
		if personID == g.operator {
			if operatorListed {
				continue
			}
			operatorListed = true
		}
		known, err := g.personByID(personID)
		if err != nil {
			return question{}, err
		}
		q.candidates[personID] = true
		fmt.Fprintf(&prompt, "- %s, %s", personID, known.DisplayName)
		if personID == g.operator {
			prompt.WriteString(" (the archive's owner; in many photos, often as the one taking them)")
		}
		if known.HowMet != "" {
			fmt.Fprintf(&prompt, "; how the owner knows them: %s", oneLine(known.HowMet, 160))
		}
		if named[personID] > 0 {
			fmt.Fprintf(&prompt, "; the context names them on %d of the %d photos read", named[personID], len(readPhotos))
		}
		if described[personID] > 0 {
			fmt.Fprintf(&prompt, "; the description names them on %d of the %d photos read", described[personID], len(readPhotos))
		}
		if suggested[personID] > 0 {
			fmt.Fprintf(&prompt, "; a face matcher suggested them for %d of the group's faces", suggested[personID])
		}
		if rejected[personID] > 0 {
			fmt.Fprintf(&prompt, "; the owner said %d of these photos do not show them, or that a face on them is not them", rejected[personID])
		}
		if faceID := confirmedFace[personID]; withFaces && faceID != "" && shownFaces < candidatesWithFaces {
			data, err := g.get(g.config.storeURL + "/faces/" + url.PathEscape(faceID) + "/image")
			if err != nil {
				return question{}, err
			}
			addImage(data)
			shownFaces++
			fmt.Fprintf(&prompt, "; image %d is a face confirmed as them", len(images))
		}
		prompt.WriteString("\n")
	}

	prompt.WriteString("\nThe photos and what was said around them:\n")
	unknownPosters := map[string]int{}
	for _, member := range spread(byTime, photosDescribed) {
		photo := g.photos[member.PhotoID]
		message, err := g.messageOf(photo)
		if err != nil {
			return question{}, err
		}
		fmt.Fprintf(&prompt, "- %s, %s, in %s", photo.ID, dateOf(photo.TakenAt), photo.Folder)
		if message.sender != "" {
			fmt.Fprintf(&prompt, "; sent by %s", message.sender)
			if !strings.Contains(message.sender, "person_") {
				unknownPosters[message.sender]++
			}
		}
		if photo.Description != "" {
			fmt.Fprintf(&prompt, "; description: %s", oneLine(photo.Description, 300))
		}
		if message.caption != "" {
			fmt.Fprintf(&prompt, "; caption: %q", oneLine(message.caption, 300))
		}
		if len(message.around) > 0 {
			fmt.Fprintf(&prompt, "; messages around it: %s", strings.Join(message.around, " | "))
		}
		prompt.WriteString("\n")
	}
	if len(unknownPosters) > 0 {
		prompt.WriteString("\nSenders with no person in the owner's contacts: ")
		first := true
		for sender, count := range unknownPosters {
			if !first {
				prompt.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&prompt, "%s (%d photos)", sender, count)
		}
		prompt.WriteString(".\n")
	}
	if withFaces {
		prompt.WriteString("\nWho is the person in this group?")
	} else {
		prompt.WriteString("\nWhich candidate do these records say is the person in the red box, if any?")
	}
	q.prompt, q.images = prompt.String(), images

	var tally []string
	for _, personID := range ranked {
		if named[personID] > 0 {
			tally = append(tally, fmt.Sprintf("[%s](ref:%s)'s chats on %d of %d photos read", g.persons[personID].DisplayName, personID, named[personID], len(readPhotos)))
		}
		if described[personID] > 0 {
			tally = append(tally, fmt.Sprintf("[%s](ref:%s) in the description of %d", g.persons[personID].DisplayName, personID, described[personID]))
		}
		if rejected[personID] > 0 {
			tally = append(tally, fmt.Sprintf("[%s](ref:%s) rejected by the owner on %d", g.persons[personID].DisplayName, personID, rejected[personID]))
		}
	}
	q.tally = strings.Join(tally, "; ")
	return q, nil
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}

func dateOf(epoch int64) string {
	if epoch == 0 {
		return "an unknown date"
	}
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

func (g *guesser) get(target string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := g.web.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d", target, response.StatusCode)
	}
	return body, nil
}

// boxedPhoto is a member's whole photo, shrunk to wholePhotoLongestSide,
// with a red box around the member's face.
func (g *guesser) boxedPhoto(member photostore.FaceGroupMember) ([]byte, error) {
	data, err := g.get(g.config.storeURL + "/photos/" + url.PathEscape(member.PhotoID) + "/thumbnail")
	if err != nil {
		return nil, err
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("thumbnail of %s: %w", member.PhotoID, err)
	}
	var face photostore.Face
	var faces []photostore.Face
	if err := g.web.GetJSON(g.config.storeURL+"/photos/"+url.PathEscape(member.PhotoID)+"/faces", nil, &faces); err != nil {
		return nil, err
	}
	for _, each := range faces {
		if each.ID == member.FaceID {
			face = each
		}
	}
	bounds := decoded.Bounds()
	scale := float64(wholePhotoLongestSide) / float64(max(bounds.Dx(), bounds.Dy()))
	if scale > 1 {
		scale = 1
	}
	width, height := int(float64(bounds.Dx())*scale), int(float64(bounds.Dy())*scale)
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.Set(x, y, decoded.At(bounds.Min.X+int(float64(x)/scale), bounds.Min.Y+int(float64(y)/scale)))
		}
	}
	if face.ID != "" {
		red := image.NewUniform(color.RGBA{255, 0, 0, 255})
		left, top := int(face.Box.Left*float64(width)), int(face.Box.Top*float64(height))
		right, bottom := int(face.Box.Right*float64(width)), int(face.Box.Bottom*float64(height))
		thickness := max(2, width/200)
		for _, edge := range []image.Rectangle{
			image.Rect(left, top, right, top+thickness), image.Rect(left, bottom-thickness, right, bottom),
			image.Rect(left, top, left+thickness, bottom), image.Rect(right-thickness, top, right, bottom),
		} {
			draw.Draw(canvas, edge.Intersect(canvas.Bounds()), red, image.Point{}, draw.Src)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

// ask puts one group's question to one model and turns its answer into a
// guess.
func (g *guesser) ask(model string, q question) (photostore.FaceGroupGuessProposal, error) {
	system := systemPrompt
	if !q.withFaces {
		system = contextOnlySystemPrompt
	}
	body, err := json.Marshal(msg.OneShotRequest{Prompt: q.prompt, SystemPrompt: system, Model: model, Caller: guesserName,
		Schema: json.RawMessage(guessSchema), Images: q.images})
	if err != nil {
		return photostore.FaceGroupGuessProposal{}, err
	}
	request, err := http.NewRequest(http.MethodPost, g.config.bridgeURL+"/instances/"+url.PathEscape(g.instances[model])+"/oneshot", bytes.NewReader(body))
	if err != nil {
		return photostore.FaceGroupGuessProposal{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-LLM-Bridge-Service-Token", g.config.bridgeToken)
	response, err := g.web.Do(request)
	if err != nil {
		return photostore.FaceGroupGuessProposal{}, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return photostore.FaceGroupGuessProposal{}, err
	}
	if response.StatusCode != http.StatusOK {
		return photostore.FaceGroupGuessProposal{}, fmt.Errorf("oneshot on %s: %d %s", g.instances[model], response.StatusCode, bytes.TrimSpace(answer))
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(answer, &reply); err != nil {
		return photostore.FaceGroupGuessProposal{}, fmt.Errorf("oneshot reply: %w", err)
	}
	var parsed struct {
		PersonID   string  `json:"person_id"`
		Confidence float64 `json:"confidence"`
		OnePerson  bool    `json:"one_person"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal(reply.Parsed, &parsed); err != nil {
		return photostore.FaceGroupGuessProposal{}, fmt.Errorf("the answer is not the guess asked for: %w (%s)", err, reply.Parsed)
	}
	evidence := strings.TrimSpace(parsed.Reason)
	if parsed.PersonID != "" && !q.candidates[parsed.PersonID] {
		evidence = fmt.Sprintf("%s\n\nIt named %s, who was not a candidate, so the guess is left empty.", evidence, parsed.PersonID)
		parsed.PersonID = ""
	}
	if !parsed.OnePerson {
		evidence = "**It thinks these faces are more than one person.** " + evidence
	}
	if q.tally != "" {
		evidence += "\n\nContext: " + q.tally + "."
	}
	return photostore.FaceGroupGuessProposal{PersonID: parsed.PersonID, Confidence: min(max(parsed.Confidence, 0), 1),
		Evidence: evidence, ProposedBy: model}, nil
}
