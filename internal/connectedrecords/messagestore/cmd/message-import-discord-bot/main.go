// message-import-discord-bot files what one Discord bot of the user's sent and
// could read: every channel and thread in its servers, and every DM the store
// knows it has, under the bot's account in message-store. Messages are filed
// by channel id and snowflake, the same keys every other Discord importer
// uses, so a message the user's own account also saw stays one row.
//
// Discord gives a bot no list of its DMs, so the bot itself files each DM
// conversation it opens (with the bot and the person as participants); this
// command then reads every DM conversation the store holds for the account.
//
// Discord answers a bot's DM channel with a user when asked to open one, the
// one they already share if there is one, without sending anything or telling
// the user. So -full also opens the DM channel with every person in the bot's
// conversations and reads each that holds messages: DMs from before the bot
// filed its channels, or that it never filed, are found that way.
//
// A run reads each channel from shortly before the last message the store
// holds, so recent edits are caught; -full reads everything again, archived
// threads included, and catches every edit.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/importclient"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

const (
	importerName  = "message-import-discord-bot"
	sourceDiscord = "discord"
)

var httpClient = &http.Client{Timeout: time.Minute}

func main() {
	credentialAccount := flag.String("credential-account", "", "the account of the auth-store provider \"discord\" credential holding the bot's token (required)")
	full := flag.Bool("full", false, "read every channel's whole history, archived threads included, not only what is new")
	reread := flag.Duration("reread", 24*time.Hour, "how far before the newest stored message each channel is read again, to catch edits")
	dryRun := flag.Bool("dry-run", false, "read Discord and count what would be sent; send nothing")
	flag.Parse()
	if *credentialAccount == "" {
		log.Fatal("-credential-account is required")
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	authStoreToken := settings.String(messagestore.SettingAuthStoreToken)
	if authStoreToken == "" {
		log.Fatal("AUTH_STORE_TOKEN is not set: the bot's Discord token is read from auth-store")
	}
	botToken, credentialID, err := resolveBotToken(strings.TrimRight(settings.String(messagestore.SettingAuthStoreURL), "/"), authStoreToken, *credentialAccount)
	if err != nil {
		log.Fatal(err)
	}
	run := &runner{
		discord:        &discordClient{token: botToken, http: httpClient},
		storeURL:       strings.TrimRight(settings.String(messagestore.SettingStoreURL), "/"),
		peopleStoreURL: strings.TrimRight(settings.String(messagestore.SettingPeopleStoreURL), "/"),
		full:           *full,
		reread:         *reread,
		credentialID:   credentialID,
		people:         map[string]string{},
	}
	run.client = importclient.New(run.storeURL, *dryRun)
	if err := run.run(); err != nil {
		log.Fatal(err)
	}
}

type runner struct {
	discord        *discordClient
	client         *importclient.Client
	storeURL       string
	peopleStoreURL string
	full           bool
	reread         time.Duration
	// credentialID is auth-store's id of the bot's token, filed on the
	// account so a sender can sign in as it.
	credentialID string

	bot     discordUser
	account messagestore.Account
	// stored is each conversation the store holds for the account, by
	// channel id.
	stored map[string]messagestore.Conversation
	// people is each Discord user id's people-store person, or "" for none.
	people map[string]string

	channelsRead, channelsSkipped int
	// directMessagesChecked counts the people whose DM channel -full opened.
	directMessagesChecked int
}

func (run *runner) run() error {
	if err := run.discord.get("/users/@me", nil, &run.bot); err != nil {
		return err
	}
	if err := run.storeCall(http.MethodPost, "/accounts", messagestore.AccountPut{Platform: "discord", Handle: run.bot.ID, DisplayName: run.bot.displayName(), Kind: "bot", CredentialID: run.credentialID}, &run.account); err != nil {
		return err
	}
	var stored []messagestore.Conversation
	if err := run.storeCall(http.MethodGet, "/conversations?"+url.Values{"account": {run.account.ID}, "limit": {"5000"}}.Encode(), nil, &stored); err != nil {
		return err
	}
	run.stored = map[string]messagestore.Conversation{}
	for _, conversation := range stored {
		if conversation.Source == sourceDiscord {
			run.stored[conversation.SourceRef] = conversation
		}
	}
	channels, err := run.channels()
	if err != nil {
		return err
	}
	description := fmt.Sprintf("Discord bot %s (%s): new messages and the last %s", run.account.ID, run.bot.displayName(), run.reread)
	if run.full {
		description = fmt.Sprintf("Discord bot %s (%s): every channel's whole history", run.account.ID, run.bot.displayName())
	}
	if err := run.client.Begin(importerName, description); err != nil {
		return err
	}
	for _, channel := range channels {
		if err := run.importChannel(channel); errors.Is(err, errNoAccess) {
			run.channelsSkipped++
		} else if err != nil {
			return fmt.Errorf("channel %s: %w", channel.ID, err)
		}
	}
	if err := run.client.Finish(); err != nil {
		return err
	}
	fmt.Printf("%s as %s: %d channels read, %d it cannot read, %d people's DM channels checked, %d Discord calls\n%s\n",
		run.account.ID, run.bot.displayName(), run.channelsRead, run.channelsSkipped, run.directMessagesChecked, run.discord.requests, run.client.Report())
	return nil
}

// channels lists every channel with messages the bot can find: its servers'
// channels and active threads (archived threads too on -full), and the DMs
// the store holds for it.
func (run *runner) channels() ([]discordChannel, error) {
	found := map[string]discordChannel{}
	var guilds []discordGuild
	if err := run.discord.get("/users/@me/guilds", nil, &guilds); err != nil {
		return nil, err
	}
	for _, guild := range guilds {
		var guildChannels []discordChannel
		if err := run.discord.get("/guilds/"+guild.ID+"/channels", nil, &guildChannels); err != nil {
			return nil, fmt.Errorf("server %s: %w", guild.ID, err)
		}
		var active struct {
			Threads []discordChannel `json:"threads"`
		}
		if err := run.discord.get("/guilds/"+guild.ID+"/threads/active", nil, &active); err != nil {
			return nil, fmt.Errorf("server %s: %w", guild.ID, err)
		}
		for _, channel := range append(guildChannels, active.Threads...) {
			if holdsMessages(channel.Type) {
				found[channel.ID] = channel
			}
		}
		if !run.full {
			continue
		}
		for _, parent := range guildChannels {
			if !hasThreads(parent.Type) {
				continue
			}
			for _, kind := range []string{"public", "private"} {
				threads, err := run.archivedThreads(parent.ID, kind)
				if errors.Is(err, errNoAccess) {
					continue
				}
				if err != nil {
					return nil, err
				}
				for _, thread := range threads {
					found[thread.ID] = thread
				}
			}
		}
	}
	if run.full {
		if err := run.findDirectMessages(found); err != nil {
			return nil, err
		}
	}
	for channelID, conversation := range run.stored {
		if _, known := found[channelID]; known || conversation.IsGroup {
			continue
		}
		var channel discordChannel
		if err := run.discord.get("/channels/"+channelID, nil, &channel); errors.Is(err, errNoAccess) {
			run.channelsSkipped++
			continue
		} else if err != nil {
			return nil, err
		}
		found[channelID] = channel
	}
	channels := make([]discordChannel, 0, len(found))
	for _, channel := range found {
		channels = append(channels, channel)
	}
	sort.Slice(channels, func(i, j int) bool { return snowflakeLess(channels[i].ID, channels[j].ID) })
	return channels, nil
}

// findDirectMessages opens the bot's DM channel with each person in its
// stored conversations and keeps every one that holds a message. A person the
// bot may not write to (another bot, someone who shares no server with it)
// is passed over.
func (run *runner) findDirectMessages(found map[string]discordChannel) error {
	handles := map[string]bool{}
	for _, conversation := range run.stored {
		for _, participant := range conversation.Participants {
			if participant.Handle != run.bot.ID {
				handles[participant.Handle] = true
			}
		}
	}
	sorted := make([]string, 0, len(handles))
	for handle := range handles {
		sorted = append(sorted, handle)
	}
	sort.Strings(sorted)
	for _, handle := range sorted {
		channel, err := run.discord.openDirectMessageChannel(handle)
		if errors.Is(err, errNoAccess) || isRefusedRecipient(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open the DM channel with %s: %w", handle, err)
		}
		run.directMessagesChecked++
		if channel.LastMessageID != "" {
			found[channel.ID] = channel
		}
	}
	return nil
}

// isRefusedRecipient is Discord's 400 for a user the bot cannot open a DM
// with, such as another bot.
func isRefusedRecipient(err error) bool {
	return err != nil && strings.Contains(err.Error(), "answered 400")
}

// archivedThreads lists a channel's archived threads of one kind, every page.
func (run *runner) archivedThreads(parentID, kind string) ([]discordChannel, error) {
	var all []discordChannel
	query := url.Values{"limit": {"100"}}
	for {
		var page struct {
			Threads []discordChannel `json:"threads"`
			HasMore bool             `json:"has_more"`
		}
		if err := run.discord.get("/channels/"+parentID+"/threads/archived/"+kind, query, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Threads...)
		if !page.HasMore || len(page.Threads) == 0 {
			return all, nil
		}
		last := page.Threads[len(page.Threads)-1]
		if last.ThreadMetadata == nil || last.ThreadMetadata.ArchiveTimestamp == "" {
			return nil, fmt.Errorf("channel %s: an archived thread with no archive time; cannot page on", parentID)
		}
		query.Set("before", last.ThreadMetadata.ArchiveTimestamp)
	}
}

// importChannel reads one channel's messages and queues them with the
// conversation, the bot listed in it so the account finds it.
func (run *runner) importChannel(channel discordChannel) error {
	after := "0"
	if stored, known := run.stored[channel.ID]; known && !run.full && stored.LastMessageAt > 0 {
		after = snowflakeAt(stored.LastMessageAt - run.reread.Milliseconds())
	}
	var messages []discordMessage
	for {
		var page []discordMessage
		if err := run.discord.get("/channels/"+channel.ID+"/messages", url.Values{"after": {after}, "limit": {"100"}}, &page); err != nil {
			return err
		}
		for _, message := range page {
			messages = append(messages, message)
			if snowflakeLess(after, message.ID) {
				after = message.ID
			}
		}
		if len(page) < 100 {
			break
		}
	}
	run.channelsRead++
	conversation := conversationOf(channel)
	participants := map[string]messagestore.Participant{run.bot.ID: {Handle: run.bot.ID, DisplayName: run.bot.displayName()}}
	for _, recipient := range channel.Recipients {
		participants[recipient.ID] = messagestore.Participant{Handle: recipient.ID, DisplayName: recipient.displayName()}
	}
	for _, message := range messages {
		participants[message.Author.ID] = messagestore.Participant{Handle: message.Author.ID, DisplayName: message.Author.displayName()}
	}
	for handle, participant := range participants {
		person, err := run.personOf(handle)
		if err != nil {
			return err
		}
		participant.PersonID = person
		conversation.Participants = append(conversation.Participants, participant)
	}
	sort.Slice(conversation.Participants, func(i, j int) bool { return conversation.Participants[i].Handle < conversation.Participants[j].Handle })
	if err := run.client.AddConversation(conversation); err != nil {
		return err
	}
	sort.Slice(messages, func(i, j int) bool { return snowflakeLess(messages[i].ID, messages[j].ID) })
	for _, message := range messages {
		if err := run.client.AddMessage(convertMessage(message)); err != nil {
			return err
		}
	}
	return nil
}

// personOf asks people-store whose a Discord account is; "" when nobody's.
func (run *runner) personOf(handle string) (string, error) {
	if person, asked := run.people[handle]; asked {
		return person, nil
	}
	var resolution struct {
		Person struct {
			ID string `json:"id"`
		} `json:"person"`
	}
	found, err := getJSON(run.peopleStoreURL+"/identities/resolve?"+url.Values{"kind": {"discord"}, "value": {handle}}.Encode(), &resolution)
	if err != nil {
		return "", err
	}
	if found && resolution.Person.ID == "" {
		return "", fmt.Errorf("people-store resolved a Discord account to a person with no id")
	}
	run.people[handle] = resolution.Person.ID
	return resolution.Person.ID, nil
}

// storeCall calls message-store directly, for what the import client does
// not carry: the account and the conversations it already holds.
func (run *runner) storeCall(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequest(method, run.storeURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("message-store %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("message-store %s %s answered %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, out)
}

func getJSON(address string, out any) (bool, error) {
	response, err := httpClient.Get(address)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s answered %d: %s", strings.SplitN(address, "?", 2)[0], response.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, json.Unmarshal(body, out)
}

// resolveBotToken reads the bot's token, and the credential's id, from
// auth-store's provider "discord" credential of the given account.
func resolveBotToken(authStoreURL, authStoreToken, account string) (token, credentialID string, err error) {
	request, err := http.NewRequest(http.MethodGet, authStoreURL+"/api/resolve/discord?"+url.Values{"account": {account}}.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Authorization", "Bearer "+authStoreToken)
	request.Header.Set("X-Auth-App", importerName)
	request.Header.Set("X-Auth-Reason", "discord:read-bot-messages")
	response, err := httpClient.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("auth-store: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("auth-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var credential struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		return "", "", fmt.Errorf("auth-store answer does not parse: %w", err)
	}
	if credential.APIKey == "" || credential.ID == "" {
		return "", "", errors.New("auth-store credential has no api_key or no id")
	}
	return credential.APIKey, credential.ID, nil
}

func init() { log.SetFlags(0); log.SetOutput(os.Stderr) }
