// Package peoplematching decides who is in a photo from two kinds of
// evidence: how close a face is to the faces already confirmed as a person,
// and who the photo's context names (the chat it was sent in, the transcript
// that cites it). It decides only; reading the stores and writing the links
// is the command's job.
package peoplematching

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Thresholds on the cosine similarity of two normalized face embeddings
// (InsightFace buffalo_l). Faces of one person usually score above 0.5 and
// faces of two people below 0.3.
const (
	// ConfirmSimilarity is how close a face must be to a person's confirmed
	// faces to be confirmed, and only when the context names them too.
	ConfirmSimilarity = 0.5
	// SuggestSimilarity is how close a face must be to be suggested.
	SuggestSimilarity = 0.42
	// ClusterSimilarity is how close two faces must be to count as the same
	// person when gathering a person's faces from their chats.
	ClusterSimilarity = 0.5
	// MinimumMargin is how far the best person must lead the next one.
	MinimumMargin = 0.08
	// ClusterMinimumPhotos is how many of a person's chat photos must share a
	// face before it is suggested as theirs.
	ClusterMinimumPhotos = 3
)

// Face is a face with its normalized embedding.
type Face struct {
	ID        string
	PhotoID   string
	Embedding []float32
}

// DecodeEmbedding reads photo-store's base64 little-endian float32s.
func DecodeEmbedding(encoded string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw)%4 != 0 || len(raw) == 0 {
		return nil, fmt.Errorf("not a float32 embedding")
	}
	values := make([]float32, len(raw)/4)
	for index := range values {
		values[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:]))
	}
	return values, nil
}

// Similarity is the cosine similarity of two normalized embeddings.
func Similarity(a, b []float32) float64 {
	var sum float64
	for index := range a {
		sum += float64(a[index]) * float64(b[index])
	}
	return sum
}

// PersonScore is how close a face is to one person's confirmed faces: the
// mean of its three closest, or fewer when fewer are known.
type PersonScore struct {
	PersonID   string
	Similarity float64
	References int
}

// ScorePeople scores a face against every person with confirmed faces, best
// first. A reference that is this very face is skipped.
func ScorePeople(face Face, references map[string][]Face) []PersonScore {
	scores := []PersonScore{}
	for personID, faces := range references {
		var similarities []float64
		for _, reference := range faces {
			if reference.ID == face.ID {
				continue
			}
			similarities = append(similarities, Similarity(face.Embedding, reference.Embedding))
		}
		if len(similarities) == 0 {
			continue
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(similarities)))
		top := similarities[:min(3, len(similarities))]
		var sum float64
		for _, value := range top {
			sum += value
		}
		scores = append(scores, PersonScore{PersonID: personID, Similarity: sum / float64(len(top)), References: len(faces)})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].Similarity > scores[j].Similarity })
	return scores
}

// Verdict is what to propose for one face.
type Verdict struct {
	PersonID   string
	Status     string // "confirmed", "suggested" or "" for nothing
	Similarity float64
	InContext  bool
}

// Decide turns a face's scores and its photo's context into a verdict:
// confirmed when the face is close to a person's confirmed faces, clearly
// closer than to anyone else, and the context names that person; suggested
// when it is close enough to be worth a look; otherwise nothing.
func Decide(scores []PersonScore, context map[string]bool) Verdict {
	if len(scores) == 0 {
		return Verdict{}
	}
	best := scores[0]
	margin := best.Similarity
	if len(scores) > 1 {
		margin = best.Similarity - scores[1].Similarity
	}
	verdict := Verdict{PersonID: best.PersonID, Similarity: best.Similarity, InContext: context[best.PersonID]}
	switch {
	case best.Similarity >= ConfirmSimilarity && margin >= MinimumMargin && verdict.InContext:
		verdict.Status = "confirmed"
	case best.Similarity >= SuggestSimilarity && margin >= MinimumMargin:
		verdict.Status = "suggested"
	default:
		verdict.Status = ""
	}
	return verdict
}

// ClusterInContext finds the face that recurs most across a person's chat
// photos — in a one-to-one chat, the face that is not the operator's is
// most often the other person's. It answers that face and its look-alikes,
// at most one per photo, or nothing when no face recurs on at least
// ClusterMinimumPhotos photos.
func ClusterInContext(faces []Face) []Face {
	var bestCluster []Face
	for _, center := range faces {
		closestPerPhoto := map[string]Face{}
		closestSimilarity := map[string]float64{}
		for _, other := range faces {
			similarity := 1.0
			if other.ID != center.ID {
				similarity = Similarity(center.Embedding, other.Embedding)
			}
			if similarity < ClusterSimilarity {
				continue
			}
			if previous, seen := closestSimilarity[other.PhotoID]; !seen || similarity > previous {
				closestSimilarity[other.PhotoID] = similarity
				closestPerPhoto[other.PhotoID] = other
			}
		}
		if len(closestPerPhoto) > len(bestCluster) {
			bestCluster = bestCluster[:0]
			for _, face := range closestPerPhoto {
				bestCluster = append(bestCluster, face)
			}
		}
	}
	if len(bestCluster) < ClusterMinimumPhotos {
		return nil
	}
	sort.Slice(bestCluster, func(i, j int) bool { return bestCluster[i].ID < bestCluster[j].ID })
	return bestCluster
}

// SeedFaces picks, from photos a person confirmed they are in, the face on
// each that is them: the face most like the faces on the other photos.
func SeedFaces(facesByPhoto map[string][]Face) []Face {
	var seeds []Face
	for photoID, faces := range facesByPhoto {
		var best Face
		bestScore := math.Inf(-1)
		for _, face := range faces {
			var score float64
			for otherPhotoID, others := range facesByPhoto {
				if otherPhotoID == photoID {
					continue
				}
				closest := math.Inf(-1)
				for _, other := range others {
					closest = math.Max(closest, Similarity(face.Embedding, other.Embedding))
				}
				if !math.IsInf(closest, -1) {
					score += closest
				}
			}
			if score > bestScore {
				best, bestScore = face, score
			}
		}
		if best.ID != "" {
			seeds = append(seeds, best)
		}
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].ID < seeds[j].ID })
	return seeds
}
