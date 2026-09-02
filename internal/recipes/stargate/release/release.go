// Package release implements the release automation recipes formerly kept as
// shell scripts in soulteary/stargate.
package release

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const defaultRepository = "soulteary/stargate"

var (
	releaseTagPattern       = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
	datedHeadingPattern     = regexp.MustCompile(`^## \[([^]]+)\] - ([0-9]{4}-[0-9]{2}-[0-9]{2})$`)
	digestPattern           = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	versionReferencePattern = regexp.MustCompile(`^\[[0-9]+\.[0-9]+\.[0-9]+\]:`)
	repositoryPartPattern   = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)
	imagePartPattern        = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
)

// Command describes an external command without involving a shell.
type Command struct {
	Name   string
	Args   []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner executes external commands. Tests replace it with a deterministic fake.
type Runner interface {
	Run(context.Context, Command) error
}

type osRunner struct{}

func (osRunner) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Dir, c.Stdin, c.Stdout, c.Stderr
	return cmd.Run()
}

// Options supplies process and environment boundaries for recipe execution.
type Options struct {
	Getenv func(string) string
	Runner Runner
}

func defaultOptions() Options {
	return Options{Getenv: os.Getenv, Runner: osRunner{}}
}

func (o Options) normalized() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Runner == nil {
		o.Runner = osRunner{}
	}
	return o
}

// Execute dispatches a stargate release recipe.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return ExecuteWithOptions(ctx, args, stdin, stdout, stderr, defaultOptions())
}

// ExecuteWithOptions is Execute with injectable boundaries for tests.
func ExecuteWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) error {
	opts = opts.normalized()
	if len(args) == 0 {
		return cli.Exit(2, "usage: ci-recipes stargate <extract-release-notes|prepare-release-notes|plan-release-aliases|publish-github-release|reconcile-release-aliases> ...")
	}
	switch args[0] {
	case "extract-release-notes", "extract-notes":
		return executeExtract(args[1:], stdout, opts)
	case "prepare-release-notes", "prepare-notes":
		return executePrepare(ctx, args[1:], stdout, stderr, opts)
	case "plan-release-aliases", "plan-aliases":
		return executePlan(args[1:], stdout)
	case "publish-github-release", "publish-github":
		return executePublish(ctx, args[1:], stdout, stderr, opts)
	case "reconcile-release-aliases", "reconcile-aliases":
		return executeReconcile(ctx, args[1:], stdout, stderr, opts)
	default:
		return cli.Exit(2, "unknown stargate release recipe %q", args[0])
	}
}

type version struct {
	major, minor, patch string
	prerelease          string
}

func parseTag(tag string) (version, bool) {
	match := releaseTagPattern.FindStringSubmatch(tag)
	if match == nil {
		return version{}, false
	}
	prerelease := match[4]
	for _, identifier := range strings.Split(prerelease, ".") {
		if len(identifier) > 1 && identifier[0] == '0' && allDigits(identifier) {
			return version{}, false
		}
	}
	return version{major: match[1], minor: match[2], patch: match[3], prerelease: prerelease}, true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (v version) core() string { return v.major + "." + v.minor + "." + v.patch }

func compareNumber(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func compareVersion(a, b version) int {
	if c := compareNumber(a.major, b.major); c != 0 {
		return c
	}
	if c := compareNumber(a.minor, b.minor); c != 0 {
		return c
	}
	return compareNumber(a.patch, b.patch)
}

func requireTag(tag string) (version, error) {
	parsed, ok := parseTag(tag)
	if !ok {
		return version{}, fmt.Errorf("unsupported release tag %q: want vMAJOR.MINOR.PATCH with an optional SemVer prerelease and no build metadata", tag)
	}
	return parsed, nil
}

func validateRepository(repository string) error {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid GITHUB_REPOSITORY %q: want OWNER/REPOSITORY", repository)
	}
	for _, part := range parts {
		if len(part) > 100 || !repositoryPartPattern.MatchString(part) || strings.Contains(part, "..") {
			return fmt.Errorf("invalid GITHUB_REPOSITORY %q: want OWNER/REPOSITORY", repository)
		}
	}
	return nil
}

func validateImage(image string) error {
	if image == "" || len(image) > 255 || strings.ContainsAny(image, "@+\\ \t\r\n") || strings.Contains(image, "://") {
		return fmt.Errorf("invalid image repository %q: want a lowercase, untagged OCI repository", image)
	}
	parts := strings.Split(image, "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid image repository %q: want a lowercase, untagged OCI repository", image)
	}
	first := parts[0]
	if colon := strings.LastIndexByte(first, ':'); colon >= 0 {
		if strings.Contains(first[:colon], ":") {
			return fmt.Errorf("invalid image repository %q: IPv6 registry names are not supported", image)
		}
		port, err := strconv.Atoi(first[colon+1:])
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid image repository %q: registry port must be 1-65535", image)
		}
		first = first[:colon]
	}
	if !imagePartPattern.MatchString(first) {
		return fmt.Errorf("invalid image repository %q: want a lowercase, untagged OCI repository", image)
	}
	for _, part := range parts[1:] {
		if !imagePartPattern.MatchString(part) {
			return fmt.Errorf("invalid image repository %q: want a lowercase, untagged OCI repository", image)
		}
	}
	return nil
}

func usage(message string) error {
	return cli.Exit(2, "usage: ci-recipes stargate "+message)
}

func executeExtract(args []string, stdout io.Writer, opts Options) error {
	if len(args) < 2 || len(args) > 3 {
		return usage("extract-release-notes TAG OUTPUT [CHANGELOG]")
	}
	changelog := "CHANGELOG.md"
	if len(args) >= 3 && args[2] != "" {
		changelog = args[2]
	}
	repository := opts.Getenv("GITHUB_REPOSITORY")
	if repository == "" {
		repository = defaultRepository
	}
	if err := validateRepository(repository); err != nil {
		return err
	}
	notes, err := extractNotes(args[0], changelog, repository)
	if err != nil {
		return err
	}
	return writeAtomic(args[1], notes)
}

func extractNotes(tag, changelog, repository string) ([]byte, error) {
	v, err := requireTag(tag)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(changelog)
	if err != nil {
		return nil, fmt.Errorf("read changelog: %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	prefix := "## [" + v.core() + "] - "
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("CHANGELOG.md has no section for %s", v.core())
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## [") || lines[i] == "### Release verification" || versionReferencePattern.MatchString(lines[i]) {
			end = i
			break
		}
	}
	section := append([]string(nil), lines[start:end]...)
	validHeading := false
	if len(section) > 0 {
		match := datedHeadingPattern.FindStringSubmatch(section[0])
		if match != nil && match[1] == v.core() {
			date, dateErr := time.Parse("2006-01-02", match[2])
			validHeading = dateErr == nil && date.Year() >= 1
		}
	}
	if !validHeading {
		return nil, fmt.Errorf("Release %s requires an exact dated CHANGELOG heading for %s", tag, v.core())
	}
	nonempty := false
	for _, line := range section[1:] {
		if strings.TrimSpace(line) != "" {
			nonempty = true
			break
		}
	}
	if !nonempty {
		return nil, fmt.Errorf("CHANGELOG.md section for %s is empty", v.core())
	}
	if v.prerelease != "" {
		section[0] = "## [" + strings.TrimPrefix(tag, "v") + "] - Prerelease"
	}
	body := strings.Join(section, "\n")
	body = strings.ReplaceAll(body, "](docs/enUS/MIGRATION_V1.md)", "](https://github.com/"+repository+"/blob/"+tag+"/docs/enUS/MIGRATION_V1.md)")
	body = strings.TrimSuffix(body, "\n") + "\n\nUpgrade instructions: [v1.0.0 migration guide](https://github.com/" + repository + "/blob/" + tag + "/docs/enUS/MIGRATION_V1.md).\n"
	return []byte(body), nil
}

func executePrepare(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) error {
	if len(args) < 2 || len(args) > 3 {
		return usage("prepare-release-notes TAG OUTPUT [CHANGELOG]")
	}
	if _, err := requireTag(args[0]); err != nil {
		return err
	}
	changelog := "CHANGELOG.md"
	if len(args) >= 3 && args[2] != "" {
		changelog = args[2]
	}
	repository := opts.Getenv("GITHUB_REPOSITORY")
	if repository == "" {
		repository = defaultRepository
	}
	if err := validateRepository(repository); err != nil {
		return err
	}
	notes, extractErr := extractNotes(args[0], changelog, repository)
	if extractErr == nil {
		return writeAtomic(args[1], notes)
	}
	if opts.Getenv("ALLOW_EXISTING_RELEASE_NOTES") != "true" {
		return fmt.Errorf("%v\nRelease-note validation failed for %s", extractErr, args[0])
	}
	repository = opts.Getenv("GITHUB_REPOSITORY")
	if repository == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is required for release-note fallback")
	}
	if err := validateRepository(repository); err != nil {
		return err
	}
	var body bytes.Buffer
	var diagnostics bytes.Buffer
	if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: []string{"release", "view", "--repo", repository, "--json", "body", "--jq", ".body", "--", args[0]}, Stdout: &body, Stderr: &diagnostics}); err != nil {
		if diagnostics.Len() > 0 {
			if _, writeErr := io.Copy(stderr, &diagnostics); writeErr != nil {
				return fmt.Errorf("report GitHub Release lookup failure: %w", writeErr)
			}
		}
		return fmt.Errorf("no existing GitHub Release notes are available for %s: %w", args[0], err)
	}
	trimmed := strings.TrimSpace(body.String())
	if trimmed == "" || trimmed == "null" {
		return fmt.Errorf("Existing GitHub Release notes are empty for %s", args[0])
	}
	if err := writeAtomic(args[1], body.Bytes()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Reusing existing GitHub Release notes for historical tag %s.\n", args[0]); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

type alias struct{ name, tag string }

func executePlan(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return usage("plan-release-aliases TAG RELEASE_TAGS_FILE")
	}
	plan, err := planAliases(args[0], args[1])
	if err != nil {
		return err
	}
	for _, item := range plan {
		if _, err := fmt.Fprintf(stdout, "%s\t%s\n", item.name, item.tag); err != nil {
			return fmt.Errorf("write alias plan: %w", err)
		}
	}
	return nil
}

func planAliases(tag, releaseTagsPath string) ([]alias, error) {
	current, err := requireTag(tag)
	if err != nil {
		return nil, err
	}
	if current.prerelease != "" {
		return nil, nil
	}
	file, err := os.Open(releaseTagsPath)
	if err != nil {
		return nil, fmt.Errorf("read release tags: %w", err)
	}
	versions := map[string]version{tag: current}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		candidate := strings.TrimSuffix(scanner.Text(), "\r")
		parsed, valid := parseTag(candidate)
		if valid && parsed.prerelease == "" {
			versions[candidate] = parsed
		}
	}
	if err := scanner.Err(); err != nil {
		file.Close()
		return nil, fmt.Errorf("read release tags: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close release tags: %w", err)
	}
	tags := make([]string, 0, len(versions))
	for candidate := range versions {
		tags = append(tags, candidate)
	}
	sort.Slice(tags, func(i, j int) bool { return compareVersion(versions[tags[i]], versions[tags[j]]) < 0 })
	latest := tags[len(tags)-1]
	majorLatest, minorLatest := "", ""
	for _, candidate := range tags {
		parsed := versions[candidate]
		if parsed.major == current.major {
			majorLatest = candidate
			if parsed.minor == current.minor {
				minorLatest = candidate
			}
		}
	}
	return []alias{{"latest", latest}, {current.major, majorLatest}, {current.major + "." + current.minor, minorLatest}}, nil
}

func regularFile(path, description string, requireContent bool) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", description, path, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("%s is missing: %s: %w", description, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a non-symlink regular file: %s", description, path)
	}
	if requireContent && info.Size() == 0 {
		return "", fmt.Errorf("%s is empty: %s", description, path)
	}
	return absolute, nil
}

func releaseAssets(directory string) ([]string, map[string]bool, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve release asset directory %q: %w", directory, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, nil, fmt.Errorf("release assets are missing: %s: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, fmt.Errorf("release asset directory must be a non-symlink directory: %s", directory)
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, nil, fmt.Errorf("read release asset directory %s: %w", directory, err)
	}
	assets := make([]string, 0, len(entries))
	wanted := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(absolute, name)
		relative, err := filepath.Rel(absolute, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return nil, nil, fmt.Errorf("release asset escapes its directory: %s", name)
		}
		assetInfo, err := os.Lstat(path)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect release asset %s: %w", name, err)
		}
		if assetInfo.Mode()&os.ModeSymlink != 0 || !assetInfo.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("release asset must be a non-symlink regular file: %s", name)
		}
		assets = append(assets, path)
		wanted[name] = true
	}
	if len(assets) == 0 {
		return nil, nil, fmt.Errorf("release assets are missing: %s", directory)
	}
	return assets, wanted, nil
}

type releaseAssetList struct {
	Assets *[]struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func decodeReleaseAssets(data []byte) ([]string, error) {
	var response releaseAssetList
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode GitHub Release assets: %w", err)
	}
	if response.Assets == nil {
		return nil, fmt.Errorf("decode GitHub Release assets: response has no assets array")
	}
	names := make([]string, 0, len(*response.Assets))
	for index, asset := range *response.Assets {
		if asset.Name == "" || strings.ContainsRune(asset.Name, 0) {
			return nil, fmt.Errorf("GitHub Release asset %d has an invalid empty or NUL-containing name", index)
		}
		names = append(names, asset.Name)
	}
	return names, nil
}

func explicitReleaseNotFound(diagnostics string) bool {
	normalized := strings.ToLower(diagnostics)
	return strings.Contains(normalized, "release not found")
}

func externalCommandError(operation string, err error, diagnostics string) error {
	message := strings.TrimSpace(diagnostics)
	if message == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, message)
}

func executePublish(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) error {
	if len(args) != 3 {
		return usage("publish-github-release TAG NOTES_FILE DIST_DIR")
	}
	parsed, err := requireTag(args[0])
	if err != nil {
		return err
	}
	repository := opts.Getenv("GITHUB_REPOSITORY")
	if repository == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is required")
	}
	if err := validateRepository(repository); err != nil {
		return err
	}
	notes, err := regularFile(args[1], "release notes", true)
	if err != nil {
		return err
	}
	assets, wanted, err := releaseAssets(args[2])
	if err != nil {
		return err
	}
	tag := args[0]
	prerelease := "--prerelease=false"
	if parsed.prerelease != "" {
		prerelease = "--prerelease=true"
	}
	metadata := []string{"--repo", repository, "--title", "Release " + tag, "--notes-file", notes, "--latest=false", prerelease}
	var existingJSON, viewDiagnostics bytes.Buffer
	viewArgs := []string{"release", "view", "--repo", repository, "--json", "assets", "--", tag}
	viewErr := opts.Runner.Run(ctx, Command{Name: "gh", Args: viewArgs, Stdout: &existingJSON, Stderr: &viewDiagnostics})
	if viewErr != nil {
		if !explicitReleaseNotFound(viewDiagnostics.String()) {
			return externalCommandError("check whether GitHub Release "+tag+" exists", viewErr, viewDiagnostics.String())
		}
		createArgs := append([]string{"release", "create"}, metadata...)
		createArgs = append(createArgs, "--verify-tag", "--", tag)
		createArgs = append(createArgs, assets...)
		if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: createArgs, Stdout: stdout, Stderr: stderr}); err != nil {
			return fmt.Errorf("create GitHub Release %s: %w", tag, err)
		}
		return nil
	}
	existing, err := decodeReleaseAssets(existingJSON.Bytes())
	if err != nil {
		return err
	}
	uploadArgs := []string{"release", "upload", "--repo", repository, "--clobber", "--", tag}
	uploadArgs = append(uploadArgs, assets...)
	if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: uploadArgs, Stdout: stdout, Stderr: stderr}); err != nil {
		return fmt.Errorf("upload assets for GitHub Release %s: %w", tag, err)
	}
	for _, name := range existing {
		if wanted[name] {
			continue
		}
		deleteArgs := []string{"release", "delete-asset", "--repo", repository, "--yes", "--", tag, name}
		if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: deleteArgs, Stdout: stdout, Stderr: stderr}); err != nil {
			return fmt.Errorf("delete stale asset %q from GitHub Release %s: %w", name, tag, err)
		}
	}
	editArgs := append([]string{"release", "edit"}, metadata...)
	editArgs = append(editArgs, "--draft=false", "--", tag)
	if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: editArgs, Stdout: stdout, Stderr: stderr}); err != nil {
		return fmt.Errorf("update metadata for GitHub Release %s: %w", tag, err)
	}
	return nil
}

func executeReconcile(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) error {
	if len(args) != 2 {
		return usage("reconcile-release-aliases TAG IMAGE")
	}
	current, err := requireTag(args[0])
	if err != nil {
		return err
	}
	if err := validateImage(args[1]); err != nil {
		return err
	}
	repository := opts.Getenv("GITHUB_REPOSITORY")
	if repository == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is required")
	}
	if err := validateRepository(repository); err != nil {
		return err
	}
	var releases bytes.Buffer
	apiArgs := []string{"api", "--paginate", "repos/" + repository + "/releases?per_page=100", "--jq", ".[] | select(.draft == false and .prerelease == false) | .tag_name"}
	if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: apiArgs, Stdout: &releases, Stderr: stderr}); err != nil {
		return fmt.Errorf("list published GitHub Releases: %w", err)
	}
	temp, err := os.CreateTemp("", "ci-recipes-release-tags-*")
	if err != nil {
		return fmt.Errorf("create temporary release-tag list: %w", err)
	}
	path := temp.Name()
	defer os.Remove(path)
	if _, err := io.Copy(temp, &releases); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary release-tag list: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary release-tag list: %w", err)
	}
	plan, err := planAliases(args[0], path)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		if _, err := fmt.Fprintf(stdout, "Prerelease %s keeps only its immutable full-version image.\n", args[0]); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return nil
	}
	type aliasUpdate struct {
		alias
		digest    string
		sourceRef string
		aliasRef  string
	}
	updates := make([]aliasUpdate, 0, len(plan))
	latestRelease := ""
	for _, item := range plan {
		sourceRef := args[1] + ":" + strings.TrimPrefix(item.tag, "v")
		aliasRef := args[1] + ":" + item.name
		if len(strings.TrimPrefix(item.tag, "v")) > 128 || len(item.name) > 128 {
			return fmt.Errorf("OCI tag exceeds 128 characters for alias %s", item.name)
		}
		var inspected bytes.Buffer
		if err := opts.Runner.Run(ctx, Command{Name: "docker", Args: []string{"buildx", "imagetools", "inspect", "--", sourceRef}, Stdout: &inspected, Stderr: stderr}); err != nil {
			return fmt.Errorf("inspect immutable image %s: %w", sourceRef, err)
		}
		digest, err := parseDigest(inspected.String())
		if err != nil {
			return fmt.Errorf("inspect immutable image %s: %w", sourceRef, err)
		}
		if !digestPattern.MatchString(digest) {
			return fmt.Errorf("unable to resolve an immutable digest for %s", sourceRef)
		}
		updates = append(updates, aliasUpdate{alias: item, digest: digest, sourceRef: sourceRef, aliasRef: aliasRef})
		if item.name == "latest" {
			latestRelease = item.tag
		}
	}
	if latestRelease == "" {
		return fmt.Errorf("alias plan does not identify the GitHub Latest Release")
	}
	minorAlias := current.major + "." + current.minor
	sort.SliceStable(updates, func(i, j int) bool {
		rank := func(name string) int {
			switch name {
			case minorAlias:
				return 0
			case current.major:
				return 1
			case "latest":
				return 2
			default:
				return 3
			}
		}
		return rank(updates[i].name) < rank(updates[j].name)
	})
	for index, update := range updates {
		createArgs := []string{"buildx", "imagetools", "create", "--tag", update.aliasRef, "--", args[1] + "@" + update.digest}
		if err := opts.Runner.Run(ctx, Command{Name: "docker", Args: createArgs, Stdout: stdout, Stderr: stderr}); err != nil {
			if index == 0 {
				return fmt.Errorf("update OCI alias %s from %s: %w", update.name, update.sourceRef, err)
			}
			return fmt.Errorf("update OCI alias %s from %s after %d successful alias update(s); registry aliases may be partially reconciled: %w", update.name, update.sourceRef, index, err)
		}
	}
	latestArgs := []string{"release", "edit", "--repo", repository, "--latest", "--", latestRelease}
	if err := opts.Runner.Run(ctx, Command{Name: "gh", Args: latestArgs, Stdout: stdout, Stderr: stderr}); err != nil {
		return fmt.Errorf("mark GitHub Release %s as Latest after updating all OCI aliases: %w", latestRelease, err)
	}
	return nil
}

func parseDigest(output string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	digest := ""
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "Digest:" {
			if digest != "" && fields[1] != digest {
				return "", fmt.Errorf("docker inspect output contains conflicting digests")
			}
			digest = fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read docker inspect output: %w", err)
	}
	return digest, nil
}

func writeAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(directory, ".ci-recipes-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		temp.Close()
		if !ok {
			os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}
