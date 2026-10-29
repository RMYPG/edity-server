package fields_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity/contracts"
)

// Every conformance fixture of the contract version go.mod pins: content is
// valid when it has no violations. Each instance is decoded both ways
// Violations takes content, with numbers as json.Number and as float64.
func TestFixtures(t *testing.T) {
	fixtures, err := contracts.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"fixtures/keywords/", "fixtures/pages/"} {
		if !slices.ContainsFunc(fixtures, func(f contracts.Fixture) bool { return strings.HasPrefix(f.Path, dir) }) {
			t.Errorf("no fixtures in %s", dir)
		}
	}
	for _, fixture := range fixtures {
		t.Run(strings.TrimPrefix(fixture.Path, "fixtures/"), func(t *testing.T) {
			for _, c := range fixture.Cases {
				t.Run(c.Description, func(t *testing.T) {
					defs, err := fields.Compile(c.Schema)
					if err != nil {
						t.Fatal(err)
					}
					for _, test := range c.Tests {
						t.Run(test.Description, func(t *testing.T) {
							found := defs.Violations(decodeNumbers(t, test.Data))
							if valid := len(found) == 0; valid != test.Valid {
								t.Errorf("valid = %v, want %v (violations %v)", valid, test.Valid, found)
							}
							if asFloats := defs.Violations(decodeFloats(t, test.Data)); !slices.Equal(asFloats, found) {
								t.Errorf("with numbers as float64, violations %v, want %v", asFloats, found)
							}
						})
					}
				})
			}
		})
	}
}

// core-violations.json holds what @edity/core's violations() finds in each
// Draft of the page fixtures, as core-violations.mjs writes it. The Editor
// checks a Draft with it, so the server must name the same violations.
func TestViolationsAreTheOnesCoreFinds(t *testing.T) {
	b, err := os.ReadFile("testdata/core-violations.json")
	if err != nil {
		t.Fatal(err)
	}
	var core map[string]map[string]map[string][]fields.Violation
	if err := json.Unmarshal(b, &core); err != nil {
		t.Fatal(err)
	}
	fixtures, err := contracts.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	const regenerate = "write testdata/core-violations.json again for this contract version, as core-violations.mjs says"
	seen := 0
	for _, fixture := range fixtures {
		if !strings.HasPrefix(fixture.Path, "fixtures/pages/") {
			continue
		}
		for _, c := range fixture.Cases {
			defs, err := fields.Compile(c.Schema)
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range c.Tests {
				want, ok := core[fixture.Path][c.Description][test.Description]
				if !ok {
					t.Errorf("%s: %s: %s: core's violations are not known: %s", fixture.Path, c.Description, test.Description, regenerate)
					continue
				}
				seen++
				got := defs.Violations(decodeNumbers(t, test.Data))
				if !sameViolations(got, want) {
					t.Errorf("%s: %s: %s: violations %v, core finds %v", fixture.Path, c.Description, test.Description, got, want)
				}
			}
		}
	}
	total := 0
	for _, cases := range core {
		for _, tests := range cases {
			total += len(tests)
		}
	}
	if seen != total {
		t.Errorf("core-violations.json holds %d Drafts, of which %d are in the page fixtures: %s", total, seen, regenerate)
	}
}

// sameViolations compares violations in any order: core lists them in the
// order of the Field definitions.
func sameViolations(got, want []fields.Violation) bool {
	key := func(v fields.Violation) string { return v.Path + "\x00" + string(v.Limit) }
	g, w := make([]string, len(got)), make([]string, len(want))
	for i, v := range got {
		g[i] = key(v)
	}
	for i, v := range want {
		w[i] = key(v)
	}
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// The page of fixtures/pages/lists.json and fixtures/pages/groups.json, in
// one.
const page = `{
  "$schema": "https://github.com/RMYPG/edity/contracts/v1/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["hero", "bullets", "plans"],
  "properties": {
    "hero": {
      "type": "object",
      "additionalProperties": false,
      "required": ["title", "cta"],
      "properties": {
        "title": {"type": "string", "pattern": "^[\\t\\v\\f\\p{Zs}]*[^\\t\\v\\f\\p{Zs}\\n\\r\\p{Zl}\\p{Zp}][^\\n\\r\\p{Zl}\\p{Zp}]*$", "x-edity-maxGraphemes": 20},
        "cta": {
          "type": "object",
          "additionalProperties": false,
          "required": ["label"],
          "properties": {"label": {"type": "string", "x-edity-maxGraphemes": 10}}
        }
      }
    },
    "bullets": {
      "type": "array", "minItems": 0, "maxItems": 2,
      "items": {"type": "string", "x-edity-maxGraphemes": 3}
    },
    "plans": {
      "type": "array", "minItems": 1, "maxItems": 2,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "sessions", "price", "features"],
        "properties": {
          "name": {"type": "string", "x-edity-maxGraphemes": 12},
          "sessions": {"type": "integer", "minimum": 1, "maximum": 20},
          "price": {"type": "number", "minimum": 0, "maximum": 500, "x-edity-decimals": 2},
          "features": {
            "type": "array", "minItems": 1, "maxItems": 2,
            "items": {"type": "string", "x-edity-maxGraphemes": 5}
          }
        }
      }
    }
  }
}`

func TestKindOf(t *testing.T) {
	defs, err := fields.Compile([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fields.Kind{
		"hero.title":         fields.Field,
		"hero.cta.label":     fields.Field,
		"bullets":            fields.List,
		"bullets.0":          fields.Field,
		"bullets.12":         fields.Field,
		"plans":              fields.List,
		"plans.1.name":       fields.Field,
		"plans.1.sessions":   fields.Field,
		"plans.1.price":      fields.Field,
		"plans.1.features":   fields.List,
		"plans.1.features.0": fields.Field,
		// Beyond the bounds of the List, which only the content can tell.
		"plans.7.price": fields.Field,

		"":                     fields.NotEditable,
		"hero":                 fields.NotEditable,
		"hero.cta":             fields.NotEditable,
		"plans.1":              fields.NotEditable,
		"footer":               fields.NotEditable,
		"hero.subtitle":        fields.NotEditable,
		"hero.title.x":         fields.NotEditable,
		"bullets.0.x":          fields.NotEditable,
		"bullets.title":        fields.NotEditable,
		"plans.x.name":         fields.NotEditable,
		"plans.-1.name":        fields.NotEditable,
		"plans.01.name":        fields.NotEditable,
		"plans.1.features.00":  fields.NotEditable,
		"plans..name":          fields.NotEditable,
		"hero.":                fields.NotEditable,
		".hero.title":          fields.NotEditable,
		"properties.hero":      fields.NotEditable,
		"plans.items.name":     fields.NotEditable,
		"plans.1.features.0.0": fields.NotEditable,
	} {
		if got := defs.KindOf(path); got != want {
			t.Errorf("KindOf(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestViolations(t *testing.T) {
	defs, err := fields.Compile([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		description string
		content     string
		want        []fields.Violation
	}{
		{
			description: "every Field within its Limits",
			content: `{"hero": {"title": "Fisioterapia", "cta": {"label": "Reservar"}}, "bullets": [],
				"plans": [{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]}]}`,
		},
		{
			description: "several violations, ordered by path with Item indices as numbers",
			content: `{"hero": {"title": "Fisio\nterapia en casa, por fin", "cta": {}, "subtitle": "Hola"},
				"bullets": ["Sí", "No", "Ya"], "footer": "©",
				"plans": [
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Wifi"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": ["Parking"]},
					{"name": "Bono 5", "sessions": 5, "price": 225.5, "features": []}
				]}`,
			want: []fields.Violation{
				{Path: "bullets", Limit: fields.MaxItems},
				{Path: "footer", Limit: fields.Unknown},
				{Path: "hero.cta.label", Limit: fields.Type},
				{Path: "hero.subtitle", Limit: fields.Unknown},
				{Path: "hero.title", Limit: fields.OneLine},
				{Path: "hero.title", Limit: fields.MaxChars},
				{Path: "plans", Limit: fields.MaxItems},
				{Path: "plans.9.features.0", Limit: fields.MaxChars},
				{Path: "plans.10.features", Limit: fields.MinItems},
			},
		},
		{
			// core's verdict: the type of a whole number does not hide its
			// range.
			description: "a number with decimals out of the range of whole numbers",
			content: `{"hero": {"title": "Fisioterapia", "cta": {"label": "Reservar"}}, "bullets": [],
				"plans": [{"name": "Bono 5", "sessions": 25.5, "price": -0.125, "features": ["Wifi"]}]}`,
			want: []fields.Violation{
				{Path: "plans.0.price", Limit: fields.Min},
				{Path: "plans.0.price", Limit: fields.Decimals},
				{Path: "plans.0.sessions", Limit: fields.Max},
				{Path: "plans.0.sessions", Limit: fields.Decimals},
			},
		},
		{
			description: "whole numbers written with a fraction or an exponent",
			content: `{"hero": {"title": "Fisioterapia", "cta": {"label": "Reservar"}}, "bullets": [],
				"plans": [{"name": "Bono 5", "sessions": 5.0, "price": 2e2, "features": ["Wifi"]}]}`,
		},
		{
			// The build reads 1e400 as Infinity, which core takes for a
			// value that is not a number.
			description: "a number too large for a double",
			content: `{"hero": {"title": "Fisioterapia", "cta": {"label": "Reservar"}}, "bullets": [],
				"plans": [{"name": "Bono 5", "sessions": 5, "price": 1e400, "features": ["Wifi"]}]}`,
			want: []fields.Violation{{Path: "plans.0.price", Limit: fields.Type}},
		},
		{
			description: "a page that is not an object",
			content:     `["hero"]`,
			want:        []fields.Violation{{Path: "", Limit: fields.Type}},
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			got := defs.Violations(decodeNumbers(t, []byte(test.content)))
			if !slices.Equal(got, test.want) {
				t.Errorf("violations %v, want %v", got, test.want)
			}
		})
	}
}

// A Violation is an entry of the violations in a 422 answer to a Publish.
func TestViolationJSON(t *testing.T) {
	b, err := json.Marshal(fields.Violation{Path: "plans.1.features.0", Limit: fields.MaxChars})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"path":"plans.1.features.0","limit":"maxChars"}`; string(b) != want {
		t.Errorf("%s, want %s", b, want)
	}
}

// A schema that names no dialect is read in Edity's, so its Limits hold.
func TestASchemaWithNoDialectIsReadInEditys(t *testing.T) {
	defs, err := fields.Compile([]byte(`{"type": "string", "x-edity-maxGraphemes": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := defs.Violations("ab"), []fields.Violation{{Path: "", Limit: fields.MaxChars}}; !slices.Equal(got, want) {
		t.Errorf("violations %v, want %v", got, want)
	}
}

func TestCompileRefuses(t *testing.T) {
	for description, schema := range map[string]string{
		"a schema that is not JSON":          `{"type": "object"`,
		"two JSON values":                    `{"type": "object"} {}`,
		"another dialect":                    `{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "string", "x-edity-maxGraphemes": 1}`,
		"a dialect that is not a string":     `{"$schema": 1, "type": "string"}`,
		"a keyword the vocabulary lacks":     `{"$schema": "https://github.com/RMYPG/edity/contracts/v1/schema", "x-edity-maxChars": 10}`,
		"a Limit with a value it can't take": `{"$schema": "https://github.com/RMYPG/edity/contracts/v1/schema", "properties": {"title": {"x-edity-maxGraphemes": 0}}}`,
		"a reference to a file":              `{"$schema": "https://github.com/RMYPG/edity/contracts/v1/schema", "$ref": "file:///etc/hostname"}`,
		"a reference to a URL":               `{"$schema": "https://github.com/RMYPG/edity/contracts/v1/schema", "$ref": "https://example.com/schema.json"}`,
	} {
		t.Run(description, func(t *testing.T) {
			if _, err := fields.Compile([]byte(schema)); err == nil {
				t.Error("compiled")
			}
		})
	}
}

// decodeNumbers decodes JSON as encoding/json does with UseNumber.
func decodeNumbers(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// decodeFloats decodes JSON as encoding/json does by default.
func decodeFloats(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
