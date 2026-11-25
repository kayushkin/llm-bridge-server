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
// model's last guess. facepasses.GuessFaceGroups does the work; this
// command reads its flags and settings and runs it.
//
//	LLMBRIDGE_SERVICE_TOKEN=… MULTICHAT_TOKEN=… photo-guess-face-groups -operator person_000001 -limit 20
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kayushkin/llm-bridge/servicesettings"
	photostore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore/facepasses"
)

const commandName = "photo-guess-face-groups"

func main() {
	options := facepasses.DefaultGuessFaceGroupsOptions()
	flag.StringVar(&options.Operator, "operator", options.Operator, "the person_ id whose photos these are")
	models := flag.String("models", strings.Join(options.Models, ","), "comma-separated model-store ids to ask, each on its own")
	contextOnlyModels := flag.String("context-only-models", strings.Join(options.ContextOnlyModels, ","), "comma-separated models of -models asked to judge from the records and whole photos, with no faces to compare")
	flag.BoolVar(&options.AskAgain, "ask-again", options.AskAgain, "ask about groups a model already answered, as well")
	flag.IntVar(&options.Limit, "limit", options.Limit, "ask about at most this many groups, the most photos first")
	flag.StringVar(&options.GroupID, "group", options.GroupID, "ask about this one face group only")
	flag.StringVar(&options.PhotoStoreURL, "store-url", options.PhotoStoreURL, "photo-store's base URL")
	flag.BoolVar(&options.DryRun, "dry-run", options.DryRun, "ask and print, write nothing")
	flag.Parse()
	registry, err := photostore.NewGuessFaceGroupsSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = registry.CheckRequired()
	}
	if err != nil {
		fail(err)
	}
	options.Models = facepasses.SplitModelList(*models)
	options.ContextOnlyModels = facepasses.SplitModelList(*contextOnlyModels)
	options.BridgeURL = registry.String(photostore.SettingBridgeURL)
	options.BridgeServiceToken = registry.String(photostore.SettingBridgeServiceToken)
	options.ModelStoreURL = registry.String(photostore.SettingModelStoreURL)
	options.PeopleStoreURL = registry.String(photostore.SettingPeopleStoreURL)
	options.MessageStoreURL = registry.String(photostore.SettingMessageStoreURL)
	options.MultichatURL = registry.String(photostore.SettingMultichatURL)
	options.MultichatToken = registry.String(photostore.SettingMultichatToken)
	options.JournalStoreURL = registry.String(photostore.SettingJournalStoreURL)
	options.Output, options.DiagnosticOutput = os.Stdout, os.Stderr
	if err := facepasses.GuessFaceGroups(context.Background(), options); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
	os.Exit(1)
}
