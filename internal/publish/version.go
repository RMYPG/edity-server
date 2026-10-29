package publish

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// releaseVersion is a version a Release can have, as the OpenAPI file
// writes it: X.Y.Z, with no leading zeros.
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// bumpPatch returns the version after pkg's, a package.json, with its patch
// one higher, and pkg with that version. It changes only the version's
// string, so that the rest of the file stays as the Developer wrote it.
func bumpPatch(pkg []byte) (string, []byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(pkg))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return "", nil, errors.New("not a JSON object")
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return "", nil, err
		}
		if name.String() != "version" {
			if err := dec.SkipValue(); err != nil {
				return "", nil, err
			}
			continue
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return "", nil, err
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		var current string
		if raw.Kind() == '"' {
			unquoted, err := jsontext.AppendUnquote(nil, raw)
			if err != nil {
				return "", nil, err
			}
			current = string(unquoted)
		}
		parts := releaseVersion.FindStringSubmatch(current)
		if parts == nil {
			return "", nil, fmt.Errorf("its version %s is not X.Y.Z", raw)
		}
		patch, err := strconv.ParseUint(parts[3], 10, 64)
		if err != nil || patch == ^uint64(0) {
			return "", nil, fmt.Errorf("its version %s has no next patch", raw)
		}
		next := fmt.Sprintf("%s.%s.%d", parts[1], parts[2], patch+1)
		bumped := append(append(bytes.Clone(pkg[:start]), `"`+next+`"`...), pkg[end:]...)
		// Whatever follows the version must still be read, so that a broken
		// file or a second version is not left in it.
		for dec.PeekKind() != '}' {
			if _, err := dec.ReadToken(); err != nil {
				return "", nil, err
			}
			if err := dec.SkipValue(); err != nil {
				return "", nil, err
			}
		}
		if err := endOfObject(dec); err != nil {
			return "", nil, err
		}
		return next, bumped, nil
	}
	return "", nil, errors.New("no version")
}

func endOfObject(dec *jsontext.Decoder) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}
