// Package content reads and writes a page's content, src/content/<page>.json
// in the Site repository, and the values a Publish's Changes carry.
//
// A content file is written back as Prettier formats it, with its keys in
// the file's own order and every value no Change touched as the file has it,
// so that the Site's CI, which runs prettier --check, passes on every
// Publish.
package content

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// A Value is a value in a page's content: text, a number, a List's Items or
// an object's keys. A content file broken by hand can also hold true, false
// and null, which no Field definitions accept. An object keeps the order of
// its keys, and text, a number or a literal its JSON text.
type Value struct {
	kind jsontext.Kind
	// text is the JSON of text, a number or a literal: the file's, with a
	// number as Prettier writes it.
	text string
	// keys are an object's keys, in order, and values the value of each
	// key, or a List's Items.
	keys   []string
	values []*Value
}

// Parse reads a content file. It accepts any JSON document, which the Field
// definitions then check, but refuses one with a key twice in an object,
// which a JSON reader takes one way or another without a word.
func Parse(data []byte) (*Value, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	v, err := read(dec, false)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// ParseChange reads a value a Change carries, from the Editor's JSON: text,
// a number, a List's Items or an Item's Fields, and nothing else. It writes
// text as JSON.stringify does and a number as JavaScript does, so that the
// Changes of any API client look alike in the content file.
func ParseChange(data []byte) (*Value, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	v, err := read(dec, true)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

func read(dec *jsontext.Decoder, change bool) (*Value, error) {
	switch kind := dec.PeekKind(); kind {
	case '{', '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		v := &Value{kind: kind}
		end := jsontext.Kind('}')
		if kind == '[' {
			end = ']'
		}
		for dec.PeekKind() != end {
			if kind == '{' {
				name, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				v.keys = append(v.keys, name.String())
			}
			value, err := read(dec, change)
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, value)
		}
		_, err := dec.ReadToken()
		return v, err
	case '"', '0':
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		v := &Value{kind: kind, text: string(raw)}
		switch {
		case kind == '0' && change:
			v.text = javaScriptNumber(v.text)
		case kind == '0':
			v.text = prettierNumber(v.text)
		case change:
			s, err := jsontext.AppendUnquote(nil, raw)
			if err != nil {
				return nil, err
			}
			v.text = quote(string(s))
		}
		return v, nil
	case 't', 'f', 'n':
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		if change {
			return nil, fmt.Errorf("%s is not text, a number, a List or an Item", raw)
		}
		return &Value{kind: kind, text: string(raw)}, nil
	default:
		_, err := dec.ReadToken()
		if err == nil {
			err = errors.New("not a JSON value")
		}
		return nil, err
	}
}

// At returns the value at path, a content path: keys and Item indices,
// dot-separated. It returns false when there is none, such as for an Item
// past a List's end.
func (v *Value) At(path string) (*Value, bool) {
	parent, i, ok := v.slot(path)
	if !ok {
		return nil, false
	}
	return parent.values[i], true
}

// Replace puts value at path in place of the value there, and returns false
// when there is none.
func (v *Value) Replace(path string, value *Value) bool {
	parent, i, ok := v.slot(path)
	if ok {
		parent.values[i] = value
	}
	return ok
}

// slot finds the object or List the value at path is in, and its place
// there.
func (v *Value) slot(path string) (parent *Value, index int, ok bool) {
	keys := strings.Split(path, ".")
	for n, key := range keys {
		index = -1
		switch v.kind {
		case '{':
			index = indexOf(v.keys, key)
		case '[':
			if i, err := strconv.Atoi(key); err == nil && isIndex(key) && i < len(v.values) {
				index = i
			}
		}
		if index < 0 {
			return nil, 0, false
		}
		if n == len(keys)-1 {
			return v, index, true
		}
		v = v.values[index]
	}
	return nil, 0, false
}

// isIndex tells whether key is an Item's index as a content path writes it:
// digits with no leading zero.
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

// Equal tells whether a and b are the same content value, as the Editor
// compares them: text by its characters, numbers by their value, Lists Item
// by Item, and objects key by key in any order.
func Equal(a, b *Value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case '"':
		return a.String() == b.String()
	case '0':
		x, errX := strconv.ParseFloat(a.text, 64)
		y, errY := strconv.ParseFloat(b.text, 64)
		if errX != nil || errY != nil {
			return a.text == b.text
		}
		return x == y
	case '[':
		if len(a.values) != len(b.values) {
			return false
		}
		for i := range a.values {
			if !Equal(a.values[i], b.values[i]) {
				return false
			}
		}
		return true
	case '{':
		if len(a.keys) != len(b.keys) {
			return false
		}
		for i, key := range a.keys {
			j := indexOf(b.keys, key)
			if j < 0 || !Equal(a.values[i], b.values[j]) {
				return false
			}
		}
		return true
	default:
		return a.text == b.text
	}
}

func indexOf(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

// String returns text's characters, or the JSON of any other value.
func (v *Value) String() string {
	if v.kind == '"' {
		s, err := jsontext.AppendUnquote(nil, v.text)
		if err == nil {
			return string(s)
		}
	}
	if v.kind == '{' || v.kind == '[' {
		return string(bytes.TrimSpace(v.Format()))
	}
	return v.text
}

// Decoded returns the value as encoding/json decodes it into an any with
// UseNumber, which is how fields.Definitions.Violations takes content.
func (v *Value) Decoded() any {
	switch v.kind {
	case '{':
		object := make(map[string]any, len(v.keys))
		for i, key := range v.keys {
			object[key] = v.values[i].Decoded()
		}
		return object
	case '[':
		items := make([]any, len(v.values))
		for i, item := range v.values {
			items[i] = item.Decoded()
		}
		return items
	case '"':
		return v.String()
	case '0':
		return json.Number(v.text)
	case 't':
		return true
	case 'f':
		return false
	default:
		return nil
	}
}

// quote writes s as JSON.stringify does: with \", \\, the short escapes of
// \b, \f, \n, \r and \t, and \u00XX for the other control characters.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// javaScriptNumber writes a JSON number as JavaScript's Number::toString
// writes the double it reads as, the shortest decimal that reads back as
// that double, which Prettier then writes as it is but for the + of an
// exponent. A number too large for a double stays as it is, for the Field
// definitions to refuse.
func javaScriptNumber(text string) string {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) {
		return prettierNumber(text)
	}
	if f == 0 {
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// d.ddde±x: the digits of the shortest decimal, and where its point is.
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	e, _ := strconv.Atoi(exponent)
	n, k := e+1, len(digits)
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		s = digits[:1]
		if k > 1 {
			s += "." + digits[1:]
		}
		s += "e" + strconv.Itoa(n-1)
	}
	return sign + s
}

// prettierNumber writes a JSON number as Prettier writes a numeric literal
// (printNumber in its src/utilities/print-number.js): with a lowercase e,
// no + and no leading zeros in its exponent, no exponent of zero, and no
// zeros after the first decimal that end its fraction.
func prettierNumber(text string) string {
	if len(text) == 1 {
		return text
	}
	text = strings.ToLower(text)
	mantissa, exponent, scientific := strings.Cut(text, "e")
	if whole, fraction, ok := strings.Cut(mantissa, "."); ok && len(fraction) > 1 {
		mantissa = whole + "." + fraction[:1] + strings.TrimRight(fraction[1:], "0")
	}
	if !scientific {
		return mantissa
	}
	negative := strings.HasPrefix(exponent, "-")
	digits := strings.TrimLeft(strings.TrimLeft(exponent, "+-"), "0")
	if digits == "" {
		return mantissa
	}
	if negative {
		digits = "-" + digits
	}
	return mantissa + "e" + digits
}
