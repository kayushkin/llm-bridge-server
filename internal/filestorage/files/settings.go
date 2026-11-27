package files

import (
	"fmt"
	"mime"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The two behavior settings the store reads at the time of use. The server it
// is mounted in declares them and stores them; this package says what a
// valid value is, so a value the API would refuse cannot be put in force by
// any other path either.
const (
	// DefaultMaximumFileBytes is the largest upload taken with nothing set:
	// 25 MiB.
	DefaultMaximumFileBytes = "26214400"
	// DefaultInlineContentTypes are the declared types served for display
	// with nothing set.
	DefaultInlineContentTypes = "image/png,image/jpeg,image/gif,image/webp,application/pdf,text/plain"
)

// contentTypesABrowserExecutes may never be served inline: an uploaded page
// shown from the dashboard's origin runs with the dashboard's cookies.
var contentTypesABrowserExecutes = map[string]bool{
	"text/html": true, "application/xhtml+xml": true, "image/svg+xml": true,
	"text/xml": true, "application/xml": true,
	"text/javascript": true, "application/javascript": true, "application/ecmascript": true,
}

// ValidateMaximumFileBytes refuses a size limit that is not a positive number of bytes.
func ValidateMaximumFileBytes(value string) error {
	bytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || bytes <= 0 {
		return fmt.Errorf("%w: the maximum file size must be a positive number of bytes, got %q", servicesettings.ErrInvalidSettingValue, value)
	}
	return nil
}

// ValidateInlineContentTypes refuses an inline list holding anything but bare
// media types, or a type a browser runs.
func ValidateInlineContentTypes(value string) error {
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		mediaType, parameters, err := mime.ParseMediaType(entry)
		if err != nil || len(parameters) > 0 || mediaType != entry {
			return fmt.Errorf("%w: %q is not a bare lowercase media type such as image/png", servicesettings.ErrInvalidSettingValue, entry)
		}
		if contentTypesABrowserExecutes[mediaType] {
			return fmt.Errorf("%w: %s is a type a browser runs, and a file of it shown inline would run as the dashboard", servicesettings.ErrInvalidSettingValue, mediaType)
		}
	}
	return nil
}
