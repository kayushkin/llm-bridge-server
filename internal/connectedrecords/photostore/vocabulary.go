package photostore

// Where a photo's taken_at came from, most trusted first. Served at
// GET /date-sources.
const (
	DateFromExif     = "exif"     // the camera's own DateTimeOriginal
	DateFromFilename = "filename" // a date in the file's name (IMG_20190704_153000)
	DateFromMtime    = "mtime"    // the file's modification time: a copy or backup resets it, so it is weak
	// DateFromMessage is when the photo was sent in a chat: it was taken then
	// or before.
	DateFromMessage = "message"
	// DateFromUpload is when the photo was posted to a site such as
	// Facebook: it was taken then or before.
	DateFromUpload = "upload"
	// DateFromGooglePhotos is Google Photos' own record of when the photo
	// was taken (photoTakenTime in a Takeout sidecar). Google usually read it
	// from the camera before the file lost it, but for a file with no camera
	// date it is when the file reached Google, so it is weaker than exif.
	DateFromGooglePhotos = "google_photos"
	DateUnknown          = "unknown"
)

// DateSources is every date source a photo can have.
var DateSources = []string{DateFromExif, DateFromFilename, DateFromMtime, DateFromMessage, DateFromUpload, DateFromGooglePhotos, DateUnknown}

func knownDateSource(source string) bool {
	for _, known := range DateSources {
		if source == known {
			return true
		}
	}
	return false
}

// TakenActivityIntervals are the span sizes GET /taken-activity counts in.
// A week starts on Monday.
var TakenActivityIntervals = []string{"day", "week", "month", "year"}
