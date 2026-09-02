package checks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func executeGoVersionContract(ctx context.Context, root string, stdout, stderr io.Writer, runner commandRunner) error {
	snapshot, err := archiveRevision(ctx, runner, root, "HEAD", nil, stderr)
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshot)

	baseline, err := contractViolations(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("inspect Go-version self-test baseline: %w", err)
	}
	readme := filepath.Join(snapshot, "README.md")
	file, err := os.OpenFile(readme, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open README for Go-version self-test: %w", err)
	}
	_, writeErr := io.WriteString(file, "\nRequires Go 1.26+.\n")
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write Go-version self-test mutation: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Go-version self-test mutation: %w", closeErr)
	}

	structure, err := markdownViolations(ctx, snapshot)
	if err != nil {
		return err
	}
	if len(structure) != 0 {
		return fmt.Errorf("Go-version self-test introduced a Markdown structure error: %s", structure[0])
	}
	mutated, err := contractViolations(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("inspect Go-version self-test mutation: %w", err)
	}
	if len(mutated) <= len(baseline) || !containsDiagnostic(mutated, "stale Go 1.26 requirement") {
		return errors.New("Expected stale Go 1.26 documentation to fail")
	}
	if err := writef(stdout, "Go documentation version contract test passed.\n"); err != nil {
		return fmt.Errorf("write Go-version result: %w", err)
	}
	return nil
}

func containsDiagnostic(diagnostics []string, fragment string) bool {
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic, fragment) {
			return true
		}
	}
	return false
}

func executeReleaseWorkflow(ctx context.Context, root string, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workflowPath := filepath.Join(root, ".github", "workflows", "release.yml")
	workflow, err := readText(workflowPath)
	if err != nil {
		return err
	}
	publisherPath := filepath.Join(root, ".github", "scripts", "publish-github-release.sh")
	publisher := ""
	legacyPublisherPresent := true
	if value, readErr := readText(publisherPath); readErr == nil {
		publisher = value
	} else {
		legacyPublisherPresent = false
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	if err := validateReleaseWorkflow(workflow, publisher); err != nil {
		return err
	}
	if err := validateReleaseWorkflowWiring(workflow, legacyPublisherPresent); err != nil {
		return err
	}
	if err := writef(stdout, "Release workflow state-machine tests passed.\n"); err != nil {
		return fmt.Errorf("write release-workflow result: %w", err)
	}
	return nil
}

var legacyReleaseWorkflowScriptPattern = regexp.MustCompile(`(?:_release-automation/)?\.github/scripts/(?:check-doc-contracts|prepare-release-notes|publish-github-release|reconcile-release-aliases)\.sh`)

func validateReleaseWorkflowWiring(workflow string, legacyScriptsPresent bool) error {
	if !legacyScriptsPresent && legacyReleaseWorkflowScriptPattern.MatchString(workflow) {
		return errors.New("Release workflow still invokes a removed shell recipe")
	}
	return nil
}

func validateReleaseWorkflow(workflow, legacyPublisher string) error {
	require := func(value, message string) error {
		if !strings.Contains(workflow, value) {
			return errors.New(message)
		}
		return nil
	}
	if err := require("group: ${{ github.repository }}-release-${{ github.event_name == 'workflow_dispatch' && inputs.release_tag || github.ref_name }}", "Release concurrency is not scoped by tag"); err != nil {
		return err
	}
	if strings.Count(workflow, "queue: max") != 2 {
		return errors.New("Expected queue:max for tag publication and alias reconciliation")
	}
	if err := require("group: ${{ github.repository }}-release-aliases", "Mutable aliases do not have a dedicated concurrency group"); err != nil {
		return err
	}

	notes := strings.Index(workflow, "name: Prepare and validate curated release notes")
	immutability := strings.Index(workflow, "name: Resolve existing immutable image")
	publishBoundary := strings.Index(workflow, "name: Attest release artifacts")
	if notes < 0 || publishBoundary < 0 || notes >= publishBoundary {
		return errors.New("Release notes are not validated before external publication")
	}
	if immutability < 0 || immutability >= publishBoundary {
		return errors.New("Image immutability is not checked before external publication")
	}
	releaseDeletePattern := regexp.MustCompile(`\bgh\s+release\s+delete(?:\s|$)`)
	if releaseDeletePattern.MatchString(workflow) || releaseDeletePattern.MatchString(legacyPublisher) {
		return errors.New("Release overwrite still deletes the existing Release")
	}

	metadataStart := strings.Index(workflow, "name: Extract metadata")
	metadataEnd := strings.Index(workflow, "name: Build amd64 image")
	if metadataStart < 0 || metadataEnd < 0 || metadataEnd <= metadataStart {
		return errors.New("Release metadata block is missing or malformed")
	}
	metadata := workflow[metadataStart:metadataEnd]
	if !strings.Contains(metadata, "pattern={{version}}") {
		return errors.New("Immutable full-version image tag is missing")
	}
	if strings.Contains(metadata, "pattern={{major") || strings.Contains(metadata, "value=latest") {
		return errors.New("Mutable aliases are still published by the immutable image step")
	}
	return nil
}
