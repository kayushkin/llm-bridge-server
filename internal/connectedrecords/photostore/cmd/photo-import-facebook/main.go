// Command photo-import-facebook imports the photos of a Facebook data export
// (JSON format) into photo-store: the photos in the user's own posts, albums
// and uncategorised photos. A photo one of the user's posts carries is
// cited by that post's journal-store entry (tag facebook-post, found by the
// photo's file name); any other is cited by its path in the export on
// -source-machine, the machine the export sits on. Its date is when it was posted (date_source upload), since the
// export keeps no camera time. Videos and formats the thumbnailer cannot read
// are counted and skipped. Running it again changes nothing that did not
// change.
//
//	photo-import-facebook -activity <export>/your_facebook_activity -source-machine <host> -dry-run
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/importbatch"
	"github.com/kayushkin/photo-store/internal/journalcitation"
)

// postTag is the journal-store tag of the user's imported Facebook posts.
const postTag = "facebook-post"

// The export's files that list photos, under your_facebook_activity.
const (
	postsFile         = "posts/your_posts__check_ins__photos_and_videos_1.json"
	albumsGlob        = "posts/album/*.json"
	uncategorisedFile = "posts/your_uncategorized_photos.json"
)

// media is one photo as the export lists it.
type media struct {
	URI               string `json:"uri"`
	CreationTimestamp int64  `json:"creation_timestamp"`
}

// listed is a photo and the folder it goes in.
type listed struct {
	media
	folder string
}

func main() {
	activity := flag.String("activity", "", "the export's your_facebook_activity folder")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	sourceMachine := flag.String("source-machine", "", "the name of the machine the export sits on, recorded as each photo's source_machine")
	dryRun := flag.Bool("dry-run", false, "read everything and post nothing")
	flag.Parse()
	if *activity == "" || *sourceMachine == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := photostore.NewImportFacebookSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = settings.CheckRequired()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "photo-import-facebook: %v\n", err)
		os.Exit(2)
	}
	journal := journalcitation.Store{Base: settings.String(photostore.SettingJournalStoreURL), Client: &http.Client{Timeout: time.Minute}}
	if err := run(journal, *activity, *sourceMachine, *storeURL, *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "photo-import-facebook: %v\n", err)
		os.Exit(1)
	}
}

func readJSON(file string, into any) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// listPhotos reads every photo the export lists, each once: a post's photo
// first, so it keeps the posts folder.
func listPhotos(activity string) ([]listed, error) {
	var photos []listed
	seen := map[string]bool{}
	keep := func(item media, folder string) {
		if item.URI != "" && !seen[item.URI] {
			seen[item.URI] = true
			photos = append(photos, listed{item, folder})
		}
	}
	var posts []struct {
		Attachments []struct {
			Data []struct {
				Media *media `json:"media"`
			} `json:"data"`
		} `json:"attachments"`
	}
	if err := readJSON(filepath.Join(activity, postsFile), &posts); err != nil {
		return nil, err
	}
	for _, post := range posts {
		for _, attachment := range post.Attachments {
			for _, data := range attachment.Data {
				if data.Media != nil {
					keep(*data.Media, "facebook/posts")
				}
			}
		}
	}
	albums, err := filepath.Glob(filepath.Join(activity, albumsGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(albums)
	for _, file := range albums {
		var album struct {
			Name   string  `json:"name"`
			Photos []media `json:"photos"`
		}
		if err := readJSON(file, &album); err != nil {
			return nil, err
		}
		for _, photo := range album.Photos {
			keep(photo, "facebook/albums/"+repair(album.Name))
		}
	}
	var uncategorised struct {
		Photos []media `json:"other_photos_v2"`
	}
	if err := readJSON(filepath.Join(activity, uncategorisedFile), &uncategorised); err != nil {
		return nil, err
	}
	for _, photo := range uncategorised.Photos {
		keep(photo, "facebook/uncategorised")
	}
	return photos, nil
}

// repair undoes the export's encoding of a name: UTF-8 bytes written as if
// each were a Latin-1 character.
func repair(text string) string {
	raw := make([]byte, 0, len(text))
	for _, character := range text {
		if character > 0xFF {
			return text
		}
		raw = append(raw, byte(character))
	}
	if repaired := string(raw); strings.ToValidUTF8(repaired, "") == repaired {
		return repaired
	}
	return text
}

func mediaTypeOf(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".mp4":
		return "video/mp4"
	default:
		return "image/jpeg"
	}
}

func run(journal journalcitation.Store, activity, sourceMachine, storeURL string, dryRun bool, out io.Writer) error {
	photos, err := listPhotos(activity)
	if err != nil {
		return err
	}
	exportRoot := filepath.Dir(activity)
	in := importbatch.New()
	for _, photo := range photos {
		file := filepath.Join(exportRoot, filepath.FromSlash(photo.URI))
		original, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			in.Report["the export lacks the file"]++
			continue
		}
		if err != nil {
			return err
		}
		sourcePath := file
		post, err := journal.EntryCiting(postTag, path.Base(photo.URI))
		switch err.(type) {
		case nil:
			sourcePath = post.ID
		case journalcitation.ErrNotOne:
		default:
			return err
		}
		in.Add(original, mediaTypeOf(photo.URI), sourceMachine, sourcePath, photo.folder,
			time.Unix(photo.CreationTimestamp, 0), photostore.DateFromUpload)
	}
	return in.Post(storeURL, dryRun, out)
}
