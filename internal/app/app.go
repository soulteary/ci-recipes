// Package app wires recipe packages into the ci-recipes command-line interface.
package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/soulteary/ci-recipes/internal/cli"
	errortracerrelease "github.com/soulteary/ci-recipes/internal/recipes/errortracer/release"
	"github.com/soulteary/ci-recipes/internal/recipes/grantseal"
	stargatechecks "github.com/soulteary/ci-recipes/internal/recipes/stargate/checks"
	stargaterelease "github.com/soulteary/ci-recipes/internal/recipes/stargate/release"
	wordpresscosign "github.com/soulteary/ci-recipes/internal/recipes/wordpress/cosign"
	wordpressevidence "github.com/soulteary/ci-recipes/internal/recipes/wordpress/evidence"
	wordpresspublished "github.com/soulteary/ci-recipes/internal/recipes/wordpress/published"
	wordpressrelease "github.com/soulteary/ci-recipes/internal/recipes/wordpress/release"
)

// Build metadata is set by release builds through -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

type executor func(context.Context, []string, io.Reader, io.Writer, io.Writer) error

type command struct {
	source      string
	name        string
	usage       string
	description string
	minimumArgs int
	execute     executor
}

var commands = []command{
	{"stargate", "check-doc-contracts", "[BASE_SHA]", "validate Markdown, runtime API, configuration and deployment documentation contracts", 0, prefix(stargatechecks.Execute, "doc-contracts")},
	{"stargate", "check-go-version-contract", "", "run the Go-version documentation regression contract", 0, prefix(stargatechecks.Execute, "go-version-contract")},
	{"stargate", "check-release-workflow", "", "validate the release workflow state-machine contract", 0, prefix(stargatechecks.Execute, "release-workflow")},
	{"stargate", "local-run", "[-port PORT]", "run the native local Stargate launcher used by its CI port tests", 0, prefix(stargatechecks.Execute, "local-run")},
	{"stargate", "extract-release-notes", "TAG OUTPUT [CHANGELOG]", "extract validated release notes from CHANGELOG", 2, prefix(stargaterelease.Execute, "extract-notes")},
	{"stargate", "prepare-release-notes", "TAG OUTPUT [CHANGELOG]", "prepare notes with an explicitly enabled historical-release fallback", 2, prefix(stargaterelease.Execute, "prepare-notes")},
	{"stargate", "plan-release-aliases", "TAG RELEASE_TAGS_FILE", "plan monotonic stable OCI aliases", 2, prefix(stargaterelease.Execute, "plan-aliases")},
	{"stargate", "publish-github-release", "TAG NOTES_FILE DIST_DIR", "create or reconcile a GitHub Release and its assets", 3, prefix(stargaterelease.Execute, "publish-github")},
	{"stargate", "reconcile-release-aliases", "TAG IMAGE", "move stable OCI aliases and GitHub Latest to the SemVer high-water mark", 2, prefix(stargaterelease.Execute, "reconcile-aliases")},
	{"docker-sqlite-wordpress", "validate-buildx-evidence", "SPDX|SLSA PLATFORMS_JSON", "validate per-platform Buildx evidence read from stdin", 2, wordpressevidence.Execute},
	{"docker-sqlite-wordpress", "validate-release", "RELEASE_VERSION [--verify-upstream]", "validate release metadata and optionally the pinned upstream digest", 1, wordpressrelease.Execute},
	{"docker-sqlite-wordpress", "verify-cosign-signature", "IMAGE IDENTITY ISSUER", "retry only delayed keyless-signature publication failures", 3, wordpresscosign.Execute},
	{"docker-sqlite-wordpress", "verify-published-release", "RELEASE_VERSION", "compare Docker Hub and GHCR manifest digests", 1, wordpresspublished.Execute},
	{"error-tracer", "build-release", "VERSION [OUTPUT_DIR]", "build deterministic six-target release archives", 1, errortracerrelease.Execute},
	{"grantseal", "check-archive-allowlist", "[DIR]|--image REF|--manifest FILE [BASE]", "enforce release archive, image or manifest path policy", 0, prefix2(grantseal.Execute, "archive", "allowlist")},
	{"grantseal", "check-sensitive-files", "[PATH ...]", "detect private-key filenames and material", 0, prefix2(grantseal.Execute, "sensitive", "files")},
	{"grantseal", "check-patch-coverage", "PROFILE [BASE_REF] [THRESHOLD]", "enforce changed-line coverage without fail-open parsing", 1, prefix2(grantseal.Execute, "patch", "coverage")},
	{"grantseal", "check-doc-language", "[ROOT] [--allowlist FILE]", "enforce documentation language policy", 0, prefix2(grantseal.Execute, "doc", "language")},
	{"grantseal", "check-doc-consistency", "[ROOT]", "enforce documentation and workflow consistency rules", 0, prefix2(grantseal.Execute, "doc", "consistency")},
	{"grantseal", "check-writeback-allowlist", "", "restrict staged CI report writeback paths", 0, prefix2(grantseal.Execute, "writeback", "allowlist")},
	{"grantseal", "inject-report-environment", "[REPORT_JSON]", "atomically replace report environment metadata", 0, prefix2(grantseal.Execute, "report", "inject-environment")},
	{"grantseal", "generate-quality-docs", "[REPO_ROOT]", "atomically render localized quality documentation", 0, prefix2(grantseal.Execute, "quality", "docs")},
}

func prefix(run executor, first string) executor {
	return func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		return run(ctx, append([]string{first}, args...), stdin, stdout, stderr)
	}
}

func prefix2(run executor, first, second string) executor {
	return func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		return run(ctx, append([]string{first, second}, args...), stdin, stdout, stderr)
	}
}

// Execute runs the selected recipe without terminating the process.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return executeWithCommands(ctx, args, stdin, stdout, stderr, commands)
}

func executeWithCommands(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, available []command) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return writeHelp(stdout)
	}
	if args[0] == "version" || args[0] == "--version" {
		_, err := fmt.Fprintf(stdout, "ci-recipes %s (commit %s, built %s)\n", Version, Commit, BuiltAt)
		return err
	}
	if len(args) < 2 {
		return cli.Exit(2, "expected SOURCE and RECIPE; run ci-recipes help")
	}
	for _, candidate := range available {
		if args[0] == candidate.source && args[1] == candidate.name {
			recipeArgs := args[2:]
			if len(recipeArgs) < candidate.minimumArgs {
				return cli.Exit(2, "usage: %s", candidate.invocation())
			}
			return candidate.execute(ctx, recipeArgs, stdin, stdout, stderr)
		}
	}
	return cli.Exit(2, "unknown recipe %q; run ci-recipes help", strings.Join(args[:2], " "))
}

func (c command) invocation() string {
	invocation := "ci-recipes " + c.source + " " + c.name
	if c.usage != "" {
		invocation += " " + c.usage
	}
	return invocation
}

func writeHelp(output io.Writer) error {
	if _, err := fmt.Fprint(output, "Usage: ci-recipes SOURCE RECIPE [ARG ...]\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Active CI recipes:"); err != nil {
		return err
	}
	current := ""
	for _, candidate := range commands {
		if candidate.source != current {
			current = candidate.source
			if _, err := fmt.Fprintf(output, "\n%s:\n", current); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(output, "  %-66s %s\n", candidate.invocation(), candidate.description); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(output, "\nOther commands: ci-recipes version, ci-recipes help\n")
	return err
}
