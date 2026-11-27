// Command photo-group-faces gathers the faces nobody has named into face
// groups, each one person, so a person names a whole group at once. A face
// already confirmed as someone is left out, and so is a face suggested as a
// person who has confirmed faces, whom photo-match-people and its model
// check settle; a face suggested only as someone with no confirmed face yet
// is grouped, since that suggestion is the guess a group exists to check.
// Free faces first join the open groups they clearly belong to; the rest
// form new groups when they span enough photos. internal/facegrouping
// decides; facepasses.GroupFaces reads photo-store and writes the groups,
// and this command runs it.
//
//	photo-group-faces -dry-run
//	photo-group-faces -evaluate
//
// -evaluate writes nothing: it groups the confirmed faces with the free ones
// and reports how often a group mixes two confirmed people, at several
// thresholds, so a threshold is chosen on measurement.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos/facepasses"
)

const commandName = "photo-group-faces"

func main() {
	options := facepasses.DefaultGroupFacesOptions()
	flag.StringVar(&options.FaceModel, "model", options.FaceModel, "the face model whose faces to group")
	flag.StringVar(&options.PhotoStoreURL, "store-url", options.PhotoStoreURL, "photo-store's base URL")
	flag.BoolVar(&options.DryRun, "dry-run", options.DryRun, "decide and report, write nothing")
	flag.BoolVar(&options.Evaluate, "evaluate", options.Evaluate, "measure how pure groups are against the confirmed faces, write nothing")
	flag.Float64Var(&options.JoinSimilarity, "join-similarity", options.JoinSimilarity, "how close a face must be to a group's average face to join it")
	flag.Parse()
	options.Output, options.DiagnosticOutput = os.Stdout, os.Stderr
	if err := facepasses.GroupFaces(context.Background(), options); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
		os.Exit(1)
	}
}
