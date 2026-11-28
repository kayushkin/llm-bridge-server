package possessions

// What the external tests (package possessions_test) reach of the
// package's own workings.

var (
	AlignOriginal       = alignOriginal
	OrientationMatching = orientationMatching
	Resized             = resized
)

const SmallPieceLongestSide = smallPieceLongestSide

// SetOriginalsForTest makes the store keep originals in originals.
func (s *Store) SetOriginalsForTest(originals OriginalFiles) { s.originals = originals }
