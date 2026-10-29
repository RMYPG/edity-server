package publish

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"

	"github.com/RMYPG/edity-server/internal/release"
)

// bumpPatch returns the version after pkg's, a package.json, with its patch
// one higher, and pkg with that version. It changes only the version's
// string, so that the rest of the file stays as the Developer wrote it.
func bumpPatch(pkg []byte) (release.Version, []byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(pkg))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return release.Version{}, nil, errors.New("not a JSON object")
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return release.Version{}, nil, err
		}
		if name.String() != "version" {
			if err := dec.SkipValue(); err != nil {
				return release.Version{}, nil, err
			}
			continue
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return release.Version{}, nil, err
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		var current string
		if raw.Kind() == '"' {
			unquoted, err := jsontext.AppendUnquote(nil, raw)
			if err != nil {
				return release.Version{}, nil, err
			}
			current = string(unquoted)
		}
		v, ok := release.ParseVersion(current)
		if !ok {
			return release.Version{}, nil, fmt.Errorf("its version %s is not X.Y.Z", raw)
		}
		next, ok := v.NextPatch()
		if !ok {
			return release.Version{}, nil, fmt.Errorf("its version %s has no next patch", raw)
		}
		bumped := append(append(bytes.Clone(pkg[:start]), `"`+next.String()+`"`...), pkg[end:]...)
		// Whatever follows the version must still be read, so that a broken
		// file or a second version is not left in it.
		for dec.PeekKind() != '}' {
			if _, err := dec.ReadToken(); err != nil {
				return release.Version{}, nil, err
			}
			if err := dec.SkipValue(); err != nil {
				return release.Version{}, nil, err
			}
		}
		if err := endOfObject(dec); err != nil {
			return release.Version{}, nil, err
		}
		return next, bumped, nil
	}
	return release.Version{}, nil, errors.New("no version")
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
