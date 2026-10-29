package content

import "strings"

// printWidth is Prettier's default, which the Sites keep for their content
// files (README.md, "Content files and Prettier").
const printWidth = 80

// Format writes a content file as Prettier formats JSON that has a line
// break after each object's opening brace, as every content file Prettier
// has checked does: each non-empty object on its own lines, a List on one
// line when it fits in the print width, and else an Item per line, or as
// many numbers per line as fit. Keys keep their order, and each value its
// text. The file ends with a line break.
func (v *Value) Format() []byte {
	var b strings.Builder
	write(&b, v, "", printWidth)
	b.WriteByte('\n')
	return []byte(b.String())
}

// write writes v, which starts a line's text at indent or follows one, in
// room: the columns left for it on its line, less one for the comma that
// follows it, if one does.
func write(b *strings.Builder, v *Value, indent string, room int) {
	switch {
	case v.kind == '{' && len(v.keys) > 0:
		inner := indent + "  "
		b.WriteString("{\n")
		for i, key := range v.keys {
			head := inner + quote(key) + ": "
			b.WriteString(head)
			write(b, v.values[i], inner, rest(head, i < len(v.keys)-1))
			if i < len(v.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case v.kind == '[' && len(v.values) > 0:
		if !mustBreak(v) {
			if line := flat(v); width(line) <= room {
				b.WriteString(line)
				return
			}
		}
		inner := indent + "  "
		b.WriteString("[\n" + inner)
		if allNumbers(v) {
			fill(b, v, inner)
		} else {
			for i, item := range v.values {
				if i > 0 {
					b.WriteString(",\n" + inner)
				}
				write(b, item, inner, rest(inner, i < len(v.values)-1))
			}
		}
		b.WriteString("\n" + indent + "]")
	default:
		b.WriteString(flat(v))
	}
}

// rest is the room left after head on its line.
func rest(head string, comma bool) int {
	room := printWidth - width(head)
	if comma {
		room--
	}
	return room
}

// flat is v on one line, which an object with keys never is.
func flat(v *Value) string {
	switch v.kind {
	case '{':
		return "{}"
	case '[':
		items := make([]string, len(v.values))
		for i, item := range v.values {
			items[i] = flat(item)
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return v.text
	}
}

// mustBreak tells whether a List goes on several lines however short it is:
// when it holds an object with keys, which has lines of its own, or Lists
// that must break, or when Prettier breaks it on sight, as a List of two or
// more Lists of two or more Items, or of objects of two or more keys.
func mustBreak(v *Value) bool {
	matrix := len(v.values) > 1
	for _, item := range v.values {
		switch {
		case item.kind == '{' && len(item.keys) > 0:
			return true
		case item.kind == '[' && mustBreak(item):
			return true
		}
		matrix = matrix && item.kind == v.values[0].kind && (item.kind == '[' || item.kind == '{') && len(item.values) > 1
	}
	return matrix
}

func allNumbers(v *Value) bool {
	for _, item := range v.values {
		if item.kind != '0' {
			return false
		}
	}
	return true
}

// fill writes a broken List of numbers as Prettier fills it: each number
// goes on the line of the one before it when the two, with their commas and
// the space between them, fit in what was left of the line before the one
// before it was written.
func fill(b *strings.Builder, v *Value, inner string) {
	item := func(i int) string {
		if i < len(v.values)-1 {
			return v.values[i].text + ","
		}
		return v.values[i].text
	}
	column := width(inner)
	for i := range v.values {
		text := item(i)
		b.WriteString(text)
		if i == len(v.values)-1 {
			break
		}
		if width(text)+1+width(item(i+1)) <= printWidth-column {
			b.WriteByte(' ')
			column += width(text) + 1
		} else {
			b.WriteString("\n" + inner)
			column = width(inner)
		}
	}
}
