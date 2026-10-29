package content

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// emoji matches what Prettier's emoji-regex matches. Go's regexp matches
// leftmost-first, as JavaScript's does, so it finds the same emoji.
var emoji = regexp.MustCompile(strings.Join(emojiPattern, "|"))

// width is how many columns Prettier counts text as when it fits a line in
// its print width, as getStringWidth in its src/utilities/get-string-width.js
// does: an emoji counts two, or one if it is narrow, an East Asian wide or
// fullwidth character two, a control character, a combining diacritic or a
// variation selector none, and any other character one.
func width(text string) int {
	if isPlainASCII(text) {
		return len(text)
	}
	columns := 0
	if strings.IndexFunc(text, isEmojiStart) >= 0 {
		text = emoji.ReplaceAllStringFunc(text, func(match string) string {
			if r, size := utf8.DecodeRuneInString(match); size == len(match) && isNarrowEmoji(r) {
				columns++
			} else {
				columns += 2
			}
			return ""
		})
	}
	for _, r := range text {
		switch {
		case r <= 0x1F, r >= 0x7F && r <= 0x9F:
		case r >= 0x300 && r <= 0x36F:
		case r >= 0xFE00 && r <= 0xFE0F:
		case inRanges(fullwidthRanges, r), inRanges(wideRanges, r):
			columns += 2
		default:
			columns++
		}
	}
	return columns
}

// isPlainASCII tells whether text has only the characters from space to
// DEL, which Prettier counts one column each without looking further.
func isPlainASCII(text string) bool {
	for i := range len(text) {
		if text[i] < 0x20 || text[i] > 0x7F {
			return false
		}
	}
	return true
}

func isEmojiStart(r rune) bool {
	_, found := slices.BinarySearch(emojiStarts, r)
	return found
}

func isNarrowEmoji(r rune) bool {
	_, found := slices.BinarySearch(narrowEmoji, r)
	return found
}

// inRanges tells whether r is in one of ranges, a sorted list of the first
// and last code point of each range.
func inRanges(ranges []rune, r rune) bool {
	// The first range that ends at r or after it.
	n := len(ranges) / 2
	i := sort.Search(n, func(i int) bool { return ranges[2*i+1] >= r })
	return i < n && ranges[2*i] <= r
}
