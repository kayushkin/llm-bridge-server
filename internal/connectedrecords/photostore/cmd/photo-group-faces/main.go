// Command photo-group-faces gathers the faces nobody has named into face
// groups, each one person, so a person names a whole group at once. A face
// already confirmed as someone is left out, and so is a face suggested as a
// person who has confirmed faces, whom photo-match-people and its model
// check settle; a face suggested only as someone with no confirmed face yet
// is grouped, since that suggestion is the guess a group exists to check.
// Free faces first join the open groups they clearly belong to; the rest
// form new groups when they span enough photos. internal/facegrouping
// decides; this command reads photo-store and writes the groups.
//
//	photo-group-faces -dry-run
//	photo-group-faces -evaluate
//
// -evaluate writes nothing: it groups the confirmed faces with the free ones
// and reports how often a group mixes two confirmed people, at several
// thresholds, so a threshold is chosen on measurement.
package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/facegrouping"
	"github.com/kayushkin/photo-store/internal/peoplematching"
	"github.com/kayushkin/photo-store/internal/photocontext"
)

const grouperName = "photo-group-faces"

// Faces smaller than this share of the thumbnail's width, or found with less
// certainty, are too unsure to group.
const (
	minimumFaceWidth      = 0.035
	minimumDetectionScore = 0.65
)

func main() {
	model := flag.String("model", "insightface-buffalo_l", "the face model whose faces to group")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	dryRun := flag.Bool("dry-run", false, "decide and report, write nothing")
	evaluate := flag.Bool("evaluate", false, "measure how pure groups are against the confirmed faces, write nothing")
	joinSimilarity := flag.Float64("join-similarity", facegrouping.DefaultSettings.JoinSimilarity, "how close a face must be to a group's average face to join it")
	flag.Parse()
	web := photocontext.Client{HTTP: &http.Client{Timeout: 2 * time.Minute}}
	settings := facegrouping.DefaultSettings
	settings.JoinSimilarity = *joinSimilarity
	if err := run(web, strings.TrimRight(*storeURL, "/"), *model, settings, *dryRun, *evaluate); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", grouperName, err)
		os.Exit(1)
	}
}

func loadFaces(web photocontext.Client, storeURL, model string) ([]facegrouping.Face, error) {
	var faces []facegrouping.Face
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
			faces = append(faces, facegrouping.Face{Face: peoplematching.Face{ID: face.ID, PhotoID: face.PhotoID, Embedding: embedding}, DetectionScore: face.DetectionScore})
		}
		if len(page) < 5000 {
			return faces, nil
		}
		after = page[len(page)-1].ID
	}
}

// loadGroups reads every group with its members, whatever its status.
func loadGroups(web photocontext.Client, storeURL string) ([]photostore.FaceGroup, error) {
	var groups []photostore.FaceGroup
	for offset := 0; ; offset += 1000 {
		var page []photostore.FaceGroup
		if err := web.GetJSON(fmt.Sprintf("%s/face-groups?limit=1000&offset=%d", storeURL, offset), nil, &page); err != nil {
			return nil, fmt.Errorf("face groups: %w", err)
		}
		for _, group := range page {
			var full photostore.FaceGroup
			if err := web.GetJSON(storeURL+"/face-groups/"+url.PathEscape(group.ID), nil, &full); err != nil {
				return nil, fmt.Errorf("face group %s: %w", group.ID, err)
			}
			groups = append(groups, full)
		}
		if len(page) < 1000 {
			return groups, nil
		}
	}
}

func run(web photocontext.Client, storeURL, model string, settings facegrouping.Settings, dryRun, evaluate bool) error {
	faces, err := loadFaces(web, storeURL, model)
	if err != nil {
		return err
	}
	links, err := photocontext.LoadLinks(web, storeURL)
	if err != nil {
		return err
	}
	confirmedAs := map[string]string{} // face id → person id
	peopleWithConfirmedFaces := map[string]bool{}
	for _, link := range links {
		if link.Status == photostore.PhotoPersonConfirmed && link.FaceID != "" {
			confirmedAs[link.FaceID] = link.PersonID
			peopleWithConfirmedFaces[link.PersonID] = true
		}
	}
	settledByMatcher := map[string]bool{}
	for _, link := range links {
		if link.Status == photostore.PhotoPersonSuggested && link.FaceID != "" && peopleWithConfirmedFaces[link.PersonID] {
			settledByMatcher[link.FaceID] = true
		}
	}
	if evaluate {
		return evaluatePurity(faces, confirmedAs)
	}

	groups, err := loadGroups(web, storeURL)
	if err != nil {
		return err
	}
	faceByID := map[string]facegrouping.Face{}
	for _, face := range faces {
		faceByID[face.ID] = face
	}
	grouped := map[string]bool{}
	open := []facegrouping.Group{}
	for _, group := range groups {
		if group.Model != model {
			continue
		}
		each := facegrouping.Group{ID: group.ID}
		for _, member := range group.Members {
			grouped[member.FaceID] = true
			if face, known := faceByID[member.FaceID]; known {
				each.Members = append(each.Members, facegrouping.Member{Face: face, Similarity: member.Similarity})
			}
		}
		if group.Status == photostore.FaceGroupOpen && len(each.Members) > 0 {
			open = append(open, each)
		}
	}
	free := []facegrouping.Face{}
	for _, face := range faces {
		if !grouped[face.ID] && confirmedAs[face.ID] == "" && !settledByMatcher[face.ID] {
			free = append(free, face)
		}
	}
	result := facegrouping.Assign(free, open, settings)
	joined := 0
	for _, members := range result.Joined {
		joined += len(members)
	}
	created := 0
	for _, group := range result.Created {
		created += len(group.Members)
	}
	mode := "wrote"
	if dryRun {
		mode = "would write"
	}
	if !dryRun {
		for groupID, members := range result.Joined {
			addition := photostore.FaceGroupMembersAddition{}
			for _, member := range members {
				addition.Members = append(addition.Members, photostore.NewFaceGroupMember{FaceID: member.Face.ID, Similarity: member.Similarity})
			}
			if err := postDroppingRetiredFaces(web, storeURL+"/face-groups/"+url.PathEscape(groupID)+"/members", &addition.Members, func() any { return addition }, http.StatusOK); err != nil {
				return err
			}
		}
		for _, group := range result.Created {
			body := photostore.NewFaceGroup{Model: model, MadeBy: grouperName}
			for _, member := range group.Members {
				body.Members = append(body.Members, photostore.NewFaceGroupMember{FaceID: member.Face.ID, Similarity: member.Similarity})
			}
			if err := postDroppingRetiredFaces(web, storeURL+"/face-groups", &body.Members, func() any { return body }, http.StatusCreated); err != nil {
				return err
			}
		}
	}
	fmt.Printf("%d faces; %d free to group; %s %d faces into %d open groups and %d new groups of %d faces\n",
		len(faces), len(free), mode, joined, len(result.Joined), len(result.Created), created)
	if dryRun {
		return nil
	}
	return guessFromDecidedGroups(web, storeURL, model, faceByID, settings.MergeSimilarity)
}

// average is the normalized mean of the faces of a group that this run
// loaded; nil when none was.
func average(group photostore.FaceGroup, faceByID map[string]facegrouping.Face) []float32 {
	var sum []float64
	for _, member := range group.Members {
		face, known := faceByID[member.FaceID]
		if !known {
			continue
		}
		if sum == nil {
			sum = make([]float64, len(face.Embedding))
		}
		for index, value := range face.Embedding {
			sum[index] += float64(value)
		}
	}
	if sum == nil {
		return nil
	}
	var squares float64
	for _, value := range sum {
		squares += value * value
	}
	averaged := make([]float32, len(sum))
	for index, value := range sum {
		averaged[index] = float32(value / math.Sqrt(squares))
	}
	return averaged
}

// guessFromDecidedGroups compares each open group's average face with the
// groups a person already decided, and when one reaches threshold, guesses
// the open group is the same: the person it was named as, or someone not
// known. A person still decides; the guess only puts the answer first. A
// group is guessed again only after it gains faces.
func guessFromDecidedGroups(web photocontext.Client, storeURL, model string, faceByID map[string]facegrouping.Face, threshold float64) error {
	groups, err := loadGroups(web, storeURL)
	if err != nil {
		return err
	}
	type decided struct {
		group   photostore.FaceGroup
		average []float32
	}
	decidedGroups := []decided{}
	for _, group := range groups {
		if group.Model == model && (group.Status == photostore.FaceGroupNamed || group.Status == photostore.FaceGroupNotKnown) {
			if each := average(group, faceByID); each != nil {
				decidedGroups = append(decidedGroups, decided{group, each})
			}
		}
	}
	written := 0
	for _, group := range groups {
		if group.Model != model || group.Status != photostore.FaceGroupOpen {
			continue
		}
		alreadyGuessed := false
		for _, guess := range group.Guesses {
			if guess.ProposedBy == grouperName && guess.CreatedAt >= group.UpdatedAt {
				alreadyGuessed = true
			}
		}
		openAverage := average(group, faceByID)
		if alreadyGuessed || openAverage == nil {
			continue
		}
		best, bestSimilarity := decided{}, threshold
		for _, each := range decidedGroups {
			if similarity := peoplematching.Similarity(openAverage, each.average); similarity >= bestSimilarity {
				best, bestSimilarity = each, similarity
			}
		}
		if best.group.ID == "" {
			continue
		}
		guess := photostore.FaceGroupGuessProposal{Confidence: 0.9, ProposedBy: grouperName}
		if best.group.Status == photostore.FaceGroupNamed {
			guess.PersonID = best.group.PersonID
			guess.Evidence = fmt.Sprintf("Its average face matches [a group](ref:%s) named as [them](ref:%s), at %.2f.", best.group.ID, best.group.PersonID, bestSimilarity)
		} else {
			guess.Evidence = fmt.Sprintf("Its average face matches [a group](ref:%s) marked as someone not known, at %.2f.", best.group.ID, bestSimilarity)
		}
		status, body, err := web.PostJSON(storeURL+"/face-groups/"+url.PathEscape(group.ID)+"/guesses", guess)
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("POST guess on %s: %d %s", group.ID, status, strings.TrimSpace(string(body)))
		}
		written++
	}
	fmt.Printf("guessed %d open groups from the groups already decided\n", written)
	return nil
}

var retiredFace = regexp.MustCompile(`(face_[0-9]+) was retired`)

// postDroppingRetiredFaces posts a body of faces; when photo-store refuses
// one because an import or rescan retired it after this run read the faces,
// it drops that face and posts again. A body left with no face is skipped.
func postDroppingRetiredFaces(web photocontext.Client, target string, members *[]photostore.NewFaceGroupMember, payload func() any, want int) error {
	for {
		if len(*members) == 0 {
			return nil
		}
		status, body, err := web.PostJSON(target, payload())
		if err != nil {
			return err
		}
		if status == want {
			return nil
		}
		match := retiredFace.FindSubmatch(body)
		if status != http.StatusBadRequest || match == nil {
			return fmt.Errorf("POST %s: %d %s", target, status, strings.TrimSpace(string(body)))
		}
		kept := (*members)[:0]
		for _, member := range *members {
			if member.FaceID != string(match[1]) {
				kept = append(kept, member)
			}
		}
		*members = kept
	}
}

// evaluatePurity groups every face at several thresholds and reports, among
// the groups holding confirmed faces, how many mix two confirmed people, and
// how many confirmed faces sit in a group whose most common person is
// someone else.
func evaluatePurity(faces []facegrouping.Face, confirmedAs map[string]string) error {
	for _, merge := range []float64{1.01, 0.55, 0.5, 0.45} {
		settings := facegrouping.DefaultSettings
		settings.MergeSimilarity = merge
		result := facegrouping.Assign(faces, nil, settings)
		labeled, mixed, misplaced, labeledFaces, groupedFaces, unlabeledGroups := 0, 0, 0, 0, 0, 0
		groupsOf := map[string]int{}
		for _, group := range result.Created {
			groupedFaces += len(group.Members)
			counts := map[string]int{}
			for _, member := range group.Members {
				if person := confirmedAs[member.Face.ID]; person != "" {
					counts[person]++
				}
			}
			if len(counts) == 0 {
				unlabeledGroups++
				continue
			}
			labeled++
			if len(counts) > 1 {
				mixed++
			}
			most, mostPerson, inGroup := 0, "", 0
			for person, count := range counts {
				inGroup += count
				if count > most {
					most, mostPerson = count, person
				}
			}
			labeledFaces += inGroup
			misplaced += inGroup - most
			groupsOf[mostPerson]++
		}
		mergeName := fmt.Sprintf("merge %.2f", merge)
		if merge > 1 {
			mergeName = "no merge"
		}
		fmt.Printf("%s: %d groups (%d faces); %d hold confirmed faces of %d people (%.1f groups a person), %d mix two people, %d confirmed faces sit with someone else; %d groups hold no confirmed face\n",
			mergeName, len(result.Created), groupedFaces, labeled, len(groupsOf), float64(labeled)/float64(max(len(groupsOf), 1)), mixed, misplaced, unlabeledGroups)
	}
	return nil
}
