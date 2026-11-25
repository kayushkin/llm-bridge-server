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
// facepasses.MatchPeople does the work; this command reads its flags and
// settings and runs it.
//
//	MULTICHAT_TOKEN=… photo-match-people -operator person_000001 -dry-run
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

const commandName = "photo-match-people"

func main() {
	options := facepasses.DefaultMatchPeopleOptions()
	flag.StringVar(&options.Operator, "operator", options.Operator, "the person_ id whose photos these are, who is in every chat")
	flag.StringVar(&options.SeedPerson, "seed-person", options.SeedPerson, "a person_ id to confirm on -seed-photos")
	seedPhotos := flag.String("seed-photos", "", "comma-separated photo ids the -seed-person said they are in")
	flag.StringVar(&options.SeedDecidedBy, "seed-decided-by", options.SeedDecidedBy, "who said so, recorded as decided_by")
	flag.StringVar(&options.FaceModel, "model", options.FaceModel, "the face model whose faces to match")
	flag.StringVar(&options.PhotoStoreURL, "store-url", options.PhotoStoreURL, "photo-store's base URL")
	flag.BoolVar(&options.DryRun, "dry-run", options.DryRun, "decide and report, write nothing")
	flag.IntVar(&options.ModelCheck, "model-check", options.ModelCheck, "ask a model that can see about up to this many suggestions after matching, confirming or rejecting them; 0 = none")
	flag.StringVar(&options.ModelCheckRole, "model-role", options.ModelCheckRole, "the model-store role the model check asks; its models must see images")
	flag.Parse()
	registry, err := photostore.NewMatchPeopleSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = registry.CheckRequired()
	}
	if err != nil {
		fail(err)
	}
	options.SeedPhotoIDs = strings.Split(*seedPhotos, ",")
	options.MultichatURL = registry.String(photostore.SettingMultichatURL)
	options.MultichatToken = registry.String(photostore.SettingMultichatToken)
	options.JournalStoreURL = registry.String(photostore.SettingJournalStoreURL)
	options.MessageStoreURL = registry.String(photostore.SettingMessageStoreURL)
	options.PeopleStoreURL = registry.String(photostore.SettingPeopleStoreURL)
	options.BridgeURL = registry.String(photostore.SettingBridgeURL)
	options.BridgeServiceToken = registry.String(photostore.SettingBridgeServiceToken)
	options.Output, options.DiagnosticOutput = os.Stdout, os.Stderr
	if err := facepasses.MatchPeople(context.Background(), options); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
	os.Exit(1)
}
