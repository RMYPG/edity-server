package fields

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// A Violation is a rule of a page's Field definitions that its content
// breaks, at Path: keys and Item indices, dot-separated, as in a Change's
// path, or "" for the page itself. It is one entry of the violations in a
// 422 answer to a Publish.
type Violation struct {
	Path  string `json:"path"`
	Limit Limit  `json:"limit"`
}

// A Limit names what a Violation breaks, with the rule names of @edity/core's
// Violation, so the Editor handles the server's verdicts as its own.
type Limit string

// The Limits a Violation can name, in the order Violations lists them at one
// path.
const (
	// Type is a value that is not what the Field definitions hold there at
	// all: text, a number, a List's Items or an object's Fields. A missing
	// Field is one, as core counts it.
	Type Limit = "type"
	// Unknown is a key the Field definitions do not have.
	Unknown Limit = "unknown"
	// OneLine is text with no visible character, or with a line break.
	OneLine Limit = "oneLine"
	// MaxChars is text with more visible characters than its Limit.
	MaxChars Limit = "maxChars"
	// Min and Max are a number out of its range.
	Min Limit = "min"
	Max Limit = "max"
	// Decimals is a number with more decimal places than its Limit.
	Decimals Limit = "decimals"
	// MinItems and MaxItems are a List with fewer or more Items than its
	// bounds.
	MinItems Limit = "minItems"
	MaxItems Limit = "maxItems"
)

var limitOrder = []Limit{Type, Unknown, OneLine, MaxChars, Min, Max, Decimals, MinItems, MaxItems}

// Violations lists every rule of the Field definitions that content breaks,
// ordered by path, or none when the Site's build accepts it. Line Limits are
// never checked: only a browser can measure lines.
//
// content is a page's content as encoding/json decodes it into an any, with
// numbers as float64 or, with UseNumber, as json.Number.
func (d *Definitions) Violations(content any) []Violation {
	err := d.schema.Validate(content)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		// Validate returns only validation errors, but content that fails
		// is never accepted, whatever the error.
		return []Violation{{Path: "", Limit: Type}}
	}
	found := violationsOf(verr, nil)
	slices.SortFunc(found, compareViolations)
	// A value that is not what the Field definitions hold breaks no other
	// rule at its path, as core counts it, and a Type sorts first: a number
	// too large for a double is not also over its max.
	kept := found[:0]
	for _, v := range found {
		if n := len(kept); n > 0 && kept[n-1].Path == v.Path && (kept[n-1].Limit == Type || kept[n-1].Limit == v.Limit) {
			continue
		}
		kept = append(kept, v)
	}
	return kept
}

// violationsOf names the failures of a validation error, which are the
// leaves of its tree of causes.
func violationsOf(e *jsonschema.ValidationError, found []Violation) []Violation {
	if len(e.Causes) > 0 {
		for _, cause := range e.Causes {
			found = violationsOf(cause, found)
		}
		return found
	}
	at := func(keys ...string) string {
		return strings.Join(append(slices.Clone(e.InstanceLocation), keys...), ".")
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		for _, key := range k.Missing {
			found = append(found, Violation{at(key), Type})
		}
		return found
	case *kind.AdditionalProperties:
		for _, key := range k.Properties {
			found = append(found, Violation{at(key), Unknown})
		}
		return found
	}
	return append(found, Violation{at(), limitOf(e.ErrorKind)})
}

// limitOf names a failure at the value it is about.
func limitOf(k jsonschema.ErrorKind) Limit {
	switch k.(type) {
	case *kind.Type, *notADouble:
		return Type
	case *kind.Pattern:
		// The one pattern of a page's schema, which every text Field has.
		return OneLine
	case *tooManyGraphemes:
		return MaxChars
	case *kind.Minimum:
		return Min
	case *kind.Maximum:
		return Max
	case *tooManyDecimals:
		return Decimals
	case *kind.MinItems:
		return MinItems
	case *kind.MaxItems:
		return MaxItems
	default:
		// Generated schemas use no other keyword that fails, so this is a
		// schema written by hand, or the dialect's meta-schema checking a
		// schema: a value the Field definitions cannot hold.
		return Type
	}
}

// compareViolations orders violations by path, key by key and Item indices
// by number, and then as limitOrder does.
func compareViolations(a, b Violation) int {
	if c := slices.CompareFunc(strings.Split(a.Path, "."), strings.Split(b.Path, "."), compareKeys); c != 0 {
		return c
	}
	return cmp.Compare(slices.Index(limitOrder, a.Limit), slices.Index(limitOrder, b.Limit))
}

func compareKeys(a, b string) int {
	if isIndex(a) && isIndex(b) {
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
	}
	return strings.Compare(a, b)
}
