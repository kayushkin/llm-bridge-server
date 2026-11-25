package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// discordAPI is Discord's REST API; a test points it at a fake.
var discordAPI = "https://discord.com/api/v10"

// errNoAccess is a channel the bot may see listed but not read (Discord's 403
// Missing Access), or one gone since it was listed (404).
var errNoAccess = errors.New("the bot cannot read this channel")

// retryableStatus are Discord's answers that mean "try again shortly".
// retryWait is the first wait before trying again; each next one is longer.
var retryWait = 2 * time.Second

var retryableStatus = map[int]bool{http.StatusBadGateway: true, http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true}

// discordClient calls Discord's REST API as one bot, waiting out rate limits
// and retrying a gateway that timed out.
type discordClient struct {
	token string
	http  *http.Client
	// requests counts calls made, for the run's report.
	requests int
}

func (c *discordClient) get(path string, query url.Values, out any) error {
	return c.call(http.MethodGet, path, query, nil, out)
}

// openDirectMessageChannel answers the bot's DM channel with a user: the one
// they already share, or a new empty one. It sends nothing, and the user is
// not told.
func (c *discordClient) openDirectMessageChannel(userID string) (discordChannel, error) {
	var channel discordChannel
	body, err := json.Marshal(map[string]string{"recipient_id": userID})
	if err != nil {
		return channel, err
	}
	return channel, c.call(http.MethodPost, "/users/@me/channels", nil, body, &channel)
}

func (c *discordClient) call(method, path string, query url.Values, body []byte, out any) error {
	target := discordAPI + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequest(method, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Authorization", "Bot "+c.token)
		request.Header.Set("User-Agent", "DiscordBot (https://github.com/kayushkin/message-store, 1) message-import-discord-bot")
		response, err := c.http.Do(request)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		answer, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		c.requests++
		if response.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			var limited struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(answer, &limited)
			time.Sleep(time.Duration((limited.RetryAfter + 0.1) * float64(time.Second)))
			continue
		}
		// Discord's gateway sometimes times out or is briefly down; the call
		// is tried again after a growing wait, and fails if it keeps on.
		if retryableStatus[response.StatusCode] && attempt < 4 {
			time.Sleep(time.Duration(attempt+1) * retryWait)
			continue
		}
		// Wait out an exhausted bucket before the next call rather than be
		// refused for it.
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			if resetAfter, err := strconv.ParseFloat(response.Header.Get("X-RateLimit-Reset-After"), 64); err == nil {
				time.Sleep(time.Duration(resetAfter * float64(time.Second)))
			}
		}
		switch {
		case response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%s %s: %w (%d)", method, path, errNoAccess, response.StatusCode)
		case response.StatusCode != http.StatusOK:
			return fmt.Errorf("%s %s answered %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(answer)))
		}
		return json.Unmarshal(answer, out)
	}
}

// Discord's objects, the parts this command reads.

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

// displayName is the name a reader sees: the global name, else the username.
func (u discordUser) displayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

type discordGuild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type discordChannel struct {
	ID            string        `json:"id"`
	Type          int           `json:"type"`
	GuildID       string        `json:"guild_id"`
	Name          string        `json:"name"`
	ParentID      string        `json:"parent_id"`
	LastMessageID string        `json:"last_message_id"`
	Recipients    []discordUser `json:"recipients"`
	// ThreadMetadata is set on a thread.
	ThreadMetadata *struct {
		ArchiveTimestamp string `json:"archive_timestamp"`
	} `json:"thread_metadata"`
}

// Channel types (Discord's CHANNEL_TYPE).
const (
	channelText           = 0
	channelDirect         = 1
	channelVoice          = 2
	channelGroupDirect    = 3
	channelAnnouncement   = 5
	channelAnnounceThread = 10
	channelPublicThread   = 11
	channelPrivateThread  = 12
	channelStage          = 13
	channelForum          = 15
	channelMedia          = 16
)

// holdsMessages says whether a channel of this type has messages of its own;
// a forum's and a media channel's are all in their threads.
func holdsMessages(channelType int) bool {
	switch channelType {
	case channelText, channelDirect, channelVoice, channelGroupDirect, channelAnnouncement,
		channelAnnounceThread, channelPublicThread, channelPrivateThread, channelStage:
		return true
	}
	return false
}

// isThread says whether a channel of this type is a thread.
func isThread(channelType int) bool {
	return channelType == channelAnnounceThread || channelType == channelPublicThread || channelType == channelPrivateThread
}

// hasThreads says whether threads can hang off a channel of this type.
func hasThreads(channelType int) bool {
	switch channelType {
	case channelText, channelAnnouncement, channelForum, channelMedia:
		return true
	}
	return false
}

type discordMessage struct {
	ID              string              `json:"id"`
	ChannelID       string              `json:"channel_id"`
	Type            int                 `json:"type"`
	Author          discordUser         `json:"author"`
	Content         string              `json:"content"`
	Timestamp       time.Time           `json:"timestamp"`
	EditedTimestamp *time.Time          `json:"edited_timestamp"`
	Attachments     []discordAttachment `json:"attachments"`
	Embeds          []discordEmbed      `json:"embeds"`
	Components      []discordComponent  `json:"components"`
	StickerItems    []struct {
		Name string `json:"name"`
	} `json:"sticker_items"`
	// Mentions are the people a message names; a system line names who it
	// is about here.
	Mentions         []discordUser `json:"mentions"`
	MessageReference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
}

type discordAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

type discordEmbed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Author      *struct {
		Name string `json:"name"`
	} `json:"author"`
	Fields []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline"`
	} `json:"fields"`
	Footer *struct {
		Text string `json:"text"`
	} `json:"footer"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
	Thumbnail *struct {
		URL string `json:"url"`
	} `json:"thumbnail"`
}

// discordComponent is an action row (type 1) holding buttons (type 2), or a
// button itself.
type discordComponent struct {
	Type     int    `json:"type"`
	Label    string `json:"label"`
	URL      string `json:"url"`
	Disabled bool   `json:"disabled"`
	Emoji    *struct {
		Name string `json:"name"`
	} `json:"emoji"`
	Components []discordComponent `json:"components"`
}

const (
	componentActionRow = 1
	componentButton    = 2
)

// discordEpoch is the first millisecond of 2015, where snowflakes count from.
const discordEpoch = 1420070400000

// snowflakeAt is the smallest snowflake of the moment unixMillis, so
// `after=` it reads every message from that moment on.
func snowflakeAt(unixMillis int64) string {
	if unixMillis <= discordEpoch {
		return "0"
	}
	return strconv.FormatUint(uint64(unixMillis-discordEpoch)<<22, 10)
}

// snowflakeLess compares two snowflakes as numbers.
func snowflakeLess(first, second string) bool {
	if len(first) != len(second) {
		return len(first) < len(second)
	}
	return first < second
}
