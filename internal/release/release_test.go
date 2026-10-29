package release

import (
	"encoding/json"
	"testing"
)

func TestAVersionIsXYZWithNoLeadingZeros(t *testing.T) {
	for s, want := range map[string]Version{
		"1.4.3":    {1, 4, 3},
		"0.0.0":    {0, 0, 0},
		"10.20.30": {10, 20, 30},
	} {
		if v, ok := ParseVersion(s); !ok || v != want || v.String() != s {
			t.Errorf("ParseVersion(%q) = %v, %t, want %v", s, v, ok, want)
		}
	}
	for _, s := range []string{"", "1.4", "1.4.3.2", "v1.4.3", "01.4.3", "1.04.3", "1.4.03", "1.4.3-rc.1", " 1.4.3", "1.4.99999999999999999999"} {
		if v, ok := ParseVersion(s); ok {
			t.Errorf("ParseVersion(%q) = %v, want none", s, v)
		}
	}
}

func TestVersionsCompareNumberByNumber(t *testing.T) {
	ordered := []Version{{0, 9, 9}, {1, 0, 0}, {1, 0, 9}, {1, 0, 10}, {1, 2, 0}, {2, 0, 0}}
	for i, v := range ordered {
		for j, w := range ordered {
			if got := v.Less(w); got != (i < j) {
				t.Errorf("%s.Less(%s) = %t", v, w, got)
			}
		}
	}
}

func TestTheNextPatch(t *testing.T) {
	if v, ok := (Version{1, 4, 9}).NextPatch(); !ok || v != (Version{1, 4, 10}) {
		t.Errorf("1.4.9's next patch = %s, %t, want 1.4.10", v, ok)
	}
	if v, ok := (Version{1, 4, ^uint64(0)}).NextPatch(); ok {
		t.Errorf("the highest patch's next = %s, want none", v)
	}
}

func TestAVersionIsAnsweredAsText(t *testing.T) {
	b, err := json.Marshal(struct {
		Version Version `json:"version"`
	}{Version{1, 4, 3}})
	if err != nil || string(b) != `{"version":"1.4.3"}` {
		t.Errorf("JSON = %s, %v", b, err)
	}
}
