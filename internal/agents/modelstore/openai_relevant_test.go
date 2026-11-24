package modelstore

import "testing"

func TestOpenaiRelevantKeepsEveryNumberedGPTVersion(t *testing.T) {
	cases := map[string]bool{
		"gpt-4o": true, "gpt-4.1-mini": true, "gpt-5.5-pro": true,
		"gpt-6-sol": true, "gpt-6.1-terra": true, "gpt-7": true,
		"o3": true, "o4-mini": true, "o9-pro": true, "chatgpt-image-latest": true,
		"gpt-3.5-turbo": false, "gpt-image-2": false, "gpt-audio-mini": false,
		"gpt-realtime-2": false, "gpt-live-1": false, "gpt-transcribe": false,
		"omni-moderation-latest": false, "text-embedding-3-large": false,
		"tts-1": false, "whisper-1": false, "chat-latest": false,
		"gpt-4o-search-preview": false,
	}
	for id, want := range cases {
		if got := openaiRelevant(id); got != want {
			t.Errorf("openaiRelevant(%q) = %v, want %v", id, got, want)
		}
	}
}
