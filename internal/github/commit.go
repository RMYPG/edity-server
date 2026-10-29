package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrStale is a commit GitHub did not make because the branch no longer
// points to the commit it was to go on.
var ErrStale = errors.New("the branch no longer points to the commit to go on")

// ErrUncertain is a commit GitHub may or may not have made: the call failed
// on the way, or GitHub failed while making it.
var ErrUncertain = errors.New("GitHub did not tell whether it made the commit")

// A NewCommit is a commit to make on a branch.
type NewCommit struct {
	Branch string
	// Parent is the commit the branch must still point to, which the new
	// commit goes on top of.
	Parent string
	// Message is the commit message: its first line, a blank line, and the
	// rest.
	Message string
	// Files are the files the commit adds or changes.
	Files []File
}

// A File is a file's path in the repository, and its new content.
type File struct {
	Path     string
	Contents []byte
}

const createCommitOnBranch = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}`

// CreateCommit makes c, and returns its sha. GitHub authors it as the App's
// bot and signs it, and moves the branch to it only if the branch still
// points to c.Parent: it returns ErrStale if not. ErrUncertain means it
// cannot tell whether GitHub made it.
//
// It calls GraphQL's createCommitOnBranch, whose expectedHeadOid makes the
// check and the move one step.
func (r *Repo) CreateCommit(ctx context.Context, c NewCommit) (string, error) {
	headline, body, _ := strings.Cut(c.Message, "\n\n")
	additions := make([]map[string]string, len(c.Files))
	for i, f := range c.Files {
		additions[i] = map[string]string{"path": f.Path, "contents": base64.StdEncoding.EncodeToString(f.Contents)}
	}
	input := map[string]any{
		"branch":          map[string]string{"repositoryNameWithOwner": r.String(), "branchName": c.Branch},
		"expectedHeadOid": c.Parent,
		"message":         map[string]string{"headline": headline, "body": body},
		"fileChanges":     map[string]any{"additions": additions},
	}
	req := request{
		method: http.MethodPost,
		path:   "/graphql",
		body:   map[string]any{"query": createCommitOnBranch, "variables": map[string]any{"input": input}},
	}
	for attempt := 0; ; attempt++ {
		token, err := r.app.token(ctx, r)
		if err != nil {
			return "", err
		}
		req.auth = token
		a, err := r.app.send(ctx, req)
		switch {
		case err != nil:
			return "", fmt.Errorf("%w: %w", ErrUncertain, err)
		case a.status == http.StatusUnauthorized && attempt == 0:
			r.app.forget(r, token)
			continue
		case a.status >= 500:
			return "", fmt.Errorf("%w: %w", ErrUncertain, a.failure())
		case a.status != http.StatusOK:
			return "", a.failure()
		}
		return created(a.body)
	}
}

// created reads createCommitOnBranch's answer.
func created(data []byte) (string, error) {
	var answer struct {
		Data struct {
			CreateCommitOnBranch *struct {
				Commit struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return "", fmt.Errorf("%w: its answer is not JSON: %w", ErrUncertain, err)
	}
	if len(answer.Errors) > 0 {
		var messages []string
		for _, e := range answer.Errors {
			if e.Type == "STALE_DATA" {
				return "", fmt.Errorf("%w: %s", ErrStale, e.Message)
			}
			messages = append(messages, e.Message)
		}
		return "", &Error{Call: "createCommitOnBranch", Status: http.StatusOK, Message: strings.Join(messages, "; ")}
	}
	if answer.Data.CreateCommitOnBranch == nil || answer.Data.CreateCommitOnBranch.Commit.OID == "" {
		return "", fmt.Errorf("%w: it answered no commit", ErrUncertain)
	}
	return answer.Data.CreateCommitOnBranch.Commit.OID, nil
}
