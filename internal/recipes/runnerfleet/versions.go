package runnerfleet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runVersionConsistency replaces scripts/check-version-consistency.sh.
//
// The baseline is the fallback tag of the default runner image in the source
// file: that is the only version number that changes runtime behavior, and
// every other occurrence is documentation or an example that has to agree
// with it.
func runVersionConsistency(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	parsed, err := parseRecipeArgs("ci-recipes runner-fleet check-version-consistency [ROOT] [--config FILE]", args)
	if err != nil {
		return err
	}
	root, err := resolveRoot(deps, parsed.root)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(deps, root, parsed.configPath)
	if err != nil {
		return err
	}

	expected, err := baselineVersion(deps, root, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "基准版本（取自 %s）: %s\n", cfg.versionSource, expected)

	files, err := listRepositoryFiles(ctx, deps, root, cfg.versionPathspecs)
	if err != nil {
		return usage("%v", err)
	}

	violations := 0
	for _, name := range files {
		if cfg.excluded(name) {
			continue
		}
		absolute := filepath.Join(root, filepath.FromSlash(name))
		// A path git lists but the filesystem does not offer as a regular file
		// is a submodule gitlink or a dangling symlink, which the shell's
		// "[ -f ]" guard skipped too.
		info, statErr := os.Stat(absolute)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return usage("stat %s: %v", name, statErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		hits, scanErr := scanVersionReferences(absolute, name, cfg, expected)
		if scanErr != nil {
			return usage("%v", scanErr)
		}
		for _, hit := range hits {
			// The annotation goes to stdout, where GitHub reads it, and the
			// human-readable duplicate to stderr, exactly as the shell did.
			fmt.Fprintf(stdout, "::error file=%s,line=%d::版本号 %s 与基准 %s 不一致\n", hit.file, hit.line, hit.version, expected)
			fmt.Fprintf(stderr, "  %s:%d 出现 %s，应为 %s\n", hit.file, hit.line, hit.version, expected)
			violations++
		}
	}

	if violations != 0 {
		fmt.Fprintln(stderr, "")
		fmt.Fprintf(stderr, "发布新版本时请一并更新上述位置，或修正 %s 中的基准版本。\n", cfg.versionSource)
		fmt.Fprintf(stderr, "若该处确需引用历史版本号（如变更说明），在该行加注释标记 %s 即可跳过。\n", cfg.versionIgnore)
		return rejected("found %d version reference(s) disagreeing with the %s baseline", violations, expected)
	}

	fmt.Fprintf(stdout, "所有文档与示例中的版本号均为 %s\n", expected)
	return nil
}

// baselineVersion returns the first configured baseline match, line by line, the
// way "grep ... | head -n 1" did.
func baselineVersion(deps dependencies, root string, cfg config) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(cfg.versionSource))
	data, err := deps.readFile(path)
	if err != nil {
		return "", usage("read version baseline %s: %v", cfg.versionSource, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		match := cfg.versionBaseline.FindStringSubmatch(scanner.Text())
		if match != nil && len(match) > 1 && match[1] != "" {
			return match[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", usage("read version baseline %s: %v", cfg.versionSource, err)
	}
	return "", usage("无法从 %s 中解析默认镜像 tag（正则 %s 未匹配到任何一行），请检查 DefaultRunnerContainerImage()",
		cfg.versionSource, cfg.versionBaseline)
}

type versionHit struct {
	file    string
	line    int
	version string
}

// scanVersionReferences reports every version on a line that disagrees with the
// baseline. A line carrying the ignore marker is skipped so that prose may cite
// a historical version on purpose.
func scanVersionReferences(path, display string, cfg config, expected string) ([]versionHit, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", display, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var hits []versionHit
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if cfg.versionIgnore != "" && strings.Contains(line, cfg.versionIgnore) {
			continue
		}
		for _, reference := range cfg.versionReferences {
			for _, found := range reference.versions(line) {
				if found != expected {
					hits = append(hits, versionHit{file: display, line: lineNumber, version: found})
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %v", display, err)
	}
	return hits, nil
}
