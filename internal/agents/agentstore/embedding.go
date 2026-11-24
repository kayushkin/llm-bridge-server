package agentstore

import (
	"math"
	"strings"
	"unicode"
)

// Embedder generates simple TF-IDF style embeddings for text.
type Embedder struct{}

// NewEmbedder creates a new embedder.
func NewEmbedder() *Embedder { return &Embedder{} }

const vectorSize = 256

// Embed generates a bag-of-words embedding vector from text.
func (e *Embedder) Embed(text string) []float64 {
	tokens := tokenize(text)

	tf := make(map[string]float64)
	for _, token := range tokens {
		tf[token]++
	}
	total := float64(len(tokens))
	if total > 0 {
		for t := range tf {
			tf[t] /= total
		}
	}

	vector := make([]float64, vectorSize)
	for token, freq := range tf {
		bucket := hashString(token) % vectorSize
		vector[bucket] += freq
	}

	// L2 normalize
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vector {
			vector[i] /= norm
		}
	}
	return vector
}

// CosineSimilarity computes cosine similarity between two vectors.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var cur strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 2 {
				t := cur.String()
				if !isStopWord(t) {
					tokens = append(tokens, t)
				}
			}
			cur.Reset()
		}
	}
	if cur.Len() > 2 {
		t := cur.String()
		if !isStopWord(t) {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func hashString(s string) int {
	h := 0
	for i := 0; i < len(s); i++ {
		h = 31*h + int(s[i])
	}
	if h < 0 {
		h = -h
	}
	return h
}

func isStopWord(token string) bool {
	switch token {
	case "the", "and", "for", "are", "but", "not", "you", "all",
		"can", "her", "was", "one", "our", "out", "has", "had",
		"have", "this", "that", "from", "with", "they", "been",
		"will", "into", "more", "than", "what", "when", "where",
		"who", "which":
		return true
	}
	return false
}
