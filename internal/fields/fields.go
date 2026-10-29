// Package fields reads a page's Field definitions from the JSON Schema that
// @edity/core generates for them, src/content/<page>.schema.json in the Site
// repository, and checks the page's content against them. It tells which
// content paths name a Field or a List, and lists what content breaks of the
// Field definitions in the terms the Editor uses, so that the server accepts
// only content the Site's build accepts.
//
// The schema is in the Edity dialect of JSON Schema 2020-12, whose
// meta-schemas and conformance fixtures come from
// github.com/RMYPG/edity/contracts. This package implements the dialect's
// Limits vocabulary.
package fields

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RMYPG/edity/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Definitions are a page's Field definitions, compiled from its generated
// JSON Schema. They are safe for concurrent use.
type Definitions struct {
	schema *jsonschema.Schema
}

// pageURL is where Compile adds a page's schema to its compiler. Nothing
// refers to it, so any URL would do.
const pageURL = "urn:edity:page"

// Compile reads a page's generated JSON Schema. It refuses a schema that
// names a dialect other than Edity's in "$schema", whose Limits a validator
// would ignore, and one the dialect does not accept, such as one with an
// x-edity- keyword the Limits vocabulary does not define. A schema that names
// no dialect is read in Edity's.
//
// The schema can refer only to the dialect's meta-schemas, which come from
// the contracts module: Compile never loads a file or a URL.
func Compile(schema []byte) (*Definitions, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("reading the page's schema: %w", err)
	}
	if obj, ok := doc.(map[string]any); ok {
		dialect, named := obj["$schema"]
		if !named {
			obj["$schema"] = contracts.DialectURL
		} else if dialect != contracts.DialectURL {
			return nil, fmt.Errorf("the page's schema is in the dialect %v, not in Edity's %s", dialect, contracts.DialectURL)
		}
		wholeNumbersAsDecimals(obj)
	}

	vocabulary, err := limitsVocabulary()
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	c.RegisterVocabulary(vocabulary)
	if err := addMetaSchemas(c); err != nil {
		return nil, err
	}
	if err := c.AddResource(pageURL, doc); err != nil {
		return nil, err
	}
	compiled, err := c.Compile(pageURL)
	if err != nil {
		return nil, fmt.Errorf("compiling the page's schema: %w", err)
	}
	return &Definitions{schema: compiled}, nil
}

// wholeNumbersAsDecimals reads "type": "integer", which @edity/core writes
// for a number Field that allows no decimals, as "type": "number" with
// "x-edity-decimals": 0. For a double the two mean the same, and the Site's
// build reads numbers as doubles. But the validator stops at a "type" that
// fails, while core checks a number's range and its decimals apart: 25.5,
// where 1 to 20 whole numbers are allowed, breaks both max and decimals. It
// walks the subschemas of a page's Fields, groups, Lists and Items.
func wholeNumbersAsDecimals(schema map[string]any) {
	if _, decimals := schema["x-edity-decimals"]; schema["type"] == "integer" && !decimals {
		schema["type"] = "number"
		schema["x-edity-decimals"] = json.Number("0")
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, property := range properties {
			if property, ok := property.(map[string]any); ok {
				wholeNumbersAsDecimals(property)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		wholeNumbersAsDecimals(items)
	}
}

func addMetaSchemas(c *jsonschema.Compiler) error {
	for _, meta := range contracts.MetaSchemas() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(meta.JSON))
		if err != nil {
			return fmt.Errorf("reading the meta-schema %s: %w", meta.URL, err)
		}
		if err := c.AddResource(meta.URL, doc); err != nil {
			return err
		}
	}
	return nil
}

// noLoader refuses to load any schema the compiler was not given.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("%s is not one of the Edity dialect's meta-schemas", url)
}

// Kind is what a content path names in a page's Field definitions.
type Kind int

const (
	// NotEditable is a path that names nothing a Change can edit: no key of
	// the Field definitions, a group of Fields, an Item made of Fields, or the
	// page itself.
	NotEditable Kind = iota
	// Field is a path that names a text or a number.
	Field
	// List is a path that names a List.
	List
)

// KindOf tells what a content path names: its keys and Item indices,
// dot-separated, as the Editor writes a Change's path (hero.title,
// services.2, plans.1.features.0). It reads the path through the schema's
// "properties" and "items" only, so plans.7.price is a Field however many
// Items the List holds in the content.
func (d *Definitions) KindOf(path string) Kind {
	s := d.schema
	if path != "" {
		for key := range strings.SplitSeq(path, ".") {
			switch {
			case is(s, "object"):
				s = s.Properties[key]
			case is(s, "array") && isIndex(key):
				s = s.Items2020
			default:
				s = nil
			}
			if s == nil {
				return NotEditable
			}
		}
	}
	switch {
	case is(s, "string"), is(s, "number"), is(s, "integer"):
		return Field
	case is(s, "array"):
		return List
	default:
		return NotEditable
	}
}

// is tells whether s allows only values of type t.
func is(s *jsonschema.Schema, t string) bool {
	if s.Types == nil {
		return false
	}
	types := s.Types.ToStrings()
	return len(types) == 1 && types[0] == t
}

// isIndex tells whether key is an Item's index, written as the Editor writes
// it: digits with no leading zero, so that one Item has one path.
func isIndex(key string) bool {
	if key == "" || (key[0] == '0' && len(key) > 1) {
		return false
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
