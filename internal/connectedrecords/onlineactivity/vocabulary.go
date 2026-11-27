package onlineactivity

// Every closed set of words the store accepts, served at GET /vocabularies.
// The store refuses a word not listed here; Google's product names are an
// open set and are kept as the export gives them.

// Actions are the verbs an activity can have.
var Actions = []string{
	ActionSearched, ActionVisited, ActionWatched, ActionViewed, ActionUsed, ActionDirections, ActionSaid,
	ActionLiked, ActionDisliked, ActionSubscribed, ActionListened, ActionPlayed, ActionLaunched, ActionPrompted,
	ActionRead, ActionShared, ActionOpened, ActionCalled, ActionAsked, ActionSaved, ActionDismissed, ActionJoined,
	ActionVoted, ActionPurchased, ActionPreregistered, ActionDefined, ActionBookmarked, ActionOther,
}

// The actions.
const (
	ActionSearched      = "searched"
	ActionVisited       = "visited"
	ActionWatched       = "watched"
	ActionViewed        = "viewed"
	ActionUsed          = "used"
	ActionDirections    = "directions"
	ActionSaid          = "said"
	ActionLiked         = "liked"
	ActionDisliked      = "disliked"
	ActionSubscribed    = "subscribed"
	ActionListened      = "listened"
	ActionPlayed        = "played"
	ActionLaunched      = "launched"
	ActionPrompted      = "prompted"
	ActionRead          = "read"
	ActionShared        = "shared"
	ActionOpened        = "opened"
	ActionCalled        = "called"
	ActionAsked         = "asked"
	ActionSaved         = "saved"
	ActionDismissed     = "dismissed"
	ActionJoined        = "joined"
	ActionVoted         = "voted"
	ActionPurchased     = "purchased"
	ActionPreregistered = "preregistered"
	ActionDefined       = "defined"
	ActionBookmarked    = "bookmarked"
	// ActionOther is a card whose words match no phrase below; its words are
	// kept in action_text and title.
	ActionOther = "other"
)

// ActionPhrases are the words My Activity starts a card with, before the
// thing done, and the action each means. A card that is only a link, with no
// words before it, is ActionViewed: Google lists the thing that was opened.
var ActionPhrases = []ActionPhrase{
	{"Searched for", ActionSearched},
	{"Visited", ActionVisited},
	{"Watched", ActionWatched},
	{"Tracking watch time points on", ActionWatched},
	{"Viewed", ActionViewed},
	{"Viewed image from", ActionViewed},
	{"Viewed job details", ActionViewed},
	{"Interacted with Google Help article", ActionViewed},
	{"Used", ActionUsed},
	{"Used a map on", ActionUsed},
	{"Directions to", ActionDirections},
	{"Said", ActionSaid},
	{"Liked", ActionLiked},
	{"Disliked", ActionDisliked},
	{"Subscribed to", ActionSubscribed},
	{"Listened to", ActionListened},
	{"Played", ActionPlayed},
	{"Launched", ActionLaunched},
	{"Prompted", ActionPrompted},
	{"Read", ActionRead},
	{"Shared", ActionShared},
	{"Opened", ActionOpened},
	{"Opened My Maps map:", ActionOpened},
	{"Called", ActionCalled},
	{"Asked Maps", ActionAsked},
	{"Saved", ActionSaved},
	{"Dismissed", ActionDismissed},
	{"Joined", ActionJoined},
	{"Joined beta program for", ActionJoined},
	{"Voted on", ActionVoted},
	{"Started to purchase", ActionPurchased},
	{"Placed an order with", ActionPurchased},
	{"Pre-registered for", ActionPreregistered},
	{"Defined", ActionDefined},
}

// ActionForPhrase is the action a card's opening words mean, and whether the
// words are known.
func ActionForPhrase(phrase string) (string, bool) {
	for _, known := range ActionPhrases {
		if known.Phrase == phrase {
			return known.Action, true
		}
	}
	return ActionOther, false
}

// Sources are where records come from.
var Sources = []string{SourceGoogleMyActivity, SourceYouTubeHistory, SourceChromeHistory, SourceChromeBookmarks}

// The sources.
const (
	// SourceGoogleMyActivity is Takeout's My Activity/<Product>/MyActivity.html.
	SourceGoogleMyActivity = "google_my_activity"
	// SourceYouTubeHistory is Takeout's YouTube watch and search history.
	SourceYouTubeHistory = "youtube_history"
	// SourceChromeHistory is Takeout's Chrome/History.json.
	SourceChromeHistory = "chrome_history"
	// SourceChromeBookmarks is Takeout's Chrome/Bookmarks.html.
	SourceChromeBookmarks = "chrome_bookmarks"
)

// ImportKinds are the kinds of file an import reads.
var ImportKinds = []string{ImportKindMyActivityHTML, ImportKindYouTubeHistoryHTML, ImportKindChromeHistoryJSON, ImportKindChromeBookmarksHTML}

// The import kinds.
const (
	ImportKindMyActivityHTML      = "google_my_activity_html"
	ImportKindYouTubeHistoryHTML  = "youtube_history_html"
	ImportKindChromeHistoryJSON   = "chrome_history_json"
	ImportKindChromeBookmarksHTML = "chrome_bookmarks_html"
)

// RevisionReasons say why an activity was copied to revisions.
var RevisionReasons = []string{RevisionDelete, RevisionRestore, RevisionPurge}

// The revision reasons.
const (
	RevisionDelete  = "delete"
	RevisionRestore = "restore"
	RevisionPurge   = "purge"
)

// The id prefixes, as kanban-store's registry knows them.
const (
	IDPrefixActivity   = "activity"
	IDPrefixAttachment = "attachment"
	IDPrefixImport     = "import"
)

func knownWord(words []string, word string) bool {
	for _, known := range words {
		if known == word {
			return true
		}
	}
	return false
}
