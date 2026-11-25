// Package facegrouping gathers faces nobody has named into groups that are
// each one person, so a person can name a whole group at once. It decides
// only; reading the store and writing the groups is the command's job.
//
// A face joins the group whose average face it is closest to, never the
// group of whichever single face it happens to resemble: linking face to
// nearest face chains one person into the next through a few in-between
// faces, and in a first measurement on real photos that put two people in
// one group of thousands. Two faces on one photo are two people, so a group
// never takes a second face from a photo it already holds.
package facegrouping

import (
	"container/heap"
	"math"
	"sort"

	"github.com/kayushkin/photo-store/internal/peoplematching"
)

// Settings tune the grouping. DefaultSettings holds the values in use.
type Settings struct {
	// JoinSimilarity is how close a face must be to a group's average face
	// to join it.
	JoinSimilarity float64
	// MinimumMargin is how far the best open group must lead the next one
	// for a face to join an existing group.
	MinimumMargin float64
	// MinimumPhotos is the fewest photos a new group must span to be kept;
	// a face seen once or twice waits for more.
	MinimumPhotos int
	// MergeSimilarity is how close two groups' average faces must be for
	// them to merge, when they share no photo.
	MergeSimilarity float64
}

// DefaultSettings: buffalo_l faces of one person usually score above 0.5
// against each other and of two people below 0.3; against an average face,
// which is cleaner than any one face, one person's score higher still.
//
// MergeSimilarity was measured on 2026-11-06 against 165 groups a person had
// named: at 0.5, averages joined 59 of 188 pairs of groups that were one
// person and no pair that was two, once two people recorded twice in the
// contacts were counted as one; at 0.45 a pair of two people joined.
var DefaultSettings = Settings{JoinSimilarity: 0.5, MinimumMargin: 0.05, MinimumPhotos: 3, MergeSimilarity: 0.5}

// Face is a face to group, with how sure the finder was it is a face.
type Face struct {
	peoplematching.Face
	DetectionScore float64
}

// Group is a set of faces, with each face's similarity to its average.
type Group struct {
	// ID is the store's face_group id; "" for a group made here.
	ID      string
	Members []Member
}

// Member is one face of a group.
type Member struct {
	Face       Face
	Similarity float64
}

// Result is what Assign decided.
type Result struct {
	// Joined maps an existing group's id to the faces that join it.
	Joined map[string][]Member
	// Created are new groups, the most photos first.
	Created []Group
}

type cluster struct {
	merged bool
	id     string
	sum    []float64
	photos map[string]bool
	faces  []Face
}

func newCluster(id string, dimensions int) *cluster {
	return &cluster{id: id, sum: make([]float64, dimensions), photos: map[string]bool{}}
}

func (c *cluster) add(face Face) {
	for index, value := range face.Embedding {
		c.sum[index] += float64(value)
	}
	c.photos[face.PhotoID] = true
	c.faces = append(c.faces, face)
}

// average is the normalized mean of the cluster's faces.
func (c *cluster) average() []float32 {
	var squares float64
	for _, value := range c.sum {
		squares += value * value
	}
	length := math.Sqrt(squares)
	averaged := make([]float32, len(c.sum))
	if length == 0 {
		return averaged
	}
	for index, value := range c.sum {
		averaged[index] = float32(value / length)
	}
	return averaged
}

// Assign places free faces: first into the open groups they clearly belong
// to, then the rest into new groups of their own, keeping only new groups
// that span settings.MinimumPhotos photos.
func Assign(free []Face, open []Group, settings Settings) Result {
	result := Result{Joined: map[string][]Member{}}
	if len(free) == 0 {
		return result
	}
	dimensions := len(free[0].Embedding)
	existing := make([]*cluster, 0, len(open))
	for _, group := range open {
		each := newCluster(group.ID, dimensions)
		for _, member := range group.Members {
			each.add(member.Face)
		}
		existing = append(existing, each)
	}
	averages := make([][]float32, len(existing))
	for index, each := range existing {
		averages[index] = each.average()
	}
	// Surest faces first, so a group's average forms from clear faces.
	ordered := append([]Face(nil), free...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].DetectionScore > ordered[j].DetectionScore })

	left := []Face{}
	joinedFaces := map[int][]Face{}
	for _, face := range ordered {
		best, second, bestIndex := -1.0, -1.0, -1
		for index, each := range existing {
			if each.photos[face.PhotoID] {
				continue
			}
			similarity := peoplematching.Similarity(face.Embedding, averages[index])
			if similarity > best {
				best, second, bestIndex = similarity, best, index
			} else if similarity > second {
				second = similarity
			}
		}
		if bestIndex >= 0 && best >= settings.JoinSimilarity && best-second >= settings.MinimumMargin {
			existing[bestIndex].photos[face.PhotoID] = true
			result.Joined[existing[bestIndex].id] = append(result.Joined[existing[bestIndex].id], Member{Face: face, Similarity: best})
			joinedFaces[bestIndex] = append(joinedFaces[bestIndex], face)
			continue
		}
		left = append(left, face)
	}

	// New groups: each face joins the closest new group's average when close
	// enough, or starts one. A second pass moves each face to the average it
	// is now closest to, since the averages settled after it was placed.
	clusters := []*cluster{}
	place := func(faces []Face, clusters []*cluster, averages [][]float32) []*cluster {
		for _, face := range faces {
			best, bestIndex := -1.0, -1
			for index, each := range clusters {
				if each.photos[face.PhotoID] {
					continue
				}
				average := averages[index]
				if average == nil {
					average = each.average()
				}
				if similarity := peoplematching.Similarity(face.Embedding, average); similarity > best {
					best, bestIndex = similarity, index
				}
			}
			if bestIndex >= 0 && best >= settings.JoinSimilarity {
				clusters[bestIndex].add(face)
				continue
			}
			fresh := newCluster("", dimensions)
			fresh.add(face)
			clusters = append(clusters, fresh)
			averages = append(averages, nil)
		}
		return clusters
	}
	clusters = place(left, clusters, make([][]float32, 0))
	settled := make([][]float32, len(clusters))
	for index, each := range clusters {
		settled[index] = each.average()
	}
	second := make([]*cluster, len(clusters))
	for index := range clusters {
		second[index] = newCluster("", dimensions)
	}
	secondAverages := make([][]float32, len(clusters))
	copy(secondAverages, settled)
	second = place(left, second, secondAverages)

	for index, faces := range joinedFaces {
		for _, face := range faces {
			existing[index].sum = addTo(existing[index].sum, face.Embedding)
			existing[index].faces = append(existing[index].faces, face)
		}
	}
	fresh := []*cluster{}
	for _, each := range second {
		if len(each.faces) > 0 {
			fresh = append(fresh, each)
		}
	}
	for existingIndex, faces := range mergeClusters(existing, fresh, settings.MergeSimilarity) {
		id := existing[existingIndex].id
		average := existing[existingIndex].average()
		for _, face := range faces {
			result.Joined[id] = append(result.Joined[id], Member{Face: face, Similarity: peoplematching.Similarity(face.Embedding, average)})
		}
	}

	for _, each := range fresh {
		if each.merged || len(each.photos) < settings.MinimumPhotos {
			continue
		}
		average := each.average()
		group := Group{}
		for _, face := range each.faces {
			group.Members = append(group.Members, Member{Face: face, Similarity: peoplematching.Similarity(face.Embedding, average)})
		}
		sort.SliceStable(group.Members, func(i, j int) bool { return group.Members[i].Similarity > group.Members[j].Similarity })
		result.Created = append(result.Created, group)
	}
	sort.SliceStable(result.Created, func(i, j int) bool { return len(result.Created[i].Members) > len(result.Created[j].Members) })
	return result
}

func addTo(sum []float64, embedding []float32) []float64 {
	for index, value := range embedding {
		sum[index] += float64(value)
	}
	return sum
}

func sharePhoto(a, b *cluster) bool {
	if len(a.photos) > len(b.photos) {
		a, b = b, a
	}
	for photo := range a.photos {
		if b.photos[photo] {
			return true
		}
	}
	return false
}

// mergeClusters merges, closest pair first, clusters whose averages reach
// threshold and share no photo: two faces on one photo are two people. The
// same person often forms several clusters — another year, another light —
// whose averages still match better than two people's do. A new cluster
// merges into an open group (fresh into existing); two open groups never
// merge, since the store keeps them apart. It marks each fresh cluster that
// merged away, and answers the faces each open group took.
func mergeClusters(existing, fresh []*cluster, threshold float64) map[int][]Face {
	all := append(append([]*cluster{}, existing...), fresh...)
	isExisting := func(index int) bool { return index < len(existing) }
	averages := make([][]float32, len(all))
	for index, each := range all {
		averages[index] = each.average()
	}
	version := make([]int, len(all))
	alive := make([]bool, len(all))
	for index := range alive {
		alive[index] = true
	}
	candidates := &pairHeap{}
	offer := func(a, b int) {
		if a > b {
			a, b = b, a
		}
		if isExisting(a) && isExisting(b) {
			return
		}
		if similarity := peoplematching.Similarity(averages[a], averages[b]); similarity >= threshold {
			heap.Push(candidates, pair{a: a, b: b, similarity: similarity, versionA: version[a], versionB: version[b]})
		}
	}
	for a := range all {
		for b := a + 1; b < len(all); b++ {
			offer(a, b)
		}
	}
	taken := map[int][]Face{}
	for candidates.Len() > 0 {
		next := heap.Pop(candidates).(pair)
		if !alive[next.a] || !alive[next.b] || version[next.a] != next.versionA || version[next.b] != next.versionB || sharePhoto(all[next.a], all[next.b]) {
			continue
		}
		// a < b, so an open group, listed first, is always a.
		target, source := all[next.a], all[next.b]
		for _, face := range source.faces {
			target.sum = addTo(target.sum, face.Embedding)
			target.photos[face.PhotoID] = true
			target.faces = append(target.faces, face)
			if isExisting(next.a) {
				taken[next.a] = append(taken[next.a], face)
			}
		}
		source.merged, alive[next.b] = true, false
		averages[next.a] = target.average()
		version[next.a]++
		for other := range all {
			if other != next.a && alive[other] {
				offer(next.a, other)
			}
		}
	}
	return taken
}

// pair is two clusters that may merge, with the versions of each it was
// measured at: a merge changes a cluster's average, and a pair measured
// before that is stale.
type pair struct {
	a, b               int
	similarity         float64
	versionA, versionB int
}

// pairHeap pops the closest pair first.
type pairHeap []pair

func (h pairHeap) Len() int           { return len(h) }
func (h pairHeap) Less(i, j int) bool { return h[i].similarity > h[j].similarity }
func (h pairHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pairHeap) Push(x any)        { *h = append(*h, x.(pair)) }
func (h *pairHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
