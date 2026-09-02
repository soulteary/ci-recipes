// Package evidence validates the per-platform evidence objects emitted by
// Docker Buildx. It is the Go equivalent of validate-buildx-evidence.sh.
package evidence

import (
	"context"
	"encoding/json"
	"io"
	"sort"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const usage = "usage: validate-buildx-evidence SPDX|SLSA EXPECTED_PLATFORMS_JSON"

// Execute validates a Buildx evidence JSON object read from stdin. The first
// two arguments retain the shell script's positional contract; additional
// arguments are intentionally ignored for compatibility.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_ = stdout
	_ = stderr
	if ctx == nil {
		return cli.Exit(1, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return cli.Exit(1, "evidence validation canceled: %v", err)
	}

	field, expectedJSON := "", ""
	if len(args) > 0 {
		field = args[0]
	}
	if len(args) > 1 {
		expectedJSON = args[1]
	}

	var expected []string
	if (field != "SPDX" && field != "SLSA") || json.Unmarshal([]byte(expectedJSON), &expected) != nil || !validPlatforms(expected) {
		return cli.Exit(2, usage)
	}

	if stdin == nil {
		stdin = emptyReader{}
	}
	// Generic io.Reader values cannot be forcefully interrupted while blocked.
	// For CLI stdin (an *os.File) and pipes, closing an io.ReadCloser unblocks
	// Decode promptly when the context is canceled. Ownership remains with the
	// caller on every non-cancellation path.
	stopClose := make(chan struct{})
	if closer, ok := stdin.(io.ReadCloser); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-stopClose:
			}
		}()
		defer close(stopClose)
	}
	var document map[string]json.RawMessage
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: stdin})
	if err := decoder.Decode(&document); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cli.Exit(1, "evidence validation canceled: %v", ctxErr)
		}
		return invalidEvidence(field)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cli.Exit(1, "evidence validation canceled: %v", ctxErr)
		}
		return invalidEvidence(field)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cli.Exit(1, "evidence validation canceled: %v", ctxErr)
	}

	if !sameKeys(document, expected) {
		return invalidEvidence(field)
	}
	for _, platform := range expected {
		var perPlatform map[string]json.RawMessage
		if err := json.Unmarshal(document[platform], &perPlatform); err != nil {
			return invalidEvidence(field)
		}
		raw, ok := perPlatform[field]
		if !ok {
			return invalidEvidence(field)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil || len(payload) == 0 {
			return invalidEvidence(field)
		}
	}
	return nil
}

func validPlatforms(platforms []string) bool {
	if len(platforms) == 0 {
		return false
	}
	for _, platform := range platforms {
		if platform == "" {
			return false
		}
	}
	return true
}

func sameKeys(document map[string]json.RawMessage, expected []string) bool {
	if document == nil || len(document) != len(expected) {
		return false
	}
	actual := make([]string, 0, len(document))
	for key := range document {
		actual = append(actual, key)
	}
	expectedCopy := append([]string(nil), expected...)
	sort.Strings(actual)
	sort.Strings(expectedCopy)
	for i := range actual {
		if actual[i] != expectedCopy[i] {
			return false
		}
	}
	return true
}

func invalidEvidence(field string) error {
	return cli.Exit(1, "evidence must contain one non-empty %s object for every expected platform", field)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	n, err := r.reader.Read(p)
	if err == nil {
		select {
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		default:
		}
	}
	return n, err
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// Usage returns the compatibility usage line for callers that expose command
// help without executing the recipe.
func Usage() string { return usage }
