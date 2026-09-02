package grantseal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var privateKeyBegin = []byte("-----BEGIN ")
var privateKeyEnd = []byte("PRIVATE KEY-----")

var sensitiveBasenames = []string{
	"*-private.key", "private-*.key", "*-private.pem", "private-*.pem", "id_rsa", "id_ed25519",
}

var sensitiveExcludedDirs = map[string]struct{}{
	".git": {}, "scripts": {}, "node_modules": {}, "vendor": {},
}

type sensitiveScanner struct {
	ctx      context.Context
	deps     dependencies
	stdout   io.Writer
	stderr   io.Writer
	repoRoot string
	gitOK    bool
	found    bool
}

func runSensitiveFiles(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	paths := args
	if len(paths) == 0 {
		paths = []string{"dist"}
	}
	scanner := &sensitiveScanner{ctx: ctx, deps: deps, stdout: stdout, stderr: stderr}
	if err := scanner.initGit(); err != nil {
		return err
	}
	if err := scanner.scanTracked(); err != nil {
		return err
	}
	for _, target := range paths {
		if target == "" {
			return usage("sensitive files: scan path must not be empty")
		}
		if err := scanner.scanTarget(target); err != nil {
			return err
		}
	}
	if scanner.found {
		return rejected("private-key material found (tracked files and/or scanned artifacts)")
	}
	fmt.Fprintln(stdout, "sensitive-file check passed: no private-key material in tracked files or scanned paths")
	return nil
}

func (s *sensitiveScanner) initGit() error {
	if _, err := s.deps.runner.LookPath("git"); err != nil {
		fmt.Fprintln(s.stderr, "note: git not found; skipping git-tracked audit")
		return nil
	}
	data, err := runOutput(s.ctx, s.deps, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		if s.ctx.Err() == nil && commandExitCode(err) == 128 && isNotGitWorkTreeError(err) {
			fmt.Fprintln(s.stderr, "note: not inside a git work tree; skipping git-tracked audit")
			return nil
		}
		return usage("inspect git work tree: %v", err)
	}
	root := strings.TrimSpace(string(data))
	if root == "" || strings.ContainsRune(root, 0) {
		return usage("git returned an invalid work-tree root")
	}
	s.repoRoot = filepath.Clean(root)
	s.gitOK = true
	return nil
}

func (s *sensitiveScanner) scanTracked() error {
	if !s.gitOK {
		return nil
	}
	data, err := runOutput(s.ctx, s.deps, "git", "-C", s.repoRoot, "ls-files", "-z", "--full-name")
	if err != nil {
		return usage("enumerate git-tracked files: %v", err)
	}
	paths, err := splitNUL(data)
	if err != nil {
		return usage("parse git-tracked file list: %v", err)
	}
	var nameHits, contentHits []string
	for _, rel := range paths {
		base := filepath.Base(filepath.FromSlash(rel))
		if sensitiveFilename(base) {
			nameHits = append(nameHits, rel)
		}
		if rel == "scripts/check-sensitive-files.sh" || rel == "scripts/check-sensitive-files-selftest.sh" {
			continue
		}
		name := filepath.Join(s.repoRoot, filepath.FromSlash(rel))
		info, statErr := os.Stat(name)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return usage("stat tracked file %q: %v", rel, statErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		hasHeader, scanErr := fileContainsPrivateKeyHeader(name)
		if scanErr != nil {
			return usage("scan tracked file %q: %v", rel, scanErr)
		}
		if hasHeader {
			contentHits = append(contentHits, rel)
		}
	}
	if len(nameHits) != 0 {
		fmt.Fprintln(s.stderr, "sensitive tracked filename(s) detected (git):")
		for _, name := range nameHits {
			fmt.Fprintln(s.stderr, name)
		}
		s.found = true
	}
	if len(contentHits) != 0 {
		fmt.Fprintln(s.stderr, "PEM private-key header(s) in tracked file(s) (git):")
		for _, name := range contentHits {
			fmt.Fprintln(s.stderr, name)
		}
		s.found = true
	}
	return nil
}

func (s *sensitiveScanner) scanTarget(display string) error {
	target := resolvePath(s.deps.workDir, display)
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(s.stderr, "note: scan path %q does not exist, skipping\n", display)
		return nil
	}
	if err != nil {
		return usage("inspect scan path %q: %v", display, err)
	}
	rootAudit := display == "." || display == "./" || (s.gitOK && samePath(target, s.repoRoot))
	if !info.IsDir() {
		return s.scanArtifactFile(target, display, rootAudit && s.gitOK)
	}
	return filepath.WalkDir(target, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return usage("walk scan path %q: %v", display, walkErr)
		}
		if entry.IsDir() {
			if name != target {
				if _, excluded := sensitiveExcludedDirs[entry.Name()]; rootAudit && excluded {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return usage("inspect file %q: %v", name, err)
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		shown, err := filepath.Rel(s.deps.workDir, name)
		if err != nil {
			shown = name
		}
		return s.scanArtifactFile(name, shown, rootAudit && s.gitOK)
	})
}

func isNotGitWorkTreeError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not a git repository") ||
		strings.Contains(message, "not a git work tree") ||
		strings.Contains(message, "must be run in a work tree")
}

func (s *sensitiveScanner) scanArtifactFile(name, shown string, applyIgnore bool) error {
	if applyIgnore {
		ignored, err := s.gitIgnored(name)
		if err != nil {
			return err
		}
		if ignored {
			return nil
		}
	}
	if sensitiveFilename(filepath.Base(name)) {
		fmt.Fprintf(s.stderr, "sensitive filename detected under scanned paths: %s\n", shown)
		s.found = true
	}
	hasHeader, err := fileContainsPrivateKeyHeader(name)
	if err != nil {
		return usage("scan file %q: %v", shown, err)
	}
	if hasHeader {
		fmt.Fprintf(s.stderr, "PEM private-key header detected under scanned paths: %s\n", shown)
		s.found = true
	}
	return nil
}

func (s *sensitiveScanner) gitIgnored(name string) (bool, error) {
	canonicalRoot, err := filepath.EvalSymlinks(s.repoRoot)
	if err != nil {
		return false, usage("resolve git work-tree root %q: %v", s.repoRoot, err)
	}
	canonicalName, err := filepath.EvalSymlinks(name)
	if err != nil {
		return false, usage("resolve git-ignore path %q: %v", name, err)
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalName)
	if err != nil {
		return false, usage("resolve git-ignore path %q: %v", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, usage("git-ignore path %q is outside work tree %q", name, s.repoRoot)
	}
	var stderr bytes.Buffer
	err = s.deps.runner.Run(s.ctx, command{
		Name: "git", Args: []string{"-C", s.repoRoot, "check-ignore", "-q", "--", filepath.ToSlash(rel)},
		Dir: s.deps.workDir, Stdout: io.Discard, Stderr: &stderr,
	})
	switch commandExitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		if err == nil {
			return false, nil
		}
		return false, usage("git check-ignore failed for %q: %s", rel, strings.TrimSpace(stderr.String()))
	}
}

func splitNUL(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("NUL-delimited output is not terminated")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			return nil, fmt.Errorf("empty path in NUL-delimited output")
		}
		result = append(result, string(part))
	}
	return result, nil
}

func sensitiveFilename(base string) bool {
	for _, pattern := range sensitiveBasenames {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

func fileContainsPrivateKeyHeader(name string) (bool, error) {
	file, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer file.Close()
	buffer := make([]byte, 32*1024)
	var carry []byte
	afterBegin := false
	for {
		n, readErr := file.Read(buffer)
		data := make([]byte, 0, len(carry)+n)
		data = append(data, carry...)
		data = append(data, buffer[:n]...)
		carry = carry[:0]
		for len(data) != 0 {
			if !afterBegin {
				index := bytes.Index(data, privateKeyBegin)
				if index < 0 {
					if readErr == nil {
						keep := len(privateKeyBegin) - 1
						if keep > len(data) {
							keep = len(data)
						}
						carry = append(carry, data[len(data)-keep:]...)
					}
					break
				}
				afterBegin = true
				data = data[index+len(privateKeyBegin):]
				continue
			}

			end := bytes.Index(data, privateKeyEnd)
			newline := bytes.IndexByte(data, '\n')
			if end >= 0 && (newline < 0 || end < newline) {
				return true, nil
			}
			if newline >= 0 {
				afterBegin = false
				data = data[newline+1:]
				continue
			}
			if readErr == nil {
				keep := len(privateKeyEnd) - 1
				if keep > len(data) {
					keep = len(data)
				}
				carry = append(carry, data[len(data)-keep:]...)
			}
			break
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return false, nil
			}
			return false, readErr
		}
	}
}

func samePath(a, b string) bool {
	infoA, statErrA := os.Stat(a)
	infoB, statErrB := os.Stat(b)
	if statErrA == nil && statErrB == nil && os.SameFile(infoA, infoB) {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
}
