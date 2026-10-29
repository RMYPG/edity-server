package content

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// prettier.json holds what Prettier makes of the cases prettier.mjs
// generates, with the Prettier the Sites run.
type prettierCases struct {
	Prettier string
	// Widths are runs of code points that are as wide alone: first, last
	// and width.
	Widths [][3]rune
	// Texts are text and its width.
	Texts [][2]json.RawMessage
	// Numbers are a JSON number and how JavaScript writes the double it
	// reads as.
	Numbers [][2]string
	// Files are content files as Prettier formats them.
	Files []string
	// Written is a content file whose numbers Prettier rewrites, and how it
	// formats it.
	Written [2]string
}

func readPrettierCases(t *testing.T) prettierCases {
	t.Helper()
	b, err := os.ReadFile("testdata/prettier.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases prettierCases
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if cases.Prettier != prettierVersion {
		t.Fatalf("testdata/prettier.json is from Prettier %s, emoji.go from %s: run both scripts with one Prettier", cases.Prettier, prettierVersion)
	}
	return cases
}

func TestWidthIsPrettiers(t *testing.T) {
	cases := readPrettierCases(t)
	for _, run := range cases.Widths {
		for r := run[0]; r <= run[1]; r++ {
			if got := width(string(r)); got != int(run[2]) {
				t.Errorf("width(%U) = %d, Prettier's %d", r, got, run[2])
			}
		}
	}
	for _, c := range cases.Texts {
		var text string
		var want int
		if err := json.Unmarshal(c[0], &text); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(c[1], &want); err != nil {
			t.Fatal(err)
		}
		if got := width(text); got != want {
			t.Errorf("width(%+q) = %d, Prettier's %d", text, got, want)
		}
	}
}

func TestFormatIsPrettiers(t *testing.T) {
	cases := readPrettierCases(t)
	for _, file := range cases.Files {
		v, err := Parse([]byte(file))
		if err != nil {
			t.Fatalf("Parse: %v\n%s", err, file)
		}
		if got := string(v.Format()); got != file {
			t.Errorf("Format:\n%s\nPrettier:\n%s", got, file)
		}
		// Each file holds only what a Change can, as JSON.stringify writes
		// it, which is how ParseChange writes it too.
		v, err = ParseChange([]byte(file))
		if err != nil {
			t.Fatalf("ParseChange: %v\n%s", err, file)
		}
		if got := string(v.Format()); got != file {
			t.Errorf("Format after ParseChange:\n%s\nPrettier:\n%s", got, file)
		}
	}
}

func TestNumbersAreWrittenAsPrettierWritesThem(t *testing.T) {
	cases := readPrettierCases(t)
	v, err := Parse([]byte(cases.Written[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(v.Format()); got != cases.Written[1] {
		t.Errorf("Format:\n%s\nPrettier:\n%s", got, cases.Written[1])
	}
}

func TestAChangesNumbersAreWrittenAsJavaScriptWritesThem(t *testing.T) {
	cases := readPrettierCases(t)
	for _, c := range cases.Numbers {
		text, javaScript := c[0], c[1]
		if javaScript == "Infinity" || javaScript == "-Infinity" {
			if got := javaScriptNumber(text); got != prettierNumber(text) {
				t.Errorf("javaScriptNumber(%s) = %s, want it as it is: no double holds it", text, got)
			}
			continue
		}
		// Prettier then drops the + of an exponent.
		if got, want := javaScriptNumber(text), prettierNumber(javaScript); got != want {
			t.Errorf("javaScriptNumber(%s) = %s, want %s", text, got, want)
		}
		if f, _ := strconv.ParseFloat(javaScriptNumber(text), 64); f != parse(t, text) {
			t.Errorf("javaScriptNumber(%s) reads back as %v", text, f)
		}
	}
}

func parse(t *testing.T, text string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) {
		t.Fatalf("%s: %v", text, err)
	}
	return f
}

func TestParseRefusesAKeyTwice(t *testing.T) {
	for _, file := range []string{
		`{"hero": {"title": "a", "title": "b"}}`,
		`{"hero": {}, "hero": {}}`,
	} {
		if _, err := Parse([]byte(file)); err == nil {
			t.Errorf("Parse(%s) took a key twice", file)
		}
	}
}

func TestParseRefusesWhatIsNotOneJSONDocument(t *testing.T) {
	for _, file := range []string{``, `{`, `{"a": 1} {}`, `{"a": 1}]`, `{"a": 01}`, "{\"a\": \"\xff\"}"} {
		if _, err := Parse([]byte(file)); err == nil {
			t.Errorf("Parse(%q) took it", file)
		}
	}
}

func TestParseKeepsWhatAFileHas(t *testing.T) {
	file := `{"z": "café", "a": [true, null, 1.50E+2], "m": {}}`
	v, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "z": "café",
  "a": [true, null, 1.5e2],
  "m": {}
}
`
	if got := string(v.Format()); got != want {
		t.Errorf("Format:\n%s\nwant:\n%s", got, want)
	}
}

func TestParseChangeTakesOnlyContentValues(t *testing.T) {
	for _, value := range []string{`null`, `true`, `[1, false]`, `{"a": null}`, `"a" "b"`} {
		if _, err := ParseChange([]byte(value)); err == nil {
			t.Errorf("ParseChange(%s) took it", value)
		}
	}
	for value, want := range map[string]string{
		`"café \/ \u0007"`:   `"café / \u0007"`,
		`49.50`:              `49.5`,
		`1E21`:               `1e21`,
		`-0`:                 `0`,
		`1e400`:              `1e400`,
		`[1.0, {"b": "\t"}]`: `[1, { "b": "\t" }]`,
	} {
		v, err := ParseChange([]byte(value))
		if err != nil {
			t.Fatalf("ParseChange(%s): %v", value, err)
		}
		got := v.text
		if v.kind == '[' {
			got = "[" + v.elems[0].text + ", { \"b\": " + v.elems[1].elems[0].text + " }]"
		}
		if got != want {
			t.Errorf("ParseChange(%s) = %s, want %s", value, got, want)
		}
	}
}

func TestAtAndReplaceFollowAContentPath(t *testing.T) {
	v, err := Parse([]byte(`{"hero": {"title": "a"}, "plans": [{"price": 1, "features": ["x", "y"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"hero.title":         `"a"`,
		"plans.0.price":      `1`,
		"plans.0.features.1": `"y"`,
		"plans.0.features":   `["x", "y"]`,
	} {
		got, ok := v.At(path)
		if !ok || flat(got) != want {
			t.Errorf("At(%s) = %v, %v, want %s", path, got, ok, want)
		}
	}
	for _, path := range []string{"hero.lead", "plans.1.price", "plans.00.price", "plans.-1", "plans.x", "hero.title.x", "plans.0.features.2", "nothing"} {
		if got, ok := v.At(path); ok {
			t.Errorf("At(%s) = %v, want none", path, flat(got))
		}
	}

	after, _ := ParseChange([]byte(`"b"`))
	if !v.Replace("plans.0.features.0", after) {
		t.Fatal("Replace(plans.0.features.0) found nothing")
	}
	if v.Replace("plans.0.features.2", after) {
		t.Error("Replace(plans.0.features.2) replaced an Item past the List's end")
	}
	if got, _ := v.At("plans.0.features"); flat(got) != `["b", "y"]` {
		t.Errorf("after Replace, plans.0.features = %s", flat(got))
	}
}

func TestEqualComparesAsTheEditorDoes(t *testing.T) {
	same := [][2]string{
		{`"café"`, `"café"`},
		{`49.5`, `49.50`},
		{`100`, `1e2`},
		{`0`, `-0`},
		{`["a", 1]`, `["a", 1.0]`},
		{`{"a": 1, "b": [2]}`, `{"b": [2], "a": 1}`},
	}
	different := [][2]string{
		{`"a"`, `"A"`},
		{`1`, `"1"`},
		{`["a", "b"]`, `["b", "a"]`},
		{`["a"]`, `["a", "a"]`},
		{`{"a": 1}`, `{"a": 1, "b": 1}`},
		{`{"a": 1}`, `{"b": 1}`},
		{`[]`, `{}`},
	}
	value := func(s string) *Value {
		v, err := Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, pair := range same {
		if !Equal(value(pair[0]), value(pair[1])) || !Equal(value(pair[1]), value(pair[0])) {
			t.Errorf("%s and %s are not Equal", pair[0], pair[1])
		}
	}
	for _, pair := range different {
		if Equal(value(pair[0]), value(pair[1])) || Equal(value(pair[1]), value(pair[0])) {
			t.Errorf("%s and %s are Equal", pair[0], pair[1])
		}
	}
}

func TestDecodedIsWhatEncodingJSONDecodes(t *testing.T) {
	file := `{"a": "café", "b": [1.5, -2, {"c": true, "d": null}], "e": false}`
	v, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(file))
	dec.UseNumber()
	var want any
	if err := dec.Decode(&want); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v.Decoded())
	wanted, _ := json.Marshal(want)
	if string(got) != string(wanted) {
		t.Errorf("Decoded = %s, want %s", got, wanted)
	}
}
