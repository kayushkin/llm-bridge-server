// Package facebookmessages reads the Messenger part of a Facebook data
// export (your_facebook_activity/messages, JSON format): its small chats,
// both sides.
//
// A thread is kept only when it has at most MaximumParticipants people: DMs
// and small chats are kept, big groups are not.
package facebookmessages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/message-store/internal/facebookexport"
)

// MaximumParticipants is the most people, the user included, a thread may
// have to be imported.
const MaximumParticipants = 8

// Folders are the folders under messages/ that hold threads.
var Folders = []string{"inbox", "archived_threads", "filtered_threads", "message_requests", "e2ee_cutover"}

// Thread is one conversation folder: messages/<folder>/<thread>/message_*.json.
type Thread struct {
	Folder       string // inbox, archived_threads, …
	ID           string // the thread folder's name
	Title        string
	Participants []string
	// ParticipantCount is how many people the export lists, two of the same
	// name counted twice.
	ParticipantCount int
	Messages         []Message
}

// Ref is the thread's folder and name, "<folder>/<thread>": the journal
// transcripts were filed under "<Ref>/<day>".
func (thread Thread) Ref() string { return thread.Folder + "/" + thread.ID }

// File is a file sent with a message, by its path in the export.
type File struct {
	// Kind is photo, video, gif, file or voice message.
	Kind string
	URI  string
}

// Name is the file's own name.
func (file File) Name() string { return path.Base(file.URI) }

// Message is one message, with its text already repaired.
type Message struct {
	// Ref is the message's key, stable across runs over the same export:
	// see Read.
	Ref         string
	Sender      string
	Time        time.Time
	Content     string
	Files       []File
	Sticker     bool
	IsCall      bool
	CallSeconds int // 0 for a missed call
	ShareLink   string
	IsUnsent    bool
}

type threadFile struct {
	Title        string `json:"title"`
	Participants []struct {
		Name string `json:"name"`
	} `json:"participants"`
	Messages []messageFile `json:"messages"`
}

type uriFile struct {
	URI string `json:"uri"`
}

type messageFile struct {
	SenderName   string    `json:"sender_name"`
	TimestampMs  int64     `json:"timestamp_ms"`
	Content      string    `json:"content"`
	Photos       []uriFile `json:"photos"`
	Videos       []uriFile `json:"videos"`
	Gifs         []uriFile `json:"gifs"`
	Files        []uriFile `json:"files"`
	AudioFiles   []uriFile `json:"audio_files"`
	Sticker      *uriFile  `json:"sticker"`
	CallDuration *int      `json:"call_duration"`
	Missed       bool      `json:"missed"`
	IsUnsent     bool      `json:"is_unsent"`
	Share        *struct {
		Link string `json:"link"`
	} `json:"share"`
}

// Skipped counts the threads Read left out.
type Skipped struct {
	TooManyParticipants int
	Messages            int // in those threads
}

// Read reads every thread of at most MaximumParticipants people under
// messagesDir (the export's your_facebook_activity/messages).
func Read(messagesDir string) ([]Thread, Skipped, error) {
	var threads []Thread
	var skipped Skipped
	for _, folder := range Folders {
		entries, err := os.ReadDir(filepath.Join(messagesDir, folder))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, skipped, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			thread, err := readThread(filepath.Join(messagesDir, folder, entry.Name()))
			if err != nil {
				return nil, skipped, err
			}
			if len(thread.Messages) == 0 {
				continue
			}
			thread.Folder, thread.ID = folder, entry.Name()
			if thread.ParticipantCount > MaximumParticipants {
				skipped.TooManyParticipants++
				skipped.Messages += len(thread.Messages)
				continue
			}
			assignRefs(&thread)
			threads = append(threads, thread)
		}
	}
	return threads, skipped, nil
}

// assignRefs keys each message. The export gives a message no id, so the key
// is built from what it holds: the thread, the millisecond, and a short hash
// of the sender and content, with "/2", "/3"… for the second and later exact
// copies in the export's own order. A re-run over the same export finds the
// same key; a message whose text the export changes is a new one.
func assignRefs(thread *Thread) {
	copies := map[string]int{}
	for index := range thread.Messages {
		message := &thread.Messages[index]
		var names []string
		for _, file := range message.Files {
			names = append(names, file.Name())
		}
		digest := sha256.Sum256([]byte(message.Sender + "\x00" + message.Content + "\x00" + strings.Join(names, "\x00")))
		ref := thread.Ref() + "/" + strconv.FormatInt(message.Time.UnixMilli(), 10) + "/" + hex.EncodeToString(digest[:])[:12]
		copies[ref]++
		if copies[ref] > 1 {
			ref += "/" + strconv.Itoa(copies[ref])
		}
		message.Ref = ref
	}
}

func readThread(directory string) (Thread, error) {
	files, err := filepath.Glob(filepath.Join(directory, "message_*.json"))
	if err != nil {
		return Thread{}, err
	}
	sort.Strings(files)
	var thread Thread
	seenParticipant := map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return Thread{}, err
		}
		var parsed threadFile
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return Thread{}, fmt.Errorf("%s: %w", file, err)
		}
		thread.Title = facebookexport.Repair(parsed.Title)
		thread.ParticipantCount = max(thread.ParticipantCount, len(parsed.Participants))
		for _, participant := range parsed.Participants {
			name := facebookexport.Repair(participant.Name)
			if !seenParticipant[name] {
				seenParticipant[name] = true
				thread.Participants = append(thread.Participants, name)
			}
		}
		for _, message := range parsed.Messages {
			thread.Messages = append(thread.Messages, convert(message))
		}
	}
	return thread, nil
}

func filesOf(kind string, uris []uriFile) []File {
	var files []File
	for _, uri := range uris {
		files = append(files, File{Kind: kind, URI: uri.URI})
	}
	return files
}

func convert(message messageFile) Message {
	converted := Message{
		Sender:   facebookexport.Repair(message.SenderName),
		Time:     time.UnixMilli(message.TimestampMs).UTC(),
		Content:  facebookexport.Repair(message.Content),
		Sticker:  message.Sticker != nil,
		IsUnsent: message.IsUnsent,
	}
	converted.Files = append(converted.Files, filesOf("photo", message.Photos)...)
	converted.Files = append(converted.Files, filesOf("video", message.Videos)...)
	converted.Files = append(converted.Files, filesOf("gif", message.Gifs)...)
	converted.Files = append(converted.Files, filesOf("file", message.Files)...)
	converted.Files = append(converted.Files, filesOf("voice message", message.AudioFiles)...)
	if message.CallDuration != nil {
		converted.IsCall = true
		if !message.Missed {
			converted.CallSeconds = *message.CallDuration
		}
	}
	if message.Share != nil && message.Share.Link != "" {
		converted.ShareLink = facebookexport.Repair(message.Share.Link)
	}
	return converted
}

// Title names a thread as the journal transcripts did, without their
// "Messenger:" prefix and day.
func Title(thread Thread, selfName string) string {
	if thread.Title != "" && thread.Title != selfName {
		return thread.Title
	}
	var others []string
	for _, participant := range thread.Participants {
		if participant != selfName {
			others = append(others, participant)
		}
	}
	if len(others) > 0 {
		return strings.Join(others, ", ")
	}
	return thread.ID
}
