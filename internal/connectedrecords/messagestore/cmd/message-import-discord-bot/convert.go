package main

import (
	"fmt"
	"strconv"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/importclient"
)

// Discord message types that carry what someone wrote, a bot's answer to a
// command included. Every other type is a line Discord wrote: a pin, a
// thread made, someone joined.
var writtenMessageTypes = map[int]bool{
	0:  true, // a message
	19: true, // a reply
	20: true, // a bot's answer to a slash command
	23: true, // a bot's answer to a context menu command
}

// convertMessage turns one Discord message into the store's form, filed under
// its channel and its own snowflake as every Discord importer files it.
func convertMessage(message discordMessage) messagestore.ImportMessage {
	converted := messagestore.ImportMessage{
		ConversationSource:    sourceDiscord,
		ConversationSourceRef: message.ChannelID,
		SentAt:                message.Timestamp.UnixMilli(),
		SenderHandle:          message.Author.ID,
		Body:                  message.Content,
		BodySource:            "platform",
		Edited:                message.EditedTimestamp != nil,
		Source:                sourceDiscord,
		SourceRef:             message.ID,
	}
	for _, attachment := range message.Attachments {
		converted.Attachments = append(converted.Attachments, messagestore.Attachment{MimeType: attachment.ContentType, Name: attachment.Filename})
	}
	for _, embed := range message.Embeds {
		converted.Embeds = append(converted.Embeds, convertEmbed(embed))
	}
	converted.Buttons = buttonsOf(message.Components)
	if message.Type == 19 && message.MessageReference != nil {
		converted.ReplyToSourceRef = message.MessageReference.MessageID
	}
	switch {
	case !writtenMessageTypes[message.Type]:
		converted.Kind = "system"
		if words := systemLineWords(message); words != "" {
			converted.Body = words
		}
	case message.Content == "" && len(message.StickerItems) > 0:
		converted.Kind, converted.Body = "sticker", message.StickerItems[0].Name
	case message.Content == "" && len(message.Embeds) == 0 && len(message.Attachments) > 0:
		converted.Kind = "media"
	default:
		converted.Kind = "text"
	}
	converted.SourceHash = importclient.MessageHash(converted)
	return converted
}

func convertEmbed(embed discordEmbed) messagestore.Embed {
	converted := messagestore.Embed{Title: embed.Title, Description: embed.Description, URL: embed.URL}
	if embed.Author != nil {
		converted.AuthorName = embed.Author.Name
	}
	for _, field := range embed.Fields {
		converted.Fields = append(converted.Fields, messagestore.EmbedField{Name: field.Name, Value: field.Value, Inline: field.Inline})
	}
	if embed.Footer != nil {
		converted.FooterText = embed.Footer.Text
	}
	if embed.Image != nil {
		converted.ImageURL = embed.Image.URL
	}
	if embed.Thumbnail != nil {
		converted.ThumbnailURL = embed.Thumbnail.URL
	}
	return converted
}

// buttonsOf lists the buttons in a message's action rows, in reading order.
func buttonsOf(components []discordComponent) []messagestore.Button {
	var buttons []messagestore.Button
	for _, component := range components {
		switch component.Type {
		case componentActionRow:
			buttons = append(buttons, buttonsOf(component.Components)...)
		case componentButton:
			button := messagestore.Button{Label: component.Label, URL: component.URL, Disabled: component.Disabled}
			if component.Emoji != nil {
				button.Emoji = component.Emoji.Name
			}
			buttons = append(buttons, button)
		}
	}
	return buttons
}

// conversationOf is a channel as a conversation: a server channel or thread
// is a group named as Discord names it; a DM is direct and unnamed.
func conversationOf(channel discordChannel) messagestore.ImportConversation {
	conversation := messagestore.ImportConversation{
		Platform:  "discord",
		Source:    sourceDiscord,
		SourceRef: channel.ID,
		Title:     channel.Name,
		IsGroup:   channel.Type != channelDirect,
	}
	if isThread(channel.Type) {
		conversation.ParentSourceRef = channel.ParentID
	}
	return conversation
}

// systemLineWords is what Discord's own app shows for a line it wrote whose
// content holds no words of its own (empty, or a boost's count), people
// named as mentions so a reader sees their names. A kind not listed, or a
// line Discord gave words, answers "" and keeps its content.
func systemLineWords(message discordMessage) string {
	mentioned := ""
	if len(message.Mentions) > 0 {
		mentioned = "<@" + message.Mentions[0].ID + ">"
	}
	switch message.Type {
	case 1: // RECIPIENT_ADD
		if mentioned != "" {
			return "added " + mentioned + " to the thread"
		}
	case 2: // RECIPIENT_REMOVE
		if mentioned != "" && message.Mentions[0].ID != message.Author.ID {
			return "removed " + mentioned + " from the thread"
		}
		return "left the thread"
	case 6: // CHANNEL_PINNED_MESSAGE
		return "pinned a message"
	case 7: // USER_JOIN
		return "joined the server"
	case 21: // THREAD_STARTER_MESSAGE
		return "started a thread from a message"
	case 8, 9, 10, 11: // USER_PREMIUM_GUILD_SUBSCRIPTION and its tiers 1 to 3
		boosted := "boosted the server"
		// Discord puts the number of boosts in the content when it is more than one.
		if count, err := strconv.Atoi(message.Content); err == nil && count > 1 {
			boosted = fmt.Sprintf("boosted the server %d times", count)
		}
		if message.Type > 8 {
			return fmt.Sprintf("%s, and it reached level %d", boosted, message.Type-8)
		}
		return boosted
	case 46: // POLL_RESULT
		return pollResultWords(message)
	}
	return ""
}

// pollResultWords is a poll's result as Discord's app states it, from the
// fields of the poll_result card Discord attaches to the line.
func pollResultWords(message discordMessage) string {
	fields := map[string]string{}
	for _, embed := range message.Embeds {
		for _, field := range embed.Fields {
			fields[field.Name] = field.Value
		}
	}
	question := fields["poll_question_text"]
	if question == "" {
		return "a poll closed"
	}
	if winner := fields["victor_answer_text"]; winner != "" {
		return fmt.Sprintf("the poll %q closed: %q won with %s of %s votes", question, winner, fields["victor_answer_votes"], fields["total_votes"])
	}
	return fmt.Sprintf("the poll %q closed with %s votes and no single winner", question, fields["total_votes"])
}
