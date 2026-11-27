package main

import (
	"encoding/base64"
	"sort"
	"strings"
	"time"
)

// The user's texts reach multichat by two paths: the mautrix Google Messages
// bridge (platform gmessages) and gmessages-web-bridge, which reads Google
// Messages for web in a browser (platform gmessagesweb). Each makes its own
// Matrix room for a conversation, so a text both carried is two log rows in
// two rooms. Both rooms name Google's own conversation id in their m.bridge
// channel: the bridge as "<login prefix>.<conversation id>", the web bridge
// as the conversation's token in the web app's address, base64url of a
// protobuf message whose field 2 is the conversation id.
//
// A web row is the same message as a bridge row in the room with the same
// conversation id when their text is equal and the web row's time is the
// bridge row's time cut to the minute (the web app shows only minutes). Rows
// are paired one to one in time order. The bridge row is kept, since it has
// the exact time; the web row is filed as it and its ref superseded.
const (
	platformGoogleMessagesBridge = "gmessages"
	platformGoogleMessagesWeb    = "gmessagesweb"
)

// googleConversationID is Google's conversation id behind a Google Messages
// row's channel, or "" when the channel does not hold one this command reads.
func googleConversationID(row loggedMessage) string {
	switch row.Platform {
	case platformGoogleMessagesBridge:
		if _, id, found := strings.Cut(row.ChannelID, "."); found {
			return id
		}
	case platformGoogleMessagesWeb:
		return protobufField2(row.ChannelID)
	}
	return ""
}

// protobufField2 reads field 2 of a base64url protobuf message made only of
// length-delimited fields, "" when the token is not one.
func protobufField2(token string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return ""
	}
	field2 := ""
	for index := 0; index < len(raw); {
		key := raw[index]
		index++
		if key&7 != 2 || key&0x80 != 0 || index >= len(raw) || raw[index]&0x80 != 0 {
			return ""
		}
		length := int(raw[index])
		index++
		if index+length > len(raw) {
			return ""
		}
		if key>>3 == 2 {
			field2 = string(raw[index : index+length])
		}
		index += length
	}
	return field2
}

// readGoogleMessagesRows reads every row of both Google Messages platforms,
// edits left out: the pairing looks at the whole history, not the window,
// so a twin that arrived late is still found.
func readGoogleMessagesRows(multichat multichatClient) ([]loggedMessage, error) {
	var rows []loggedMessage
	for _, platform := range []string{platformGoogleMessagesBridge, platformGoogleMessagesWeb} {
		reader := &logReader{multichat: multichat, platform: platform, seen: map[string]bool{}}
		for {
			page, err := reader.next()
			if err != nil {
				return nil, err
			}
			if len(page.Messages) == 0 {
				break
			}
			for _, row := range page.Messages {
				if row.ReplacesEventID == "" {
					rows = append(rows, row)
				}
			}
		}
	}
	return rows, nil
}

// googleMessagesTwins is what matchGoogleMessagesTwins found.
type googleMessagesTwins struct {
	// bridgeRowOfWebRow is, by a web row's ref, the bridge row it is.
	bridgeRowOfWebRow map[string]loggedMessage
	// webRefsOfBridgeRow are, by a bridge row's ref, the web rows filed as it.
	webRefsOfBridgeRow                                 map[string][]string
	twinRooms, webRoomsWithoutTwin, unreadableWebRooms int
}

func matchGoogleMessagesTwins(rows []loggedMessage) googleMessagesTwins {
	twins := googleMessagesTwins{bridgeRowOfWebRow: map[string]loggedMessage{}, webRefsOfBridgeRow: map[string][]string{}}
	type key struct {
		conversation string
		minute       int64
		body         string
	}
	sorted := append([]loggedMessage(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SentAt.Before(sorted[j].SentAt) })
	bridgeConversations := map[string]bool{}
	waiting := map[key][]loggedMessage{}
	for _, row := range sorted {
		if row.Platform != platformGoogleMessagesBridge {
			continue
		}
		if id := googleConversationID(row); id != "" {
			bridgeConversations[id] = true
			k := key{id, row.SentAt.Truncate(time.Minute).UnixMilli(), row.Body}
			waiting[k] = append(waiting[k], row)
		}
	}
	webRooms := map[string]string{}
	for _, row := range sorted {
		if row.Platform != platformGoogleMessagesWeb {
			continue
		}
		id := googleConversationID(row)
		webRooms[row.RoomID] = id
		if id == "" {
			continue
		}
		k := key{id, row.SentAt.UnixMilli(), row.Body}
		if candidates := waiting[k]; len(candidates) > 0 {
			bridgeRow := candidates[0]
			waiting[k] = candidates[1:]
			twins.bridgeRowOfWebRow[row.Ref] = bridgeRow
			twins.webRefsOfBridgeRow[bridgeRow.Ref] = append(twins.webRefsOfBridgeRow[bridgeRow.Ref], row.Ref)
		}
	}
	for _, id := range webRooms {
		switch {
		case id == "":
			twins.unreadableWebRooms++
		case bridgeConversations[id]:
			twins.twinRooms++
		default:
			twins.webRoomsWithoutTwin++
		}
	}
	return twins
}

// apply replaces each imported web row that has a bridge twin by that bridge
// row, once, keeping newest first; a bridge row the selection leaves out is
// not added.
func (twins googleMessagesTwins) apply(imported []loggedMessage, chosen selection, rooms map[string]room) []loggedMessage {
	present := map[string]bool{}
	for _, row := range imported {
		present[row.Ref] = true
	}
	var out []loggedMessage
	for _, row := range imported {
		bridgeRow, twin := twins.bridgeRowOfWebRow[row.Ref]
		if !twin {
			out = append(out, row)
			continue
		}
		if !present[bridgeRow.Ref] && chosen.skipReason(bridgeRow, rooms) == "" {
			present[bridgeRow.Ref] = true
			out = append(out, bridgeRow)
		}
	}
	// Every bridge row with a twin is sent, so a web row stored before its
	// twin was logged is superseded however old it is.
	for bridgeRef := range twins.webRefsOfBridgeRow {
		if present[bridgeRef] {
			continue
		}
		bridgeRow := twins.bridgeRowOfWebRow[twins.webRefsOfBridgeRow[bridgeRef][0]]
		if chosen.skipReason(bridgeRow, rooms) == "" {
			present[bridgeRef] = true
			out = append(out, bridgeRow)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SentAt.After(out[j].SentAt) })
	return out
}
