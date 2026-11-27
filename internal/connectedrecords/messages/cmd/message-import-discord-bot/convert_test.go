package main

import (
	"encoding/json"
	"testing"
)

func TestABotCardKeepsItsEmbedAndButtonsAndAReplyNamesItsTarget(t *testing.T) {
	var message discordMessage
	raw := `{"id":"20","channel_id":"5","type":19,"author":{"id":"900","username":"helper","bot":true},"content":"",
		"timestamp":"2026-01-02T10:00:00.000000+00:00","edited_timestamp":"2026-01-02T11:00:00.000000+00:00",
		"embeds":[{"title":"Picnic","fields":[{"name":"Going","value":"3 of 10","inline":true}],"footer":{"text":"Saturday"}}],
		"components":[{"type":1,"components":[{"type":2,"label":"Join","emoji":{"name":"✅"}},{"type":2,"label":"Map","url":"https://example.com"}]}],
		"message_reference":{"message_id":"19"}}`
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	converted := convertMessage(message)
	if converted.Kind != "text" || !converted.Edited || converted.ReplyToSourceRef != "19" || converted.SenderHandle != "900" {
		t.Fatalf("converted = %+v", converted)
	}
	if len(converted.Embeds) != 1 || converted.Embeds[0].Fields[0].Value != "3 of 10" || converted.Embeds[0].FooterText != "Saturday" {
		t.Fatalf("embeds = %+v", converted.Embeds)
	}
	if len(converted.Buttons) != 2 || converted.Buttons[0].Emoji != "✅" || converted.Buttons[1].URL != "https://example.com" {
		t.Fatalf("buttons = %+v", converted.Buttons)
	}
	if converted.ConversationSourceRef != "5" || converted.SourceRef != "20" || converted.SourceHash == "" {
		t.Fatalf("keys = %+v", converted)
	}
}

func TestALineDiscordWroteIsASystemLine(t *testing.T) {
	converted := convertMessage(discordMessage{ID: "1", ChannelID: "5", Type: 18, Author: discordUser{ID: "7"}})
	if converted.Kind != "system" {
		t.Fatalf("kind = %s, want system", converted.Kind)
	}
}

func TestSnowflakesCountFromDiscordsEpoch(t *testing.T) {
	// 1420070400000 + 1000 ms is 1000<<22.
	if got := snowflakeAt(1420070401000); got != "4194304000" {
		t.Fatalf("snowflakeAt = %s", got)
	}
	if !snowflakeLess("999", "1000") || snowflakeLess("1000", "999") {
		t.Fatal("snowflakes compare as numbers")
	}
}

func TestAThreadNamesItsChannelAndAChannelNamesNone(t *testing.T) {
	if got := conversationOf(discordChannel{ID: "t1", Type: channelPublicThread, ParentID: "c1"}); got.ParentSourceRef != "c1" {
		t.Fatalf("thread parent = %q", got.ParentSourceRef)
	}
	if got := conversationOf(discordChannel{ID: "c1", Type: channelText, ParentID: "category"}); got.ParentSourceRef != "" {
		t.Fatalf("a channel's category is not a parent conversation: %q", got.ParentSourceRef)
	}
}

func TestALineDiscordWroteWithNoTextSaysWhatHappened(t *testing.T) {
	added := convertMessage(discordMessage{ID: "1", ChannelID: "5", Type: 1, Author: discordUser{ID: "900"}, Mentions: []discordUser{{ID: "42"}}})
	if added.Kind != "system" || added.Body != "added <@42> to the thread" {
		t.Fatalf("added = %+v", added)
	}
	left := convertMessage(discordMessage{ID: "2", ChannelID: "5", Type: 2, Author: discordUser{ID: "42"}, Mentions: []discordUser{{ID: "42"}}})
	if left.Body != "left the thread" {
		t.Fatalf("left = %q", left.Body)
	}
	named := convertMessage(discordMessage{ID: "3", ChannelID: "5", Type: 18, Author: discordUser{ID: "42"}, Content: "a thread name"})
	if named.Body != "a thread name" {
		t.Fatalf("a line Discord gave words keeps them: %q", named.Body)
	}
}

func TestABoostAndAPollResultSayWhatHappened(t *testing.T) {
	cases := []struct {
		message discordMessage
		want    string
	}{
		{discordMessage{ID: "1", Type: 8, Author: discordUser{ID: "7"}}, "boosted the server"},
		{discordMessage{ID: "2", Type: 8, Author: discordUser{ID: "7"}, Content: "3"}, "boosted the server 3 times"},
		{discordMessage{ID: "3", Type: 10, Author: discordUser{ID: "7"}}, "boosted the server, and it reached level 2"},
	}
	var poll discordMessage
	if err := json.Unmarshal([]byte(`{"id":"4","channel_id":"5","type":46,"author":{"id":"7"},"embeds":[{"type":"poll_result","fields":[
		{"name":"poll_question_text","value":"Pizza or tacos?"},{"name":"victor_answer_text","value":"Tacos"},
		{"name":"victor_answer_votes","value":"5"},{"name":"total_votes","value":"8"}]}]}`), &poll); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct {
		message discordMessage
		want    string
	}{poll, `the poll "Pizza or tacos?" closed: "Tacos" won with 5 of 8 votes`})
	for _, c := range cases {
		if got := convertMessage(c.message); got.Kind != "system" || got.Body != c.want {
			t.Errorf("type %d: %q (%s), want %q", c.message.Type, got.Body, got.Kind, c.want)
		}
	}
}
