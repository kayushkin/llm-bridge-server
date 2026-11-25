// Command photo-import-messages imports the photos people sent in the user's
// chats into photo-store: the images in multichat's message log from rooms of
// at most -max-members members (never Discord server channels, whose names
// start with #), with -discord-archive the image attachments
// multichat-fetch-discord-history saved from the user's own servers, with
// -discord-direct-messages the ones it saved from the user's DMs, and with
// -facebook-messages the photos in a Facebook export's Messenger chats of at
// most -max-members people. -room imports one named multichat room whatever
// its size, Discord server channels included, for a room the user chose; a
// Discord channel's photos go under discord/<server>/<channel>, as archive
// images do. A DM or Messenger photo is cited by the
// journal-store transcript of the day it was sent, found by its Discord
// message id or file name; -chats=false skips multichat's own chats. Each photo's
// source is the message it came in (its message_ ref), and its date is when
// it was sent (date_source message). Running it again changes nothing that
// did not change.
//
//	MULTICHAT_TOKEN=… photo-import-messages -max-members 8 -discord-archive <archive>/discord -dry-run
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/importbatch"
	"github.com/kayushkin/photo-store/internal/journalcitation"
	"github.com/kayushkin/photo-store/internal/messagecitation"
)

type conversation struct {
	RoomID      string `json:"room_id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	MemberCount int    `json:"member_count"`
}

type loggedMessage struct {
	Ref              string    `json:"ref"`
	RoomName         string    `json:"room_name"`
	Platform         string    `json:"platform"`
	NetworkName      string    `json:"network_name"`
	ChannelName      string    `json:"channel_name"`
	DiscordMessageID string    `json:"discord_message_id"`
	MediaURL         string    `json:"media_url"`
	MediaMimetype    string    `json:"media_mimetype"`
	SentAt           time.Time `json:"sent_at"`
}

type multichat struct {
	base, token string
	client      *http.Client
}

func (m multichat) get(path string, query url.Values) ([]byte, error) {
	target := strings.TrimRight(m.base, "/") + "/api" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+m.token)
	response, err := m.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("multichat GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("multichat GET %s: %d %s", path, response.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

// messages pages the whole log for one filter, newest first.
func (m multichat) messages(filter url.Values) ([]loggedMessage, error) {
	var all []loggedMessage
	before := ""
	for {
		query := url.Values{"limit": {"1000"}}
		for key, values := range filter {
			query[key] = values
		}
		if before != "" {
			query.Set("before", before)
		}
		body, err := m.get("/message-log", query)
		if err != nil {
			return nil, err
		}
		var page struct {
			Messages []loggedMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if len(page.Messages) < 1000 {
			return all, nil
		}
		before = page.Messages[len(page.Messages)-1].SentAt.Format(time.RFC3339Nano)
	}
}

// media fetches mxc://server/id through multichat.
func (m multichat) media(mxc string) ([]byte, error) {
	rest, found := strings.CutPrefix(mxc, "mxc://")
	server, id, ok := strings.Cut(rest, "/")
	if !found || !ok || server == "" || id == "" {
		return nil, fmt.Errorf("%q is not an mxc:// URI", mxc)
	}
	return m.get("/media/"+url.PathEscape(server)+"/"+url.PathEscape(id), nil)
}

// multichatMachine is the source_machine of every photo this command imports:
// the original lives with the chat it came from.
const multichatMachine = "multichat"

func main() {
	maxMembers := flag.Int("max-members", 8, "take photos only from chats with at most this many members")
	archive := flag.String("discord-archive", "", "a multichat-fetch-discord-history output directory whose saved images to import as well")
	directMessages := flag.String("discord-direct-messages", "", "a multichat-fetch-discord-history direct-messages folder whose saved images to import, each citing the message-store message it was sent in")
	facebookMessages := flag.String("facebook-messages", "", "a Facebook export's your_facebook_activity/messages folder whose photos to import, cited by journal-store's Messenger transcripts")
	chats := flag.Bool("chats", true, "import the photos in multichat's chats")
	var rooms roomList
	flag.Var(&rooms, "room", "a multichat room id whose photos to import whatever its size, Discord server channels included; repeat for more")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	dryRun := flag.Bool("dry-run", false, "read everything and post nothing")
	flag.Parse()
	settings, err := photostore.NewImportMessagesSettingsRegistry(servicesettings.ProcessEnvironment())
	if err == nil {
		err = settings.CheckRequired()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "photo-import-messages: %v\n", err)
		os.Exit(2)
	}
	chat := multichat{base: settings.String(photostore.SettingMultichatURL), token: settings.String(photostore.SettingMultichatToken),
		client: &http.Client{Timeout: 2 * time.Minute}}
	journal := journalcitation.Store{Base: settings.String(photostore.SettingJournalStoreURL), Client: &http.Client{Timeout: time.Minute}}
	messages := messagecitation.Store{Base: settings.String(photostore.SettingMessageStoreURL), Client: &http.Client{Timeout: time.Minute}}
	sources := sources{chats: *chats, rooms: rooms, archive: *archive, directMessages: *directMessages, facebookMessages: *facebookMessages}
	if err := run(chat, journal, messages, *maxMembers, sources, *storeURL, *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "photo-import-messages: %v\n", err)
		os.Exit(1)
	}
}

// sources say where to take photos from.
type sources struct {
	chats            bool
	rooms            []string
	archive          string
	directMessages   string
	facebookMessages string
}

func run(chat multichat, journal journalcitation.Store, messages messagecitation.Store, maxMembers int, from sources, storeURL string, dryRun bool, out io.Writer) error {
	in := importbatch.New()
	if from.chats {
		if err := importChats(chat, in, maxMembers, out); err != nil {
			return err
		}
	}
	for _, room := range from.rooms {
		if err := importRoom(chat, in, room, out); err != nil {
			return err
		}
	}
	if from.archive != "" {
		if err := importArchive(chat, in, from.archive); err != nil {
			return err
		}
	}
	if from.directMessages != "" {
		if err := importDirectMessages(messages, in, from.directMessages); err != nil {
			return err
		}
	}
	if from.facebookMessages != "" {
		if err := importFacebookMessages(journal, in, from.facebookMessages, maxMembers); err != nil {
			return err
		}
	}
	return in.Post(storeURL, dryRun, out)
}

func importChats(chat multichat, in *importbatch.Batch, maxMembers int, out io.Writer) error {
	body, err := chat.get("/conversations", nil)
	if err != nil {
		return err
	}
	var conversations []conversation
	if err := json.Unmarshal(body, &conversations); err != nil {
		return err
	}
	for _, room := range conversations {
		if room.MemberCount > maxMembers || (room.Platform == "discord" && strings.HasPrefix(room.Name, "#")) {
			continue
		}
		if err := addRoomImages(chat, in, room.RoomID, func(message loggedMessage) string {
			return "messages/" + message.Platform + "/" + message.RoomName
		}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "chats: %d photos so far\n", len(in.Photos))
	return nil
}

// importRoom adds the images of one room the user named, whatever its size.
// A Discord server channel's go under discord/<server>/<channel>, where its
// archive images go; any other room's under messages/<platform>/<room>.
func importRoom(chat multichat, in *importbatch.Batch, roomID string, out io.Writer) error {
	if err := addRoomImages(chat, in, roomID, func(message loggedMessage) string {
		if message.Platform == "discord" && message.NetworkName != "" {
			return "discord/" + message.NetworkName + "/" + message.ChannelName
		}
		return "messages/" + message.Platform + "/" + message.RoomName
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "room %s: %d photos so far\n", roomID, len(in.Photos))
	return nil
}

// addRoomImages adds every image still on the homeserver that was sent in a
// room, each cited by its message log ref, under the folder folderFor names.
// A message deleted on its app is kept like any other: the log keeps it and
// the homeserver keeps its file, so this is the copy that outlives the delete.
func addRoomImages(chat multichat, in *importbatch.Batch, roomID string, folderFor func(loggedMessage) string) error {
	messages, err := chat.messages(url.Values{"room_id": {roomID}})
	if err != nil {
		return fmt.Errorf("room %s: %w", roomID, err)
	}
	for _, message := range messages {
		if !strings.HasPrefix(message.MediaMimetype, "image/") || message.MediaURL == "" {
			continue
		}
		original, err := chat.media(message.MediaURL)
		if err != nil {
			in.Report["the file is gone from the homeserver"]++
			continue
		}
		in.Add(original, message.MediaMimetype, multichatMachine, message.Ref, folderFor(message), message.SentAt, photostore.DateFromMessage)
	}
	return nil
}

// roomList collects repeated -room flags.
type roomList []string

func (rooms *roomList) String() string { return strings.Join(*rooms, ",") }

func (rooms *roomList) Set(room string) error {
	if strings.TrimSpace(room) == "" {
		return fmt.Errorf("a room id is required")
	}
	*rooms = append(*rooms, room)
	return nil
}

// importArchive adds the images multichat-fetch-discord-history saved, each
// cited by the message log row the archive import made for its message.
func importArchive(chat multichat, in *importbatch.Batch, archive string) error {
	manifests, err := filepath.Glob(filepath.Join(archive, "*", "images.jsonl"))
	if err != nil {
		return err
	}
	for _, manifest := range manifests {
		server := filepath.Base(filepath.Dir(manifest))
		if server == directMessagesFolder {
			continue // -discord-direct-messages reads it
		}
		logged, err := chat.messages(url.Values{"network_id": {server}})
		if err != nil {
			return fmt.Errorf("server %s: %w", server, err)
		}
		byDiscordID := map[string]loggedMessage{}
		for _, message := range logged {
			if message.DiscordMessageID != "" {
				byDiscordID[message.DiscordMessageID] = message
			}
		}
		file, err := os.Open(manifest)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var line struct {
				MessageID   string `json:"message_id"`
				ContentType string `json:"content_type"`
				Path        string `json:"path"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				file.Close()
				return fmt.Errorf("%s: %w", manifest, err)
			}
			message, known := byDiscordID[line.MessageID]
			if !known {
				in.Report["its message is not in the message log (import the archive first)"]++
				continue
			}
			original, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), line.Path))
			if err != nil {
				file.Close()
				return err
			}
			in.Add(original, line.ContentType, multichatMachine, message.Ref, "discord/"+message.NetworkName+"/"+message.ChannelName, message.SentAt, photostore.DateFromMessage)
		}
		file.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
	}
	return nil
}
