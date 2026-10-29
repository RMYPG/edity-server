// Package githubtest holds what the tests of the packages that call GitHub
// share: GitHub's answers, as each package's testdata/record.sh records them
// from RMYPG/edity-sandbox, and edited copies of them.
package githubtest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A Response is one of GitHub's answers.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Recorded reads an answer the test's package's testdata/record.sh
// recorded, testdata/<name>: a status line, headers and the body.
func Recorded(t testing.TB, name string) Response {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(data)))
	line, err := r.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	_, code, _ := strings.Cut(line, " ")
	status, err := strconv.Atoi(code[:3])
	if err != nil {
		t.Fatalf("%s: status line %q", name, line)
	}
	header, err := r.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	body, err := io.ReadAll(r.R)
	if err != nil {
		t.Fatal(err)
	}
	return Response{Status: status, Header: http.Header(header), Body: body}
}

// Edited is r with its JSON body changed by edit: the answers GitHub gave
// at other moments, or would give had something gone wrong.
func Edited(t testing.TB, r Response, edit func(body map[string]any)) Response {
	t.Helper()
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	edit(body)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return Response{Status: r.Status, Header: r.Header, Body: b}
}

// Key is a private key for a test's App, generated once.
var Key = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})
