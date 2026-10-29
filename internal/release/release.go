// Package release names a Site's Releases by their versions, X.Y.Z, which
// the Site's package.json holds and whose vX.Y.Z tag release.yml pushes.
package release

import (
	"fmt"
	"regexp"
	"strconv"
)

// A Version is a Release's version: its major, minor and patch numbers.
type Version [3]uint64

// pattern is the OpenAPI file's pattern for a version: X.Y.Z, with no
// leading zeros.
var pattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseVersion reads a version written X.Y.Z, such as 1.4.3, and returns
// false for anything else.
func ParseVersion(s string) (Version, bool) {
	parts := pattern.FindStringSubmatch(s)
	if parts == nil {
		return Version{}, false
	}
	var v Version
	for i := range v {
		n, err := strconv.ParseUint(parts[i+1], 10, 64)
		if err != nil {
			return Version{}, false
		}
		v[i] = n
	}
	return v, true
}

// Less tells whether v is a lower version than w.
func (v Version) Less(w Version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

// NextPatch returns v with its patch one higher, and false when the patch
// can't go higher.
func (v Version) NextPatch() (Version, bool) {
	if v[2] == ^uint64(0) {
		return Version{}, false
	}
	v[2]++
	return v, true
}

// String writes v as X.Y.Z.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// MarshalText writes v as X.Y.Z, as the API answers it.
func (v Version) MarshalText() ([]byte, error) {
	return []byte(v.String()), nil
}
