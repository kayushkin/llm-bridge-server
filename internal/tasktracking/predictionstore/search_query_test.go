package predictionstore

import (
	"errors"
	"testing"
)

func TestSearchQueriesSplitWordsAsTheDocumentDoes(t *testing.T) {
	for query, want := range map[string]string{
		"deploy":                 "deploy",
		"deploy.sh":              `"deploy sh"`,
		"-tools/deploy.go":       `-"tools deploy go"`,
		`"bridge.db is locked"`:  `"bridge db is locked"`,
		"prediction_000012 or x": `"prediction 000012" or x`,
		"!!! -":                  "",
		"naïve café":             "naïve café",
	} {
		got, err := searchQueryOf(query)
		if err != nil || got != want {
			t.Errorf("searchQueryOf(%q) = %q, %v; want %q", query, got, err, want)
		}
	}
	if _, err := searchQueryOf(`"unclosed`); !errors.Is(err, ErrInvalidPrediction) {
		t.Errorf("an unclosed quote = %v, want ErrInvalidPrediction", err)
	}
}
