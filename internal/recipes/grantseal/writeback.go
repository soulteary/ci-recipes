package grantseal

import (
	"context"
	"fmt"
	"io"
	"sort"
)

var grantsealWritebackPaths = map[string]struct{}{
	".github/go-test-report.json":    {},
	".github/go-test-report.md":      {},
	".github/coverage.svg":           {},
	".github/goreportcard.svg":       {},
	".github/goreportcard-report.md": {},
	"docs/enUS/quality.md":           {},
	"docs/zhCN/quality.md":           {},
}

func runWritebackAllowlist(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return usage("usage: writeback allowlist")
	}
	if _, err := deps.runner.LookPath("git"); err != nil {
		return usage("git not found")
	}
	data, err := runOutput(ctx, deps, "git", "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return usage("inspect staged paths: %v", err)
	}
	paths, err := splitNUL(data)
	if err != nil {
		return usage("parse staged paths: %v", err)
	}
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "note: nothing staged, write-back allowlist check is a no-op")
		return nil
	}
	var disallowed []string
	for _, name := range paths {
		if _, ok := grantsealWritebackPaths[name]; !ok {
			disallowed = append(disallowed, name)
		}
	}
	sort.Strings(disallowed)
	if len(disallowed) != 0 {
		for _, name := range disallowed {
			fmt.Fprintf(stderr, "disallowed staged path (outside report allowlist): %s\n", name)
		}
		return rejected("write-back staged files outside the report allowlist; refusing to commit")
	}
	fmt.Fprintln(stdout, "write-back allowlist check passed: all staged files are allowlisted report artifacts")
	return nil
}
