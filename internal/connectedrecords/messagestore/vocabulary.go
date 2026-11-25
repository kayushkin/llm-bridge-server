package messagestore

// Platforms are the apps a conversation happened on.
var Platforms = []string{"sms", "skype", "facebook-messenger", "discord", "whatsapp", "telegram", "signal", "slack", "instagram", "google-chat", "matrix"}

// MessageKinds are what a message row can be.
var MessageKinds = []string{
	"text",    // words, with or without attachments
	"media",   // only attachments
	"call",    // a voice or video call, call_seconds long (0: missed or unanswered)
	"sticker", // a sticker, body its name
	"system",  // a line the platform wrote: someone joined, the title changed
}

// BodySources are where a message's text came from. The platform's own text
// outranks a bridge's rendering of it: a bridge rewrites what it carries
// (Discord's mention and time tokens become plain names and UTC times), so a
// bridge's text never replaces the platform's.
var BodySources = []string{
	"platform", // the app's own text: an export, a data package, its API
	"bridge",   // a bridge's rendering of the message, as a Matrix bridge carried it
}

// AccountKinds are whose an account of ours is.
var AccountKinds = []string{
	"user", // the user's own account
	"bot",  // a bot the user runs: what it sends is what people get from it
}

// ActivityGroupings are the keys GET /activity can count by.
var ActivityGroupings = []string{"person", "conversation", "platform"}

// ActivityIntervals are the bucket sizes GET /activity can count in.
var ActivityIntervals = []string{"day", "week", "month", "year"}

// DayInvolvements say how the user took part in a conversation on a day.
var DayInvolvements = []string{
	"direct",       // a direct chat: everything in it was to or from the user
	"participated", // the user wrote in it that day
	"addressed",    // someone replied to the user or mentioned them that day
	"none",         // a group or channel the user only belonged to that day
}

// ConversationMergeRules are why one conversation was merged into another.
var ConversationMergeRules = []string{
	"google_conversation_id", // two Matrix rooms carry one Google Messages conversation
	"phone_numbers",          // a text thread's other people have exactly the numbers of a phone thread
	"direct_person",          // two direct chats with the same people-store person on one app
}

// Vocabulary is GET /vocabulary.
type Vocabulary struct {
	Platforms              []string `json:"platforms"`
	ConversationMergeRules []string `json:"conversation_merge_rules"`
	MessageKinds           []string `json:"message_kinds"`
	ActivityGroupings      []string `json:"activity_groupings"`
	ActivityIntervals      []string `json:"activity_intervals"`
	DayInvolvements        []string `json:"day_involvements"`
	AccountKinds           []string `json:"account_kinds"`
	BodySources            []string `json:"body_sources"`
}

func vocabulary() Vocabulary {
	return Vocabulary{Platforms: Platforms, ConversationMergeRules: ConversationMergeRules, MessageKinds: MessageKinds, ActivityGroupings: ActivityGroupings, ActivityIntervals: ActivityIntervals, DayInvolvements: DayInvolvements, AccountKinds: AccountKinds, BodySources: BodySources}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
