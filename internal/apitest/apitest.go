// Package apitest checks the server's requests and responses against its
// OpenAPI description, api/openapi.yaml, so that each handler's tests also
// test the contract the Editor's API client is generated from. Only tests
// import it.
package apitest

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	validationerrors "github.com/pb33f/libopenapi-validator/errors"
	"github.com/pb33f/libopenapi-validator/helpers"

	"github.com/RMYPG/edity-server/api"
)

// Operation is one route the OpenAPI file describes.
type Operation struct {
	// ID is its operationId, such as "publish".
	ID string
	// Method is its HTTP method, such as "POST".
	Method string
	// Path is its path template, such as "/_edity/api/publish/{sha}".
	Path string
	// Example is Path with each parameter replaced by its example in the
	// file, such as "/_edity/api/publish/4f9d…".
	Example string
}

// Operations returns every route the OpenAPI file describes, in the file's
// order.
func Operations(t testing.TB) []Operation {
	t.Helper()
	return slices.Clone(load(t).operations)
}

// Handler wraps h for a test, so that every request it serves, and the
// response h gives it, are checked against the OpenAPI file. Each mismatch
// fails t:
//
//   - A route the file does not describe gets 404 or 405: the server serves
//     nothing else.
//   - Any other response is one the file describes for its route: one of its
//     statuses, with that status's required headers, and a body of one of its
//     media types that matches its schema.
//   - A request the file does not allow, such as a body of another shape, a
//     path parameter of another form or a missing session cookie, gets a 4xx:
//     the server accepts nothing the file leaves out.
//
// The response reaches the test as h gave it.
func Handler(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	c := load(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, problem := range c.serve(w, r, h) {
			t.Errorf("%s %s: %s", r.Method, r.URL.Path, problem)
		}
	})
}

// contract is the OpenAPI file, parsed.
type contract struct {
	validator  validator.Validator
	operations []Operation
}

var parsed = sync.OnceValues(parse)

func load(t testing.TB) *contract {
	t.Helper()
	c, err := parsed()
	if err != nil {
		t.Fatalf("api/openapi.yaml: %v", err)
	}
	return c
}

func parse() (*contract, error) {
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		return nil, err
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		return nil, err
	}
	v, errs := validator.NewValidator(doc)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if ok, errs := v.ValidateDocument(); !ok {
		return nil, fmt.Errorf("not a valid OpenAPI document: %s", describe(errs))
	}
	c := &contract{validator: v}

	for path, item := range model.Model.Paths.PathItems.FromOldest() {
		for method, op := range item.GetOperations().FromOldest() {
			example := path
			for _, p := range append(item.Parameters, op.Parameters...) {
				if p.In != "path" {
					continue
				}
				if p.Example == nil {
					return nil, fmt.Errorf("%s %s: the path parameter %q has no example", method, path, p.Name)
				}
				example = strings.ReplaceAll(example, "{"+p.Name+"}", p.Example.Value)
			}
			c.operations = append(c.operations, Operation{
				ID:      op.OperationId,
				Method:  strings.ToUpper(method),
				Path:    path,
				Example: example,
			})
		}
	}
	return c, nil
}

// serve passes r to h, writes h's response to w, and returns how the two
// break the contract.
func (c *contract) serve(w http.ResponseWriter, r *http.Request, h http.Handler) []string {
	// It leaves the body for h to read.
	_, invalid := c.validator.ValidateHttpRequest(r)
	documented := !slices.ContainsFunc(invalid, isUndescribedRoute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	body := rec.Body.Bytes()
	maps.Copy(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(body)

	if !documented {
		if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusMethodNotAllowed {
			return []string{fmt.Sprintf("answered %d to a route the OpenAPI file does not describe", res.StatusCode)}
		}
		return nil
	}
	var problems []string
	if ok, errs := c.validator.ValidateHttpResponse(r, res); !ok {
		problems = append(problems, fmt.Sprintf("the %d response is not one the OpenAPI file describes: %s", res.StatusCode, describe(errs)))
	}
	if len(invalid) > 0 && res.StatusCode < 400 {
		problems = append(problems, fmt.Sprintf("answered %d to a request the OpenAPI file does not allow: %s", res.StatusCode, describe(invalid)))
	}
	return problems
}

// isUndescribedRoute tells whether e says that the OpenAPI file has no such
// path, or no such method for it.
func isUndescribedRoute(e *validationerrors.ValidationError) bool {
	return e.ValidationType == helpers.PathValidation &&
		(e.ValidationSubType == helpers.ValidationMissing || e.ValidationSubType == helpers.ValidationMissingOperation)
}

// describe puts the validator's errors in one line each.
func describe(errs []*validationerrors.ValidationError) string {
	var lines []string
	for _, e := range errs {
		line := e.Message
		for _, s := range e.SchemaValidationErrors {
			at := s.FieldPath
			if at == "" {
				at = "/" + strings.Join(s.InstancePath, "/")
			}
			line += fmt.Sprintf("; at %s: %s", at, s.Reason)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n\t")
}
