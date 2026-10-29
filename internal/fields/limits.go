package fields

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/RMYPG/edity/contracts"
	"github.com/clipperhouse/uax29/v2/graphemes"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/message"
)

// limitsVocabulary is the Limits vocabulary of the Edity dialect, as the
// contracts module's README defines it. Its meta-schema is compiled once, and
// every page's compiler shares it.
var limitsVocabulary = sync.OnceValues(func() (*jsonschema.Vocabulary, error) {
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	if err := addMetaSchemas(c); err != nil {
		return nil, err
	}
	meta, err := c.Compile(contracts.VocabularyMetaSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("compiling the Limits vocabulary's meta-schema: %w", err)
	}
	return &jsonschema.Vocabulary{
		URL:     contracts.VocabularyURL,
		Schema:  meta,
		Compile: compileLimits,
	}, nil
})

// limits holds the assertions of one schema object. x-edity-maxLines and
// x-edity-maxLinesNarrow are annotations, since only a browser can measure
// lines, so they have nothing to check.
//
// The values are doubles, as in the Site's build: the meta-schema has checked
// that they are whole numbers, but not that they fit an int.
type limits struct {
	maxGraphemes *float64
	decimals     *float64
}

func compileLimits(_ *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
	var l limits
	for keyword, field := range map[string]**float64{
		"x-edity-maxGraphemes": &l.maxGraphemes,
		"x-edity-decimals":     &l.decimals,
	} {
		v, ok := obj[keyword]
		if !ok {
			continue
		}
		n, ok := double(v)
		if !ok {
			return nil, fmt.Errorf("%s: %v is not a number", keyword, v)
		}
		*field = &n
	}
	if l.maxGraphemes == nil && l.decimals == nil {
		return nil, nil
	}
	return &l, nil
}

func (l *limits) Validate(ctx *jsonschema.ValidatorContext, v any) {
	switch v := v.(type) {
	case string:
		if l.maxGraphemes != nil {
			if n := graphemeCount(v); float64(n) > *l.maxGraphemes {
				ctx.AddError(&tooManyGraphemes{max: *l.maxGraphemes, got: n})
			}
		}
	case json.Number, float64:
		if l.decimals != nil {
			f, ok := double(v)
			if !ok {
				ctx.AddError(&notADouble{number: v})
				return
			}
			if n := decimalPlaces(f); float64(n) > *l.decimals {
				ctx.AddError(&tooManyDecimals{max: *l.decimals, got: n})
			}
		}
	}
}

// double reads a JSON number as the Site's build does, as an IEEE 754
// double. It reports whether the number is one: 1e400 is too large to be.
func double(v any) (float64, bool) {
	var f float64
	switch v := v.(type) {
	case json.Number:
		var err error
		if f, err = strconv.ParseFloat(string(v), 64); err != nil {
			return 0, false
		}
	case float64:
		f = v
	default:
		return 0, false
	}
	return f, !math.IsInf(f, 0) && !math.IsNaN(f)
}

// graphemeCount counts the visible characters of s as the Editor does with
// Intl.Segmenter: extended grapheme clusters (Unicode UAX #29), by Unicode 17.
func graphemeCount(s string) int {
	n := 0
	for it := graphemes.FromString(s); it.Next(); {
		n++
	}
	return n
}

// decimalPlaces counts the fraction digits of the shortest decimal form of
// f, without an exponent: 19.99 has 2, 0.1 + 0.2 has 17 and 1e-7 has 7.
func decimalPlaces(f float64) int {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// The validation errors of the Limits vocabulary.
type (
	tooManyGraphemes struct {
		max float64
		got int
	}
	tooManyDecimals struct {
		max float64
		got int
	}
	// notADouble is a number the Site's build cannot read, which it takes
	// for something other than a number.
	notADouble struct{ number any }
)

func (*tooManyGraphemes) KeywordPath() []string { return []string{"x-edity-maxGraphemes"} }

func (k *tooManyGraphemes) LocalizedString(p *message.Printer) string {
	return p.Sprintf("%d visible characters, want at most %v", k.got, k.max)
}

func (*tooManyDecimals) KeywordPath() []string { return []string{"x-edity-decimals"} }

func (k *tooManyDecimals) LocalizedString(p *message.Printer) string {
	return p.Sprintf("%d decimals, want at most %v", k.got, k.max)
}

func (*notADouble) KeywordPath() []string { return []string{"x-edity-decimals"} }

func (k *notADouble) LocalizedString(p *message.Printer) string {
	return p.Sprintf("%v does not fit a double", k.number)
}
