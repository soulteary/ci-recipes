package grantseal

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveAllowlistCleanTarAndZip(t *testing.T) {
	dir := t.TempDir()
	entries := []tarEntry{{name: "pkg/license-tool", contents: "binary"}, {name: "pkg/LICENSE", contents: "license"}, {name: "README.md", contents: "readme"}}
	writeTarGzip(t, filepath.Join(dir, "pkg.tar.gz"), entries)
	writeZip(t, filepath.Join(dir, "pkg.zip"), entries)
	stdout, stderr, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "check passed") || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestArchiveAllowlistRejectsSelftestFixtures(t *testing.T) {
	for _, planted := range []string{"private.key", "source.go", "config.json"} {
		for _, format := range []string{"tar.gz", "zip"} {
			t.Run(planted+"/"+format, func(t *testing.T) {
				dir := t.TempDir()
				entries := []tarEntry{{name: "license-tool"}, {name: planted}}
				if format == "zip" {
					writeZip(t, filepath.Join(dir, "pkg.zip"), entries)
				} else {
					writeTarGzip(t, filepath.Join(dir, "pkg.tar.gz"), entries)
				}
				_, stderr, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
				requireExitCode(t, err, 1)
				if !strings.Contains(stderr, planted) {
					t.Fatalf("diagnostic %q does not name planted file", stderr)
				}
			})
		}
	}
}

func TestArchiveAllowlistRejectsUnsafePathsAndTypes(t *testing.T) {
	tests := []tarEntry{
		{name: "../license-tool"},
		{name: "/license-tool"},
		{name: "safe/../license-tool"},
		{name: "LICENSE\nREADME"},
		{name: "license-tool", typeflag: tar.TypeSymlink},
		{name: "license-tool", typeflag: tar.TypeLink},
	}
	for _, entry := range tests {
		t.Run(strings.ReplaceAll(entry.name, "/", "_"), func(t *testing.T) {
			dir := t.TempDir()
			writeTarGzip(t, filepath.Join(dir, "pkg.tar.gz"), []tarEntry{entry})
			_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
			requireExitCode(t, err, 1)
		})
	}
}

func TestArchiveAllowlistRejectsWindowsQualifiedPaths(t *testing.T) {
	for _, planted := range []string{"C:/README", "C:README", "safe/C:stream/README"} {
		for _, format := range []string{"tar.gz", "zip"} {
			t.Run(format+"/"+strings.ReplaceAll(planted, "/", "_"), func(t *testing.T) {
				dir := t.TempDir()
				entries := []tarEntry{{name: planted, contents: "hidden"}}
				if format == "zip" {
					writeZip(t, filepath.Join(dir, "pkg.zip"), entries)
				} else {
					writeTarGzip(t, filepath.Join(dir, "pkg.tar.gz"), entries)
				}
				_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
				requireExitCode(t, err, 1)
			})
		}
	}
}

func TestArchiveAllowlistFailsClosedOnCorruptArchive(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "broken.tar.gz"), "not gzip")
	_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
	requireExitCode(t, err, 2)
}

func TestArchiveAllowlistRejectsSymlinkedArchive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "payload")
	writeTarGzip(t, target, []tarEntry{{name: "README.md", contents: "readme"}})
	if err := os.Symlink(target, filepath.Join(dir, "release.tar.gz")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
	requireExitCode(t, err, 2)
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error = %v, want symbolic-link diagnostic", err)
	}
}

func TestArchiveAllowlistVerifiesGzipTrailer(t *testing.T) {
	valid := tarGzipBytes(t, []tarEntry{{name: "license-tool", contents: "binary"}})
	tests := map[string][]byte{
		"bad checksum":      append([]byte(nil), valid...),
		"truncated trailer": append([]byte(nil), valid[:len(valid)-4]...),
	}
	tests["bad checksum"][len(tests["bad checksum"])-8] ^= 0xff
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "broken.tar.gz"), contents, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
			requireExitCode(t, err, 2)
		})
	}
}

func TestArchiveAllowlistRejectsHiddenGzipData(t *testing.T) {
	valid := tarGzipBytes(t, []tarEntry{{name: "README.md", contents: "readme"}})
	hiddenMember := tarGzipBytes(t, []tarEntry{{name: "private.key", contents: "secret"}})
	hiddenTarTail := append(tarBytes(t, []tarEntry{{name: "README.md", contents: "readme"}}), []byte("PRIVATE MATERIAL")...)
	hiddenPadding := tarBytes(t, []tarEntry{{name: "README.md", contents: "x"}})
	copy(hiddenPadding[513:], []byte("PRIVATE MATERIAL"))
	tests := map[string][]byte{
		"concatenated gzip member": append(append([]byte(nil), valid...), hiddenMember...),
		"non-zero tar tail":        gzipTestBytes(t, hiddenTarTail, gzip.Header{}),
		"member alignment padding": gzipTestBytes(t, hiddenPadding, gzip.Header{}),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pkg.tar.gz"), contents, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
			requireExitCode(t, err, 2)
		})
	}
}

func TestArchiveAllowlistRejectsGzipMetadata(t *testing.T) {
	payload := tarBytes(t, []tarEntry{{name: "README.md", contents: "readme"}})
	for _, header := range []gzip.Header{{Name: "hidden.key"}, {Comment: "secret"}, {Extra: []byte("secret")}} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pkg.tar.gz"), gzipTestBytes(t, payload, header), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
		requireExitCode(t, err, 2)
	}
}

func TestArchiveAllowlistRejectsZipHiddenMetadataAndTail(t *testing.T) {
	valid := zipTestBytes(t, "", "", nil)
	tests := map[string][]byte{
		"archive comment":      zipTestBytes(t, "secret", "", nil),
		"entry comment":        zipTestBytes(t, "", "secret", nil),
		"unknown extra":        zipTestBytes(t, "", "", []byte{0xfe, 0xca, 0x01, 0x00, 0x00}),
		"appended tail":        append(append([]byte(nil), valid...), []byte("-----BEGIN PRIVATE KEY-----")...),
		"deflate payload tail": zipWithDeflateTail(t, []byte("-----BEGIN PRIVATE KEY-----")),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pkg.zip"), contents, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
			requireExitCode(t, err, 2)
		})
	}
}

func TestArchiveAllowlistAcceptsGNUZipMetadata(t *testing.T) {
	zipBinary, err := exec.LookPath("zip")
	if err != nil {
		t.Skip("GNU/Info-ZIP is not installed")
	}
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	if err := os.MkdirAll(filepath.Join(payload, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(payload, "pkg", "license-tool"), strings.Repeat("binary", 16<<10))
	writeTestFile(t, filepath.Join(payload, "pkg", "LICENSE"), "license")
	writeTestFile(t, filepath.Join(payload, "README.md"), "readme")
	command := exec.Command(zipBinary, "-q", "-r", filepath.Join(dir, "pkg.zip"), ".")
	command.Dir = payload
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("zip fixture: %v: %s", err, output)
	}
	stdout, stderr, err := executeTest(t, testDeps(dir), "archive", "allowlist", ".")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "check passed") || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestArchiveByteLimits(t *testing.T) {
	const payloadSize = 256 << 10
	dir := t.TempDir()
	gzipName := filepath.Join(dir, "bomb.tar.gz")
	writeTarGzip(t, gzipName, []tarEntry{{name: "README.md", contents: strings.Repeat("\x00", payloadSize)}})
	if info, err := os.Stat(gzipName); err != nil || info.Size() >= 32<<10 {
		t.Fatalf("gzip bomb fixture is not sufficiently compressed: info=%v err=%v", info, err)
	}
	limits := archiveLimits{compressed: 32 << 10, decompressed: 32 << 10}
	if _, err := readTarGzipMembersWithLimits(context.Background(), gzipName, limits); err == nil || !strings.Contains(err.Error(), "decompressed") {
		t.Fatalf("gzip decompression limit error = %v", err)
	}

	zipName := filepath.Join(dir, "bomb.zip")
	writeDeflateZip(t, zipName, "README.md", bytes.Repeat([]byte{0}, payloadSize))
	if _, err := readZipMembersWithLimits(context.Background(), zipName, limits); err == nil || !strings.Contains(err.Error(), "decompressed") {
		t.Fatalf("zip decompression limit error = %v", err)
	}

	imageName := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(imageName, tarBytes(t, []tarEntry{{name: "usr/share/zoneinfo/UTC", contents: strings.Repeat("x", payloadSize)}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageTarWithLimit(context.Background(), imageName, 32<<10); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("image tar limit error = %v", err)
	}

	info, err := os.Stat(gzipName)
	if err != nil {
		t.Fatal(err)
	}
	compressedLimits := archiveLimits{compressed: info.Size() - 1, decompressed: maxDecompressedArchiveBytes}
	if _, err := readTarGzipMembersWithLimits(context.Background(), gzipName, compressedLimits); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("compressed archive limit error = %v", err)
	}
}

func TestArchiveReadersHonorCancellation(t *testing.T) {
	dir := t.TempDir()
	tarGzipName := filepath.Join(dir, "pkg.tar.gz")
	writeTarGzip(t, tarGzipName, []tarEntry{{name: "README.md", contents: "readme"}})
	zipName := filepath.Join(dir, "pkg.zip")
	writeZip(t, zipName, []tarEntry{{name: "README.md", contents: "readme"}})
	imageName := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(imageName, tarBytes(t, []tarEntry{{name: "license-tool", contents: "binary", mode: 0o755}}), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestName := filepath.Join(dir, "manifest")
	writeTestFile(t, manifestName, "0755 - license-tool\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readers := map[string]func() error{
		"tar gzip": func() error { _, err := readTarGzipMembers(ctx, tarGzipName); return err },
		"zip":      func() error { _, err := readZipMembers(ctx, zipName); return err },
		"image tar": func() error {
			_, err := readImageTar(ctx, imageName)
			return err
		},
		"manifest": func() error { _, err := parseManifestFile(ctx, manifestName); return err },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestArchiveImagePropagatesCancellationAfterExport(t *testing.T) {
	exported := tarBytes(t, []tarEntry{{name: "license-tool", contents: "binary", mode: 0o755}})
	ctx, cancel := context.WithCancel(context.Background())
	deps := testDeps(t.TempDir())
	deps.runner = fakeRunner{
		lookPath: func(string) (string, error) { return "/docker", nil },
		run: func(_ context.Context, command command) error {
			switch command.Args[0] {
			case "create":
				_, _ = io.WriteString(command.Stdout, strings.Repeat("a", 64)+"\n")
			case "export":
				if _, err := command.Stdout.Write(exported); err != nil {
					return err
				}
				cancel()
			case "rm":
			default:
				return errors.New("unexpected docker command")
			}
			return nil
		},
	}
	var stdout, stderr bytes.Buffer
	err := execute(ctx, deps, []string{"archive", "allowlist", "--image", "example:test"}, nil, &stdout, &stderr)
	requireExitCode(t, err, 2)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestArchiveAllowlistMissingAndEmptyDirectoriesPass(t *testing.T) {
	dir := t.TempDir()
	_, stderr, err := executeTest(t, testDeps(dir), "archive", "allowlist", "missing")
	requireExitCode(t, err, 0)
	if !strings.Contains(stderr, "does not exist") {
		t.Fatalf("stderr=%q", stderr)
	}
	_, stderr, err = executeTest(t, testDeps(dir), "archive", "allowlist", ".")
	requireExitCode(t, err, 0)
	if !strings.Contains(stderr, "no *.tar.gz") {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestArchiveManifestSelftestCases(t *testing.T) {
	clean := strings.Join([]string{
		"0644 - etc/passwd", "0644 - etc/group", "0644 - etc/nsswitch.conf",
		"0644 - etc/ssl/certs/ca-certificates.crt", "0644 - etc/os-release", "0644 - usr/lib/os-release",
		"0755 - home/nonroot", "0777 - tmp", "0644 - .dockerenv", "0755 - dev/console",
		"0644 - etc/hostname", "0644 - etc/hosts", "0644 - etc/resolv.conf", "0644 - etc/debian_version",
		"0644 - etc/protocols", "0644 - etc/services", "0644 - etc/mime.types",
		"0644 - usr/share/base-files/profile", "0644 - usr/share/common-licenses/Apache-2.0",
		"0644 - usr/share/doc/base-files/copyright", "0644 - usr/share/doc/netbase/copyright",
		"0644 - usr/share/zoneinfo/Asia/Shanghai", "0644 - usr/share/zoneinfo/right/Etc/UTC",
		"0644 - usr/share/lintian/overrides/tzdata", "0644 - var/lib/dpkg/status.d/tzdata",
		"0644 - var/lib/dpkg/status.d/base-files.md5sums", "0755 - license-tool", "0644 - LICENSE",
	}, "\n") + "\n"
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "clean.manifest"), clean)
	_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", "clean.manifest")
	requireExitCode(t, err, 0)

	cases := []string{
		"0600 - usr/share/grantseal/private.key",
		"0600 - usr/share/zoneinfo/private.key",
		"0644 - etc/grantseal/config.json",
		"0644 - var/lib/grantseal/source.go",
		"0644 - app/config.json",
		"0755 - usr/bin/license-tool",
		"0644 - var/lib/dpkg/status.d/openssl",
		"0644 - usr/share/grantseal/notes.txt",
		"0666 - dev/full",
	}
	for index, planted := range cases {
		name := filepath.Join(dir, "bad-"+string(rune('a'+index))+".manifest")
		writeTestFile(t, name, clean+planted+"\n")
		_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", filepath.Base(name))
		requireExitCode(t, err, 1)
	}
	writeTestFile(t, filepath.Join(dir, "nonexec.manifest"), strings.Replace(clean, "0755 - license-tool", "0644 - license-tool", 1))
	_, _, err = executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", "nonexec.manifest")
	requireExitCode(t, err, 1)
}

func TestArchiveManifestUsesBaselineAndValidatesInput(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("a", 64)
	writeTestFile(t, filepath.Join(dir, "base"), "0644 "+digest+" opt/custom/data\n")
	writeTestFile(t, filepath.Join(dir, "same"), "0644 "+digest+" opt/custom/data\n0755 - license-tool\n")
	_, _, err := executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", "same", "base")
	requireExitCode(t, err, 0)
	writeTestFile(t, filepath.Join(dir, "changed"), "0600 "+digest+" opt/custom/data\n0755 - license-tool\n")
	_, _, err = executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", "changed", "base")
	requireExitCode(t, err, 1)
	for _, test := range []struct {
		name     string
		manifest string
	}{
		{name: "unknown-mode", manifest: "- " + digest + " opt/custom/data\n0755 - license-tool\n"},
		{name: "unknown-digest", manifest: "0644 - opt/custom/data\n0755 - license-tool\n"},
	} {
		writeTestFile(t, filepath.Join(dir, test.name), test.manifest)
		_, _, err = executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", test.name, "base")
		requireExitCode(t, err, 1)
	}
	writeTestFile(t, filepath.Join(dir, "malformed"), "this is not a manifest\n")
	_, _, err = executeTest(t, testDeps(dir), "archive", "allowlist", "--manifest", "malformed")
	requireExitCode(t, err, 2)
}

func TestBaseImageLinksStayWithinAllowlist(t *testing.T) {
	for _, test := range []struct {
		name   string
		member imageMember
		want   bool
	}{
		{
			name: "legitimate relative zoneinfo symlink",
			member: imageMember{
				Path: "usr/share/zoneinfo/US/Eastern", Linkname: "../America/New_York",
				Mode: "0777", Type: tar.TypeSymlink, TypeKnown: true,
			},
			want: true,
		},
		{
			name: "zoneinfo symlink to shadow",
			member: imageMember{
				Path: "usr/share/zoneinfo/X", Linkname: "/etc/shadow",
				Mode: "0777", Type: tar.TypeSymlink, TypeKnown: true,
			},
		},
		{
			name: "zoneinfo hardlink to unlisted path",
			member: imageMember{
				Path: "usr/share/zoneinfo/X", Linkname: "etc/shadow",
				Mode: "0644", Type: tar.TypeLink, TypeKnown: true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := imageMemberSafe(test.member) && baseImageMemberAllowed(test.member)
			if got != test.want {
				t.Fatalf("allowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestArchiveImageWithInjectedRunner(t *testing.T) {
	// Exercise the explicit missing-Docker compatibility path.
	deps := testDeps(t.TempDir())
	deps.runner = fakeRunner{lookPath: func(string) (string, error) { return "", fs.ErrNotExist }}
	_, stderr, err := executeTest(t, deps, "archive", "allowlist", "--image", "example:test")
	requireExitCode(t, err, 0)
	if !strings.Contains(stderr, "docker not available") {
		t.Fatalf("stderr=%q", stderr)
	}

	for _, test := range []struct {
		name    string
		entries []tarEntry
		code    int
	}{
		{name: "clean", entries: []tarEntry{{name: "license-tool", mode: 0o755}, {name: "LICENSE"}, {name: "etc/passwd"}}, code: 0},
		{name: "planted", entries: []tarEntry{{name: "license-tool", mode: 0o755}, {name: "app/config.json"}}, code: 1},
		{name: "non executable binary", entries: []tarEntry{{name: "license-tool", mode: 0o644}}, code: 1},
		{name: "planted key under zoneinfo", entries: []tarEntry{{name: "license-tool", mode: 0o755}, {name: "usr/share/zoneinfo/private.key", contents: "secret"}}, code: 1},
		{name: "private material under zoneinfo", entries: []tarEntry{{name: "license-tool", mode: 0o755}, {name: "usr/share/zoneinfo/Asia/Shanghai", contents: "-----BEGIN PRIVATE KEY-----\nsecret"}}, code: 1},
		{name: "special file under zoneinfo", entries: []tarEntry{{name: "license-tool", mode: 0o755}, {name: "usr/share/zoneinfo/Asia/Shanghai", typeflag: tar.TypeFifo}}, code: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			exported := tarBytes(t, test.entries)
			removed := false
			deps := testDeps(t.TempDir())
			deps.runner = fakeRunner{
				lookPath: func(string) (string, error) { return "/docker", nil },
				run: func(_ context.Context, cmd command) error {
					switch cmd.Args[0] {
					case "create":
						_, _ = io.WriteString(cmd.Stdout, strings.Repeat("a", 64)+"\n")
					case "export":
						_, _ = cmd.Stdout.Write(exported)
					case "rm":
						removed = true
					default:
						return errors.New("unexpected docker command")
					}
					return nil
				},
			}
			_, _, err := executeTest(t, deps, "archive", "allowlist", "--image", "example:test")
			requireExitCode(t, err, test.code)
			if !removed {
				t.Fatal("temporary container was not removed")
			}
		})
	}
}

func TestNormalizeArchivePath(t *testing.T) {
	for _, name := range []string{"../x", "a/../../x", "a\\x", "a\x00x", "a//x"} {
		if _, _, err := normalizeArchivePath(name, false); err == nil {
			t.Errorf("normalizeArchivePath(%q) succeeded", name)
		}
	}
	if got, _, err := normalizeArchivePath("./pkg/LICENSE", false); err != nil || got != "pkg/LICENSE" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestArchiveDispatcherUsage(t *testing.T) {
	_, _, err := executeTest(t, testDeps(t.TempDir()), "archive", "allowlist", "--image")
	requireExitCode(t, err, 2)
	_, _, err = executeTest(t, testDeps(t.TempDir()), "unknown", "thing")
	requireExitCode(t, err, 2)
	err = Execute(context.Background(), []string{"unknown", "thing"}, nil, nil, nil)
	requireExitCode(t, err, 2)
}

func gzipTestBytes(t *testing.T, contents []byte, header gzip.Header) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	writer.Header = header
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func zipTestBytes(t *testing.T, archiveComment, entryComment string, extra []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	if archiveComment != "" {
		if err := writer.SetComment(archiveComment); err != nil {
			t.Fatal(err)
		}
	}
	header := &zip.FileHeader{Name: "README.md", Method: zip.Store, Comment: entryComment, Extra: extra}
	header.SetMode(0o644)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "readme"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func writeDeflateZip(t *testing.T, name, entryName string, contents []byte) {
	t.Helper()
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: entryName, Method: zip.Deflate})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := entry.Write(contents); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func zipWithDeflateTail(t *testing.T, tail []byte) []byte {
	t.Helper()
	name := filepath.Join(t.TempDir(), "fixture.zip")
	writeDeflateZip(t, name, "README.md", []byte("readme"))
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	endOffset := len(contents) - 22
	if endOffset < 0 || binary.LittleEndian.Uint32(contents[endOffset:endOffset+4]) != 0x06054b50 {
		t.Fatal("fixture has no terminal zip end record")
	}
	centralOffset := int(binary.LittleEndian.Uint32(contents[endOffset+16 : endOffset+20]))
	if centralOffset < 16 || binary.LittleEndian.Uint32(contents[centralOffset:centralOffset+4]) != 0x02014b50 {
		t.Fatal("fixture has no central directory")
	}
	descriptorOffset := centralOffset - 16
	if binary.LittleEndian.Uint32(contents[descriptorOffset:descriptorOffset+4]) != 0x08074b50 {
		t.Fatal("fixture has no signed data descriptor")
	}
	oldCompressedSize := binary.LittleEndian.Uint32(contents[centralOffset+20 : centralOffset+24])
	mutated := make([]byte, 0, len(contents)+len(tail))
	mutated = append(mutated, contents[:descriptorOffset]...)
	mutated = append(mutated, tail...)
	mutated = append(mutated, contents[descriptorOffset:]...)
	newDescriptorOffset := descriptorOffset + len(tail)
	newCentralOffset := centralOffset + len(tail)
	newEndOffset := endOffset + len(tail)
	newCompressedSize := oldCompressedSize + uint32(len(tail))
	binary.LittleEndian.PutUint32(mutated[newDescriptorOffset+8:newDescriptorOffset+12], newCompressedSize)
	binary.LittleEndian.PutUint32(mutated[newCentralOffset+20:newCentralOffset+24], newCompressedSize)
	binary.LittleEndian.PutUint32(mutated[newEndOffset+16:newEndOffset+20], uint32(newCentralOffset))
	return mutated
}
