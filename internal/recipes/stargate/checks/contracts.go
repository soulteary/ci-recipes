package checks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func executeDocContracts(ctx context.Context, root, baseSHA string, stdout, stderr io.Writer, runner commandRunner) error {
	structure, err := markdownViolations(ctx, root)
	if err != nil {
		return err
	}
	if len(structure) != 0 {
		if err := writeDiagnostics(stderr, structure); err != nil {
			return err
		}
		return fmt.Errorf("Markdown structure checks failed with %d violation(s)", len(structure))
	}
	currentViolations, err := contractViolations(ctx, root)
	if err != nil {
		return err
	}

	if baseSHA != "" {
		baseRoot, err := archiveRevision(ctx, runner, root, baseSHA, baseArchivePaths, stderr)
		if err != nil {
			return err
		}
		defer os.RemoveAll(baseRoot)
		baseViolations, err := contractViolations(ctx, baseRoot)
		if err != nil {
			return fmt.Errorf("inspect base documentation contracts: %w", err)
		}
		if err := writef(stdout, "Documentation contract violations: base=%d current=%d\n", len(baseViolations), len(currentViolations)); err != nil {
			return fmt.Errorf("write documentation comparison: %w", err)
		}
		if len(currentViolations) > len(baseViolations) {
			if err := writeDiagnostics(stderr, currentViolations); err != nil {
				return err
			}
			return errors.New("Documentation contract violations increased")
		}
	} else if len(currentViolations) != 0 {
		if err := writef(stderr, "Documentation contract violations: %d\n", len(currentViolations)); err != nil {
			return fmt.Errorf("write documentation violation count: %w", err)
		}
		if err := writeDiagnostics(stderr, currentViolations); err != nil {
			return err
		}
		return errors.New("Documentation contract checks failed")
	}

	if err := writef(stdout, "Documentation contract checks passed.\n"); err != nil {
		return fmt.Errorf("write documentation result: %w", err)
	}
	return nil
}

func writeDiagnostics(stderr io.Writer, diagnostics []string) error {
	for _, diagnostic := range diagnostics {
		if err := writef(stderr, "%s\n", diagnostic); err != nil {
			return fmt.Errorf("write diagnostic: %w", err)
		}
	}
	return nil
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

func requireGlob(pattern, label string) ([]string, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", label, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no %s files matched %q", label, pattern)
	}
	sort.Strings(paths)
	return paths, nil
}

func contractViolations(ctx context.Context, root string) ([]string, error) {
	violations := make([]string, 0)
	add := func(format string, values ...any) {
		violations = append(violations, fmt.Sprintf(format, values...))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	environment, err := runtimeEnvironment(root)
	if err != nil {
		return nil, err
	}
	configFiles, err := requireGlob(filepath.Join(root, "docs", "*", "CONFIG.md"), "localized CONFIG.md")
	if err != nil {
		return nil, err
	}
	runtimeEnvironmentSet := make(map[string]bool, len(environment))
	for _, name := range environment {
		runtimeEnvironmentSet[name] = true
	}
	documentedSettingPattern := regexp.MustCompile(`(?m)^(?:\|\s*|#{3,6}\s*)` + "`" + `([A-Z][A-Z0-9_]*)` + "`" + ``)
	for _, path := range configFiles {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		for _, name := range environment {
			if !strings.Contains(text, "`"+name+"`") {
				add("Missing %s in %s", name, path)
			}
		}
		documented := map[string]bool{}
		for _, match := range documentedSettingPattern.FindAllStringSubmatch(text, -1) {
			documented[match[1]] = true
		}
		names := make([]string, 0, len(documented))
		for name := range documented {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !runtimeEnvironmentSet[name] {
				add("Documented setting %s is not registered at runtime in %s", name, path)
			}
		}
	}

	routes, err := runtimeRoutes(root)
	if err != nil {
		return nil, err
	}
	apiFiles, err := requireGlob(filepath.Join(root, "docs", "*", "API.md"), "localized API.md")
	if err != nil {
		return nil, err
	}
	for _, path := range apiFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		checkAPIRoutes(text, path, routes, add)
		checkAPISections(text, path, add)
	}

	securityFiles, err := requireGlob(filepath.Join(root, "docs", "*", "SECURITY.md"), "localized SECURITY.md")
	if err != nil {
		return nil, err
	}
	for _, path := range securityFiles {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		for _, term := range []string{"DEBUG=true", "DEBUG=false", "HERALD_TEST_MODE", "debug_code", "POST /_send_verify_code"} {
			if !strings.Contains(text, term) {
				add("Missing debug verification-code security contract %s in %s", term, path)
			}
		}
	}

	deploymentFiles, err := requireGlob(filepath.Join(root, "docs", "*", "DEPLOYMENT.md"), "localized DEPLOYMENT.md")
	if err != nil {
		return nil, err
	}
	migrationFiles, err := requireGlob(filepath.Join(root, "docs", "*", "MIGRATION_V1.md"), "localized MIGRATION_V1.md")
	if err != nil {
		return nil, err
	}
	for _, path := range deploymentFiles {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		for _, term := range []string{"SESSION_STORAGE_ENABLED=false", "SESSION_STORAGE_ENABLED=true", "SESSION_STORAGE_REDIS_*", "SESSION_EXCHANGE_SECRET"} {
			if !strings.Contains(text, term) {
				add("Missing session-storage deployment scope %s in %s", term, path)
			}
		}
		checkMigrationTerms(text, path, "safe v1 environment migration", add)
	}
	for _, path := range migrationFiles {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		checkMigrationTerms(text, path, "v1 migration environment", add)
	}

	changelogPath := filepath.Join(root, "CHANGELOG.md")
	changelog, err := readText(changelogPath)
	if err != nil {
		return nil, err
	}
	checkChangelog(changelog, changelogPath, add)
	if err := checkForbiddenDocumentation(ctx, root, add); err != nil {
		return nil, err
	}
	if err := checkCompose(root, deploymentFiles, add); err != nil {
		return nil, err
	}
	return violations, nil
}

var (
	environmentRegistryPattern = regexp.MustCompile(`(?s)var envVariables = \[\]\*EnvVariable\{(.*?)\}\s*\n`)
	environmentSymbolPattern   = regexp.MustCompile(`&([A-Za-z0-9_]+)`)
	environmentNamePattern     = regexp.MustCompile(`Name:\s*"([A-Z0-9_]+)"`)
)

func runtimeEnvironment(root string) ([]string, error) {
	path := filepath.Join(root, "src", "internal", "config", "config.go")
	source, err := readText(path)
	if err != nil {
		return nil, err
	}
	registry := environmentRegistryPattern.FindStringSubmatch(source)
	if registry == nil {
		return nil, fmt.Errorf("cannot find runtime environment-variable registry in %s", path)
	}
	symbolMatches := environmentSymbolPattern.FindAllStringSubmatch(registry[1], -1)
	if len(symbolMatches) == 0 {
		return nil, fmt.Errorf("runtime environment-variable registry in %s is empty", path)
	}
	result := make([]string, 0, len(symbolMatches)+1)
	for _, match := range symbolMatches {
		declaration := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(match[1]) + `\s*=\s*EnvVariable\{(.*?)\}`).FindStringSubmatch(source)
		if declaration == nil {
			return nil, fmt.Errorf("cannot find declaration for %s in %s", match[1], path)
		}
		name := environmentNamePattern.FindStringSubmatch(declaration[1])
		if name == nil {
			return nil, fmt.Errorf("cannot find environment name for %s in %s", match[1], path)
		}
		result = append(result, name[1])
	}
	result = append(result, "LOG_LEVEL")
	return result, nil
}

func runtimeRoutes(root string) (map[string]bool, error) {
	constantsPath := filepath.Join(root, "src", "cmd", "stargate", "constants.go")
	constants, err := readText(constantsPath)
	if err != nil {
		return nil, err
	}
	constantPattern := regexp.MustCompile(`\b(Route[A-Za-z0-9_]+)\s*=\s*"([^"]+)"`)
	constantValues := map[string]string{}
	for _, match := range constantPattern.FindAllStringSubmatch(constants, -1) {
		constantValues[match[1]] = match[2]
	}
	if len(constantValues) == 0 {
		return nil, fmt.Errorf("cannot find route constants in %s", constantsPath)
	}

	serverPath := filepath.Join(root, "src", "cmd", "stargate", "server.go")
	server, err := readText(serverPath)
	if err != nil {
		return nil, err
	}
	start := strings.Index(server, "func setupRoutes")
	if start < 0 {
		return nil, fmt.Errorf("cannot find setupRoutes in %s", serverPath)
	}
	endRelative := strings.Index(server[start:], "\n}\n\n// findAssetsPath")
	if endRelative < 0 {
		return nil, fmt.Errorf("cannot find end of setupRoutes in %s", serverPath)
	}
	routeSource := server[start : start+endRelative+2]
	routePattern := regexp.MustCompile(`(?s)\bapp\.(Get|Post|Put|Patch|Delete)\(\s*(Route[A-Za-z0-9_]+|"[^"]+")`)
	routes := map[string]bool{}
	for _, match := range routePattern.FindAllStringSubmatch(routeSource, -1) {
		method := strings.ToUpper(match[1])
		expression := match[2]
		path := strings.Trim(expression, "\"")
		if !strings.HasPrefix(expression, "\"") {
			var ok bool
			path, ok = constantValues[expression]
			if !ok {
				return nil, fmt.Errorf("cannot resolve route constant %s", expression)
			}
		}
		routes[method+" "+path] = true
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("setupRoutes in %s contains no recognized routes", serverPath)
	}
	for _, route := range []string{"GET /log/level", "PUT /log/level", "POST /log/level"} {
		routes[route] = true
	}
	return routes, nil
}

func checkAPIRoutes(text, path string, routes map[string]bool, add func(string, ...any)) {
	headingPattern := regexp.MustCompile("(?m)^###\\s+`((?:GET|POST|PUT|PATCH|DELETE) /[^`]*)`\\s*$")
	headings := map[string]bool{}
	for _, match := range headingPattern.FindAllStringSubmatch(text, -1) {
		headings[match[1]] = true
	}
	keys := make([]string, 0, len(routes))
	for route := range routes {
		keys = append(keys, route)
	}
	sort.Strings(keys)
	mentionOnly := map[string]bool{"GET /health": true, "PUT /log/level": true, "POST /log/level": true}
	for _, route := range keys {
		if mentionOnly[route] {
			if !strings.Contains(text, "`"+route+"`") {
				add("Missing compatibility route contract %s in %s", route, path)
			}
		} else if !headings[route] {
			add("Missing route heading %s in %s", route, path)
		}
	}
}

func markdownSection(text, heading string) (string, bool) {
	headingPattern := regexp.MustCompile(`(?m)^### ` + regexp.QuoteMeta("`"+heading+"`") + `[ \t]*\n`)
	location := headingPattern.FindStringIndex(text)
	if location == nil {
		return "", false
	}
	content := text[location[1]:]
	nextHeading := regexp.MustCompile(`(?m)^### `).FindStringIndex(content)
	if nextHeading == nil {
		return content, true
	}
	return content[:nextHeading[0]], true
}

func markedSubsection(section, marker string) (string, bool) {
	pattern := regexp.MustCompile(regexp.QuoteMeta(marker) + `[ \t\r\n]*\n#### [^\n]+\n`)
	location := pattern.FindStringIndex(section)
	if location == nil {
		return "", false
	}
	content := section[location[1]:]
	nextHeading := regexp.MustCompile(`(?m)^#### `).FindStringIndex(content)
	if nextHeading != nil {
		content = content[:nextHeading[0]]
	}
	return content, true
}

func tableFieldCounts(section string) map[string]int {
	counts := map[string]int{}
	pattern := regexp.MustCompile("(?m)^\\|\\s*`([^`]+)`\\s*\\|")
	for _, match := range pattern.FindAllStringSubmatch(section, -1) {
		counts[match[1]]++
	}
	return counts
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}

func checkAPISections(text, path string, add func(string, ...any)) {
	confirm, confirmOK := markdownSection(text, "POST /totp/enroll/confirm")
	confirmRequest, requestOK := markedSubsection(confirm, "<!-- api-contract: totp-enroll-confirm-request-body -->")
	if !confirmOK || !requestOK || !containsAll(confirm, "`enroll_id`", "`code`", "`401 Unauthorized`", "10") ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`application/x-www-form-urlencoded`+"`"+`\s*\|\s*✅\s*\|`).MatchString(confirmRequest) ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`multipart/form-data`+"`"+`\s*\|\s*✅\s*\|`).MatchString(confirmRequest) ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`application/json`+"`"+`\s*\|\s*❌\s*\|`).MatchString(confirmRequest) {
		add("Incomplete TOTP confirmation form or authentication contract in %s", path)
	}

	login, loginOK := markdownSection(text, "POST /_login")
	fields := tableFieldCounts(login)
	missingField := false
	for _, field := range []string{"password", "phone", "mail", "challenge_id", "verify_code", "use_otp", "otp_code"} {
		missingField = missingField || fields[field] < 1
	}
	if !loginOK || fields["auth_method"] < 2 || fields["callback"] < 2 || missingField || !containsAll(login,
		"`phone` + `mail`", "`HERALD_TOTP_ENABLED=true`",
		"auth_method=password&password=yourpassword",
		"auth_method=warden&mail=user@example.com&challenge_id=ch_xxx&verify_code=123456&callback=app.example.com",
		"auth_method=warden&mail=user@example.com&use_otp=true&otp_code=123456&callback=app.example.com",
		"`400 Bad Request`", "`401 Unauthorized`", "`502 Bad Gateway`", "`503 Service Unavailable`", "`500 Internal Server Error`") {
		add("Incomplete password/Warden challenge/TOTP login contract in %s", path)
	}

	send, sendOK := markdownSection(text, "POST /_send_verify_code")
	sendRequest, sendRequestOK := markedSubsection(send, "<!-- api-contract: send-verify-code-request-body -->")
	if !sendOK || !sendRequestOK || !containsAll(send, "`deliver_via`", "`deliver_via=dingtalk`", "`phone` + `mail`", "`401 Unauthorized`", "`503 Service Unavailable`") ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`application/x-www-form-urlencoded`+"`"+`\s*\|\s*✅\s*\|`).MatchString(sendRequest) ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`multipart/form-data`+"`"+`\s*\|\s*✅\s*\|`).MatchString(sendRequest) ||
		!regexp.MustCompile(`(?m)^\|\s*`+"`"+`application/json`+"`"+`\s*\|\s*❌\s*\|`).MatchString(sendRequest) {
		add("Incomplete verification-send form or status contract in %s", path)
	}

	enrollPage, enrollPageOK := markdownSection(text, "GET /totp/enroll")
	enrollStart, enrollStartOK := markdownSection(text, "POST /totp/enroll")
	revokePage, revokePageOK := markdownSection(text, "GET /totp/revoke")
	revokeConfirm, revokeConfirmOK := markdownSection(text, "POST /totp/revoke")
	if !enrollPageOK || !containsAll(enrollPage, "10", "`/_login`", "`302 Found`", "`401 Unauthorized`") ||
		!enrollStartOK || !containsAll(enrollStart, "10", "`/_login`", "`302 Found`", "`401 Unauthorized`") ||
		!revokePageOK || !containsAll(revokePage, "`/_login`", "`302 Found`") ||
		!revokeConfirmOK || !containsAll(revokeConfirm, "`password`", "`code`", "`401 Unauthorized`") {
		add("Incomplete TOTP session or reauthentication contract in %s", path)
	}
}

var migrationTerms = []string{
	"stargate-v0.12.0.env",
	"stargate-v1.env",
	"WARDEN_OTP_ENABLED",
	"WARDEN_OTP_SECRET_KEY",
	"HERALD_ENABLED=true",
	"HERALD_TOTP_ENABLED=true",
	"HERALD_URL",
	"HERALD_TOTP_ENCRYPTION_KEY",
	"--env-file ./stargate-v1.env",
	`old_container=${STARGATE_OLD_CONTAINER:-stargate}`,
	`mktemp "${old_env}.tmp.XXXXXX"`,
	`docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$old_container"`,
	`ln "$export_tmp" "$old_env"`,
	`chmod 600 "$old_env"`,
	`set -C; cat "$old_env" > "$rollback_env"`,
	`WARDEN_OTP_(ENABLED|SECRET_KEY)`,
	`PORT[[:space:]]*(=|$)`,
	`print "PORT=8080"`,
	`test ! -e "$rollback_env"`,
	`test ! -e "$v1_env"`,
	`umask 077`,
	`trap 'rm -f "$export_tmp"' 0 1 2 15`,
	`rm -f "$export_tmp"`,
	`trap - 0 1 2 15`,
	`(set -C; awk '`,
	`"$old_env" > "$v1_env")`,
}

func checkMigrationTerms(text, path, label string, add func(string, ...any)) {
	for _, term := range migrationTerms {
		if !strings.Contains(text, term) {
			add("Missing %s contract %s in %s", label, term, path)
		}
	}
	wardenPrerequisite := regexp.MustCompile("`WARDEN_ENABLED=true`[^\\n]*`WARDEN_URL`")
	if !wardenPrerequisite.MatchString(text) {
		add("Missing Warden prerequisites for Herald TOTP migration in %s", path)
	}
}

func checkChangelog(changelog, path string, add func(string, ...any)) {
	versionHeading := regexp.MustCompile(`(?m)^## \[1\.0\.0\][^\n]*\n`).FindStringIndex(changelog)
	if versionHeading == nil {
		add("Missing v1.0.0 Breaking changes section in %s", path)
		return
	}
	versionSection := changelog[versionHeading[1]:]
	if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(versionSection); next != nil {
		versionSection = versionSection[:next[0]]
	}
	breakingHeading := regexp.MustCompile(`(?m)^### Breaking changes[ \t]*\n`).FindStringIndex(versionSection)
	if breakingHeading == nil {
		add("Missing v1.0.0 Breaking changes section in %s", path)
		return
	}
	breaking := versionSection[breakingHeading[1]:]
	if next := regexp.MustCompile(`(?m)^### `).FindStringIndex(breaking); next != nil {
		breaking = breaking[:next[0]]
	}
	portPattern := regexp.MustCompile(`(?i)container[^\n]*` + "`8080`" + `[^\n]*` + "`80`")
	if !portPattern.MatchString(breaking) {
		add("Missing container port 80 to 8080 migration in v1.0.0 Breaking changes")
	}
	if !strings.Contains(breaking, "PASSWORD_HEADER_AUTH_ENABLED=true") {
		add("Missing password-header authentication opt-in in v1.0.0 Breaking changes")
	}
}

func checkForbiddenDocumentation(ctx context.Context, root string, add func(string, ...any)) error {
	paths, err := markdownPathsForPatterns(root)
	if err != nil {
		return err
	}
	patterns := []struct {
		label   string
		pattern *regexp.Regexp
	}{
		{"legacy Redis setting", regexp.MustCompile(`\b(?:REDIS_ENABLED|REDIS_ADDR|REDIS_PASSWORD)\b`)},
		{"stale Fiber version", regexp.MustCompile(`Fiber v2\.52`)},
		{"invalid bcrypt command", regexp.MustCompile(`go run -c [^\n]*golang\.org/x/crypto/bcrypt`)},
		{"overstated Herald Key ID requirement", regexp.MustCompile(`(?:also requires|还需要) ` + "`HERALD_HMAC_KEY_ID`")},
		{"unsupported readiness claim", regexp.MustCompile(`(?i)(?:Enterprise-Grade|Enterprise Authentication|Battle-tested)`)},
		{"verification endpoint incorrectly claims JSON input", regexp.MustCompile(`(?i)(?:or|oder|ou|o|または|또는|或)\s+JSON\s*\(` + "`application/json`" + `\)`)},
		{"container health check omits port 8080", regexp.MustCompile(`http://localhost/healthz`)},
		{"metrics incorrectly described as new in v1", regexp.MustCompile(`(?:No metrics endpoint|无指标端点|Added Prometheus metrics)`)},
		{"stale Go 1.26 requirement", regexp.MustCompile(`(?i)\bGo(?:\s+(?:Version|版本))?\s*[:：]?\s*1\.26(?:\.\d+)?\+?\b`)},
		{"unsafe htpasswd batch-password option", regexp.MustCompile(`\bhtpasswd\s+-[A-Za-z]*b[A-Za-z]*\b`)},
	}
	retiredPattern := regexp.MustCompile(`\bWARDEN_OTP_(?:ENABLED|SECRET_KEY)\b`)
	authRefreshPattern := regexp.MustCompile(`(?s)#### ` + "`AUTH_REFRESH_ENABLED`" + `.*?\|\s*\*\*(?:Default|默认值)\*\*\s*\|\s*` + "`false`")
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, err := readText(path)
		if err != nil {
			return err
		}
		retiredText := text
		if strings.HasSuffix(filepath.ToSlash(path), "/DEPLOYMENT.md") || strings.HasSuffix(filepath.ToSlash(path), "/MIGRATION_V1.md") {
			retiredText = strings.ReplaceAll(retiredText, "`WARDEN_OTP_ENABLED`", "")
			retiredText = strings.ReplaceAll(retiredText, "`WARDEN_OTP_SECRET_KEY`", "")
		}
		for range retiredPattern.FindAllStringIndex(retiredText, -1) {
			add("active or ambiguous retired Warden OTP setting in %s", path)
		}
		for _, entry := range patterns {
			for range entry.pattern.FindAllStringIndex(text, -1) {
				add("%s in %s", entry.label, path)
			}
		}
		if strings.HasSuffix(filepath.ToSlash(path), "/ARCHITECTURE.md") {
			for _, line := range strings.Split(text, "\n") {
				lower := strings.ToLower(line)
				image := strings.Index(lower, "alpine:3.24")
				if image >= 0 && strings.Contains(lower[image+len("alpine:3.24"):], "curl") {
					add("runtime image incorrectly claims curl in %s", path)
				}
			}
		}
		for range authRefreshPattern.FindAllStringIndex(text, -1) {
			add("incorrect auth-refresh default in %s", path)
		}
		checkShellDocumentationLines(text, path, add)
	}
	return nil
}

func markdownPathsForPatterns(root string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk documentation patterns: %w", err)
	}
	readmes, err := filepath.Glob(filepath.Join(root, "README*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob README files: %w", err)
	}
	if len(readmes) == 0 {
		return nil, errors.New("no README*.md files found")
	}
	paths = append(paths, readmes...)
	sort.Strings(paths)
	return paths, nil
}

func containsUnescapedDollar(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] != '$' {
			continue
		}
		beforeDollar := index > 0 && value[index-1] == '$'
		afterDollar := index+1 < len(value) && value[index+1] == '$'
		if !beforeDollar && !afterDollar {
			return true
		}
	}
	return false
}

func checkShellDocumentationLines(text, path string, add func(string, ...any)) {
	lines := strings.Split(text, "\n")
	curlHeaderPattern := regexp.MustCompile(`^[ \t]*curl[ \t]+-H[ \t]+["']Stargate-Password:`)
	for index, line := range lines {
		if strings.HasPrefix(line, "PASSWORDS=") || strings.HasPrefix(line, "export PASSWORDS=") {
			value := line[strings.IndexByte(line, '=')+1:]
			if value != "" && value[0] != '\'' && value[0] != '"' && strings.ContainsAny(value, "|$") {
				add("unquoted password shell assignment in %s", path)
			}
		}
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "-e ") {
			assignment := strings.TrimPrefix(trimmed, "-e ")
			for _, name := range []string{"PASSWORDS", "LOGIN_PAGE_TITLE", "LOGIN_PAGE_FOOTER_TEXT"} {
				prefix := name + "="
				if strings.HasPrefix(assignment, prefix) {
					value := strings.TrimPrefix(assignment, prefix)
					if value != "" && value[0] != '\'' && value[0] != '"' && strings.ContainsAny(value, "|$ \t\r\v\f") {
						add("unquoted docker environment value in %s", path)
					}
				}
			}
		}
		if strings.HasPrefix(trimmed, "-") {
			rest := strings.TrimPrefix(trimmed, "-")
			if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
				assignment := strings.TrimLeft(rest, " \t")
				if strings.HasPrefix(assignment, "PASSWORDS=bcrypt:") && containsUnescapedDollar(assignment) {
					add("unescaped Compose bcrypt value in %s", path)
				}
			}
		}
		if curlHeaderPattern.MatchString(line) {
			start := index - 2
			if start < 0 {
				start = 0
			}
			context := strings.Join(lines[start:index], "\n")
			if !strings.Contains(context, "PASSWORD_HEADER_AUTH_ENABLED=true") {
				add("header-auth command omits server enablement prerequisite in %s", path)
			}
		}
	}
}

func checkCompose(root string, deploymentFiles []string, add func(string, ...any)) error {
	composePath := filepath.Join(root, "docker-compose.yml")
	compose, err := readText(composePath)
	if err != nil {
		return err
	}
	networkPattern := regexp.MustCompile(`(?m)^networks:\s*\n\s{2}([^:\s]+):\s*\n\s{4}name:\s*([^\s#]+)`)
	network := networkPattern.FindStringSubmatch(compose)
	if network == nil {
		add("Cannot resolve the Compose-managed Traefik network in %s", composePath)
	} else if network[1] != "traefik" || network[2] != "stargate-traefik" {
		add("Bundled Compose must preserve logical key traefik and actual name stargate-traefik in %s", composePath)
	} else {
		attachmentPattern := regexp.MustCompile(`(?m)^ {6}-[ \t]+traefik[ \t]*$`)
		if len(attachmentPattern.FindAllStringIndex(compose, -1)) < 3 {
			add("Not every bundled service uses logical network key traefik in %s", composePath)
		}
		labelPattern := regexp.MustCompile(`traefik\.docker\.network=([^\s"]+)`)
		labels := labelPattern.FindAllStringSubmatch(compose, -1)
		if len(labels) == 0 {
			add("Missing Traefik Docker network labels in %s", composePath)
		}
		for _, label := range labels {
			if label[1] != network[2] {
				add("Traefik label network %s does not match %s in %s", label[1], network[2], composePath)
			}
		}
	}
	subnet := regexp.MustCompile(`(?m)^\s*-\s+subnet:\s*([^\s#]+)`).FindStringSubmatch(compose)
	trusted := regexp.MustCompile(`(?m)^\s*-\s+TRUSTED_PROXIES=([^\s#]+)`).FindStringSubmatch(compose)
	if subnet == nil || trusted == nil || subnet[1] != trusted[1] {
		add("Compose subnet and TRUSTED_PROXIES differ in %s", composePath)
	}

	externalName := `${TRAEFIK_NETWORK_NAME:-traefik}`
	requirements := []struct{ label, value string }{
		{"bundled network name", "stargate-traefik"},
		{"bundled Compose validation", "docker compose config"},
		{"external network name export", `export TRAEFIK_NETWORK_NAME="${TRAEFIK_NETWORK_NAME:-traefik}"`},
		{"existing network inspection", `docker network inspect "$TRAEFIK_NETWORK_NAME"`},
		{"explicit external subnet creation", "--subnet 172.30.0.0/24"},
		{"created subnet export", "export TRAEFIK_NETWORK_CIDR=172.30.0.0/24"},
		{"inspected trusted-proxy CIDR", `TRUSTED_PROXIES=${TRAEFIK_NETWORK_CIDR:?set TRAEFIK_NETWORK_CIDR from docker network inspect}`},
		{"external Compose network name", "name: " + externalName},
		{"external network declaration", "external: true"},
		{"external Traefik label", "traefik.docker.network=" + externalName},
	}
	for _, path := range deploymentFiles {
		text, err := readText(path)
		if err != nil {
			return err
		}
		for _, requirement := range requirements {
			if !strings.Contains(text, requirement.value) {
				add("Missing %s contract in %s", requirement.label, path)
			}
		}
		if strings.Contains(text, "TRUSTED_PROXIES=172.30.0.0/24") || strings.Contains(text, "traefik.docker.network=traefik") || strings.Contains(text, "docker network create traefik") {
			add("Hard-coded external Traefik network contract in %s", path)
		}
		labelPattern := regexp.MustCompile(`traefik\.docker\.network=([^\s"]+)`)
		for _, match := range labelPattern.FindAllStringSubmatch(text, -1) {
			if match[1] != externalName {
				add("External Traefik label does not use the configured network name in %s", path)
			}
		}
	}
	return nil
}
