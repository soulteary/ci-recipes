package grantseal

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxArchiveMembers           = 100_000
	maxZipArchiveMembers        = 65_534 // 65,535 is ZIP64's legacy sentinel.
	maxCompressedArchiveBytes   = int64(256 << 20)
	maxDecompressedArchiveBytes = int64(1 << 30)
	maxManifestBytes            = int64(64 << 20)
	maxImageTarBytes            = maxDecompressedArchiveBytes
)

type archiveLimits struct {
	compressed   int64
	decompressed int64
}

var defaultArchiveLimits = archiveLimits{
	compressed:   maxCompressedArchiveBytes,
	decompressed: maxDecompressedArchiveBytes,
}

type imageMember struct {
	Mode              string
	SHA256            string
	Path              string
	Linkname          string
	Type              byte
	Regular           bool
	TypeKnown         bool
	SensitiveMaterial bool
}

func runArchiveAllowlist(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "--manifest" {
		if len(args) < 2 || len(args) > 3 {
			return usage("usage: archive allowlist --manifest MANIFEST_FILE [BASE_MANIFEST_FILE]")
		}
		return checkManifestFiles(ctx, args[1], optionalArg(args, 2), deps.workDir, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "--image" {
		if len(args) != 2 || args[1] == "" {
			return usage("usage: archive allowlist --image IMAGE_REF")
		}
		return checkDockerImage(ctx, deps, args[1], stdout, stderr)
	}
	if len(args) > 1 {
		return usage("usage: archive allowlist [DIR]")
	}
	dir := "dist"
	if len(args) == 1 {
		dir = args[0]
	}
	return checkArchiveDir(ctx, resolvePath(deps.workDir, dir), dir, stdout, stderr)
}

func optionalArg(args []string, index int) string {
	if index < len(args) {
		return args[index]
	}
	return ""
}

func resolvePath(workDir, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(workDir, name)
}

func checkArchiveDir(ctx context.Context, dir, display string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return usage("inspect archive dir %q: %v", display, err)
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		fmt.Fprintf(stderr, "note: archive dir %q does not exist, skipping\n", display)
		return nil
	}
	if err != nil {
		return usage("inspect archive dir %q: %v", display, err)
	}

	archives, err := archivesAtDepth(ctx, dir, 2)
	if err != nil {
		return usage("enumerate archive dir %q: %v", display, err)
	}
	if len(archives) == 0 {
		fmt.Fprintf(stderr, "note: no *.tar.gz / *.zip archives found under %q\n", display)
		return nil
	}

	var violations []string
	for _, archive := range archives {
		members, err := readArchiveMembers(ctx, archive)
		if err != nil {
			return usage("inspect archive %q: %v", archive, err)
		}
		for _, member := range members {
			if reason := validateReleaseMember(member); reason != "" {
				violations = append(violations, fmt.Sprintf("disallowed file in %s: %s (%s)", archive, member.Name, reason))
			}
		}
	}
	if len(violations) != 0 {
		for _, violation := range violations {
			fmt.Fprintln(stderr, violation)
		}
		return rejected("release archive(s) contain files outside the allowlist")
	}
	fmt.Fprintln(stdout, "archive allowlist check passed: all archive members are allowlisted")
	return nil
}

func archivesAtDepth(ctx context.Context, root string, maxDepth int) ([]string, error) {
	var result []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if entry.IsDir() {
			if depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if depth > maxDepth || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip") {
			result = append(result, name)
		}
		return nil
	})
	sort.Strings(result)
	return result, err
}

type archiveMember struct {
	Name        string
	Mode        fs.FileMode
	Type        fs.FileMode
	Tar         bool
	TarType     byte
	HasMetadata bool
}

func readArchiveMembers(ctx context.Context, name string) ([]archiveMember, error) {
	if strings.HasSuffix(name, ".tar.gz") {
		return readTarGzipMembers(ctx, name)
	}
	if strings.HasSuffix(name, ".zip") {
		return readZipMembers(ctx, name)
	}
	return nil, fmt.Errorf("unsupported archive format")
}

func readTarGzipMembers(ctx context.Context, name string) ([]archiveMember, error) {
	return readTarGzipMembersWithLimits(ctx, name, defaultArchiveLimits)
}

func readTarGzipMembersWithLimits(ctx context.Context, name string, limits archiveLimits) ([]archiveMember, error) {
	if err := validateArchiveLimits(limits); err != nil {
		return nil, err
	}
	file, _, err := openRegularFile(ctx, name, limits.compressed, "compressed archive")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	compressed := bufio.NewReader(&boundedContextReader{
		ctx: ctx, reader: file, remaining: limits.compressed, maximum: limits.compressed, label: "compressed archive",
	})
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	gz.Multistream(false)
	if gz.Name != "" || gz.Comment != "" || len(gz.Extra) != 0 {
		return nil, fmt.Errorf("gzip header contains disallowed name, comment, or extra data")
	}

	expanded := &boundedContextReader{
		ctx: ctx, reader: gz, remaining: limits.decompressed, maximum: limits.decompressed, label: "decompressed archive",
	}
	padding := &tarPaddingReader{reader: expanded}
	reader := tar.NewReader(padding)
	result := make([]archiveMember, 0, 16)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar stops at the two zero blocks. Drain the first gzip
			// member to verify its CRC/size trailer, but permit only zero-valued
			// tar record padding after the terminator.
			if err := consumeZeroPadding(ctx, padding); err != nil {
				return nil, fmt.Errorf("verify tar terminator and gzip stream: %w", err)
			}
			// Multistream(false) leaves the compressed reader exactly after the
			// first member. Any remaining byte is a concatenated gzip member or
			// an opaque tail and is therefore hidden artifact material.
			if _, err := compressed.Peek(1); err == nil {
				return nil, fmt.Errorf("gzip archive contains trailing or concatenated data")
			} else if !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("inspect gzip trailer: %w", err)
			}
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if len(result) >= maxArchiveMembers {
			return nil, fmt.Errorf("archive contains more than %d members", maxArchiveMembers)
		}
		if err := padding.expectPayload(header.Size); err != nil {
			return nil, err
		}
		mode := header.FileInfo().Mode()
		result = append(result, archiveMember{
			Name: header.Name, Mode: mode.Perm(), Type: mode.Type(),
			Tar: true, TarType: header.Typeflag,
			HasMetadata: len(header.PAXRecords) != 0 || len(header.Xattrs) != 0,
		})
	}
}

func readZipMembers(ctx context.Context, name string) ([]archiveMember, error) {
	return readZipMembersWithLimits(ctx, name, defaultArchiveLimits)
}

func readZipMembersWithLimits(ctx context.Context, name string, limits archiveLimits) ([]archiveMember, error) {
	if err := validateArchiveLimits(limits); err != nil {
		return nil, err
	}
	file, size, err := openRegularFile(ctx, name, limits.compressed, "compressed archive")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entryCount, err := validateZipContainer(ctx, file, size)
	if err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(contextReaderAt{ctx: ctx, reader: file}, size)
	if err != nil {
		return nil, err
	}
	if reader.Comment != "" {
		return nil, fmt.Errorf("zip archive comment is not allowed")
	}
	if len(reader.File) != entryCount {
		return nil, fmt.Errorf("zip central-directory entry count mismatch")
	}
	if len(reader.File) > maxZipArchiveMembers {
		return nil, fmt.Errorf("zip archive contains more than %d members", maxZipArchiveMembers)
	}
	result := make([]archiveMember, 0, len(reader.File))
	var expanded int64
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.Comment != "" {
			return nil, fmt.Errorf("zip member %q contains a comment", file.Name)
		}
		if err := validateZipExtra(file.Extra, true); err != nil {
			return nil, fmt.Errorf("zip member %q: %w", file.Name, err)
		}
		if file.UncompressedSize64 > uint64(limits.decompressed-expanded) {
			return nil, fmt.Errorf("decompressed archive exceeds %d bytes", limits.decompressed)
		}
		expanded += int64(file.UncompressedSize64)
		if err := verifyZipMemberPayload(ctx, file); err != nil {
			return nil, fmt.Errorf("verify zip member %q: %w", file.Name, err)
		}
		mode := file.Mode()
		result = append(result, archiveMember{Name: file.Name, Mode: mode.Perm(), Type: mode.Type()})
	}
	return result, nil
}

func validateArchiveLimits(limits archiveLimits) error {
	if limits.compressed <= 0 || limits.decompressed <= 0 {
		return fmt.Errorf("archive byte limits must be positive")
	}
	return nil
}

func openRegularFile(ctx context.Context, name string, maxSize int64, label string) (*os.File, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", label)
	}
	if info.Size() > maxSize {
		return nil, 0, fmt.Errorf("%s exceeds %d bytes", label, maxSize)
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > maxSize {
		file.Close()
		return nil, 0, fmt.Errorf("%s changed or exceeds %d bytes", label, maxSize)
	}
	return file, opened.Size(), nil
}

type boundedContextReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
	maximum   int64
	label     string
}

type tarPaddingReader struct {
	reader       io.Reader
	offset       int64
	paddingStart int64
	paddingEnd   int64
	paddingSet   bool
	err          error
}

func (reader *tarPaddingReader) expectPayload(size int64) error {
	if size < 0 {
		return fmt.Errorf("tar member has a negative size")
	}
	if reader.err != nil {
		return reader.err
	}
	if reader.paddingSet && reader.offset < reader.paddingEnd {
		return fmt.Errorf("tar reader advanced before validating member padding")
	}
	padding := (512 - size%512) % 512
	reader.paddingStart = reader.offset + size
	reader.paddingEnd = reader.paddingStart + padding
	reader.paddingSet = padding != 0
	return nil
}

func (reader *tarPaddingReader) Read(buffer []byte) (int, error) {
	if reader.err != nil {
		return 0, reader.err
	}
	start := reader.offset
	count, err := reader.reader.Read(buffer)
	reader.offset += int64(count)
	if reader.paddingSet {
		overlapStart := maxInt64(start, reader.paddingStart)
		overlapEnd := minInt64(reader.offset, reader.paddingEnd)
		for position := overlapStart; position < overlapEnd; position++ {
			if buffer[position-start] != 0 {
				reader.err = fmt.Errorf("tar member contains non-zero alignment padding")
				return count, reader.err
			}
		}
		if reader.offset >= reader.paddingEnd {
			reader.paddingSet = false
		}
	}
	return count, err
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (reader *boundedContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if reader.remaining == 0 {
		var probe [1]byte
		count, err := reader.reader.Read(probe[:])
		if count != 0 {
			return 0, fmt.Errorf("%s exceeds %d bytes", reader.label, reader.maximum)
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	if err == nil {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return count, contextErr
		}
	}
	return count, err
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (reader contextReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.ReadAt(buffer, offset)
	if err == nil {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return count, contextErr
		}
	}
	return count, err
}

func consumeZeroPadding(ctx context.Context, reader io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := reader.Read(buffer)
		if bytes.IndexFunc(buffer[:count], func(r rune) bool { return r != 0 }) >= 0 {
			return fmt.Errorf("tar archive contains non-zero data after its terminator")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func verifyZipMemberPayload(ctx context.Context, file *zip.File) error {
	raw, err := file.OpenRaw()
	if err != nil {
		return err
	}
	compressed := &contextByteReader{ctx: ctx, reader: raw}
	var contents io.ReadCloser
	switch file.Method {
	case zip.Store:
		contents = io.NopCloser(compressed)
	case zip.Deflate:
		contents = flate.NewReader(compressed)
	default:
		return fmt.Errorf("unsupported compression method %d", file.Method)
	}
	checksum := crc32.NewIEEE()
	count, readErr := copyWithContext(ctx, checksum, contents)
	closeErr := contents.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if uint64(count) != file.UncompressedSize64 {
		return fmt.Errorf("member size is %d bytes, expected %d", count, file.UncompressedSize64)
	}
	if compressed.count != file.CompressedSize64 {
		return fmt.Errorf("compressed payload contains %d unconsumed bytes", file.CompressedSize64-compressed.count)
	}
	if checksum.Sum32() != file.CRC32 {
		return fmt.Errorf("checksum mismatch")
	}
	return nil
}

type contextByteReader struct {
	ctx    context.Context
	reader io.Reader
	count  uint64
}

func (reader *contextByteReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.Read(buffer)
	reader.count += uint64(count)
	if err == nil {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return count, contextErr
		}
	}
	return count, err
}

func (reader *contextByteReader) ReadByte() (byte, error) {
	var buffer [1]byte
	count, err := reader.Read(buffer[:])
	if count == 1 {
		return buffer[0], err
	}
	return 0, err
}

type strictZipEntry struct {
	name             []byte
	flags            uint16
	method           uint16
	crc              uint32
	compressedSize   uint32
	uncompressedSize uint32
	localOffset      uint32
}

// validateZipContainer rejects every byte that is not an entry payload or
// required ZIP framing. ZIP comments, extra fields, prepended stubs, gaps, and
// tails can otherwise carry material invisible to archive/zip's file listing.
func validateZipContainer(ctx context.Context, file *os.File, size int64) (int, error) {
	const endSize = int64(22)
	if size < endSize {
		return 0, fmt.Errorf("zip archive is too short")
	}
	end := make([]byte, endSize)
	if err := readAtContext(ctx, file, end, size-endSize); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(end[0:4]) != 0x06054b50 {
		return 0, fmt.Errorf("zip end record is not at the physical end of file")
	}
	if binary.LittleEndian.Uint16(end[20:22]) != 0 {
		return 0, fmt.Errorf("zip archive comment is not allowed")
	}
	if binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 {
		return 0, fmt.Errorf("multi-disk zip archives are not allowed")
	}
	entriesOnDisk := binary.LittleEndian.Uint16(end[8:10])
	entryCount := binary.LittleEndian.Uint16(end[10:12])
	centralSize := binary.LittleEndian.Uint32(end[12:16])
	centralOffset := binary.LittleEndian.Uint32(end[16:20])
	if entriesOnDisk != entryCount {
		return 0, fmt.Errorf("zip central-directory entry count mismatch")
	}
	if entryCount == ^uint16(0) || centralSize == ^uint32(0) || centralOffset == ^uint32(0) {
		return 0, fmt.Errorf("zip64 archives are not supported by this bounded checker")
	}
	if int(entryCount) > maxZipArchiveMembers {
		return 0, fmt.Errorf("zip archive contains more than %d members", maxZipArchiveMembers)
	}
	centralEnd := int64(centralOffset) + int64(centralSize)
	if centralEnd != size-endSize {
		return 0, fmt.Errorf("zip central directory is not contiguous with the physical EOF")
	}

	entries := make([]strictZipEntry, 0, entryCount)
	cursor := int64(centralOffset)
	for index := 0; index < int(entryCount); index++ {
		fixed := make([]byte, 46)
		if err := readAtContext(ctx, file, fixed, cursor); err != nil {
			return 0, fmt.Errorf("read zip central entry %d: %w", index+1, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != 0x02014b50 {
			return 0, fmt.Errorf("invalid zip central entry %d", index+1)
		}
		nameLength := int64(binary.LittleEndian.Uint16(fixed[28:30]))
		extraLength := int64(binary.LittleEndian.Uint16(fixed[30:32]))
		commentLength := int64(binary.LittleEndian.Uint16(fixed[32:34]))
		if commentLength != 0 {
			return 0, fmt.Errorf("zip central entry %d contains a comment", index+1)
		}
		if binary.LittleEndian.Uint16(fixed[34:36]) != 0 {
			return 0, fmt.Errorf("zip central entry %d refers to another disk", index+1)
		}
		entry := strictZipEntry{
			flags:            binary.LittleEndian.Uint16(fixed[8:10]),
			method:           binary.LittleEndian.Uint16(fixed[10:12]),
			crc:              binary.LittleEndian.Uint32(fixed[16:20]),
			compressedSize:   binary.LittleEndian.Uint32(fixed[20:24]),
			uncompressedSize: binary.LittleEndian.Uint32(fixed[24:28]),
			localOffset:      binary.LittleEndian.Uint32(fixed[42:46]),
		}
		if entry.compressedSize == ^uint32(0) || entry.uncompressedSize == ^uint32(0) || entry.localOffset == ^uint32(0) {
			return 0, fmt.Errorf("zip64 entry %d is not supported by this bounded checker", index+1)
		}
		if entry.flags&0x0001 != 0 {
			return 0, fmt.Errorf("encrypted zip member %d is not allowed", index+1)
		}
		entry.name = make([]byte, nameLength)
		if err := readAtContext(ctx, file, entry.name, cursor+46); err != nil {
			return 0, fmt.Errorf("read zip central name %d: %w", index+1, err)
		}
		extra := make([]byte, extraLength)
		if err := readAtContext(ctx, file, extra, cursor+46+nameLength); err != nil {
			return 0, fmt.Errorf("read zip central extra data %d: %w", index+1, err)
		}
		if err := validateZipExtra(extra, true); err != nil {
			return 0, fmt.Errorf("zip central entry %d: %w", index+1, err)
		}
		entries = append(entries, entry)
		cursor += 46 + nameLength + extraLength
	}
	if cursor != centralEnd {
		return 0, fmt.Errorf("zip central directory contains unparsed data")
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].localOffset < entries[j].localOffset })
	cursor = 0
	for index, entry := range entries {
		if int64(entry.localOffset) != cursor {
			return 0, fmt.Errorf("zip local entry %d is preceded by hidden or overlapping data", index+1)
		}
		fixed := make([]byte, 30)
		if err := readAtContext(ctx, file, fixed, cursor); err != nil {
			return 0, fmt.Errorf("read zip local entry %d: %w", index+1, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != 0x04034b50 {
			return 0, fmt.Errorf("invalid zip local entry %d", index+1)
		}
		if binary.LittleEndian.Uint16(fixed[6:8]) != entry.flags || binary.LittleEndian.Uint16(fixed[8:10]) != entry.method {
			return 0, fmt.Errorf("zip local and central metadata differ for entry %d", index+1)
		}
		nameLength := int64(binary.LittleEndian.Uint16(fixed[26:28]))
		extraLength := int64(binary.LittleEndian.Uint16(fixed[28:30]))
		name := make([]byte, nameLength)
		if err := readAtContext(ctx, file, name, cursor+30); err != nil {
			return 0, fmt.Errorf("read zip local name %d: %w", index+1, err)
		}
		if !bytes.Equal(name, entry.name) {
			return 0, fmt.Errorf("zip local and central names differ for entry %d", index+1)
		}
		extra := make([]byte, extraLength)
		if err := readAtContext(ctx, file, extra, cursor+30+nameLength); err != nil {
			return 0, fmt.Errorf("read zip local extra data %d: %w", index+1, err)
		}
		if err := validateZipExtra(extra, false); err != nil {
			return 0, fmt.Errorf("zip local entry %d: %w", index+1, err)
		}
		if entry.flags&0x0008 == 0 {
			if binary.LittleEndian.Uint32(fixed[14:18]) != entry.crc ||
				binary.LittleEndian.Uint32(fixed[18:22]) != entry.compressedSize ||
				binary.LittleEndian.Uint32(fixed[22:26]) != entry.uncompressedSize {
				return 0, fmt.Errorf("zip local and central sizes differ for entry %d", index+1)
			}
		}
		cursor += 30 + nameLength + extraLength + int64(entry.compressedSize)
		if cursor > int64(centralOffset) {
			return 0, fmt.Errorf("zip member %d overlaps the central directory", index+1)
		}
		if entry.flags&0x0008 != 0 {
			length, err := validateZipDataDescriptor(ctx, file, cursor, entry)
			if err != nil {
				return 0, fmt.Errorf("zip data descriptor %d: %w", index+1, err)
			}
			cursor += length
		}
	}
	if cursor != int64(centralOffset) {
		return 0, fmt.Errorf("zip contains hidden data before its central directory")
	}
	return int(entryCount), nil
}

// validateZipExtra permits only the bounded numeric metadata emitted by
// Info-ZIP/GNU zip for ordinary Unix files. Unknown fields are opaque storage
// and therefore rejected. Central-directory timestamp records commonly carry
// only mtime even when their flag byte also describes local atime/ctime.
func validateZipExtra(extra []byte, central bool) error {
	if len(extra) > 128 {
		return fmt.Errorf("zip extra metadata exceeds 128 bytes")
	}
	if containsSensitiveMaterial(extra) {
		return fmt.Errorf("zip extra metadata contains sensitive material")
	}
	seen := make(map[uint16]struct{})
	for len(extra) != 0 {
		if len(extra) < 4 {
			return fmt.Errorf("zip extra metadata is truncated")
		}
		fieldID := binary.LittleEndian.Uint16(extra[0:2])
		length := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]
		if length > len(extra) {
			return fmt.Errorf("zip extra field 0x%04x is truncated", fieldID)
		}
		data := extra[:length]
		extra = extra[length:]
		if _, duplicate := seen[fieldID]; duplicate {
			return fmt.Errorf("duplicate zip extra field 0x%04x", fieldID)
		}
		seen[fieldID] = struct{}{}
		switch fieldID {
		case 0x5455: // Extended Timestamp (UT).
			if len(data) < 1 || data[0]&^byte(0x07) != 0 {
				return fmt.Errorf("invalid extended-timestamp zip extra field")
			}
			timestamps := 0
			for flags := data[0]; flags != 0; flags >>= 1 {
				timestamps += int(flags & 1)
			}
			expected := 1 + 4*timestamps
			if central && data[0]&1 != 0 && len(data) == 5 {
				break
			}
			if len(data) != expected {
				return fmt.Errorf("invalid extended-timestamp zip extra field length")
			}
		case 0x7875: // Info-ZIP New Unix UID/GID.
			if len(data) < 5 || data[0] != 1 {
				return fmt.Errorf("invalid Unix UID/GID zip extra field")
			}
			uidLength := int(data[1])
			if uidLength < 1 || uidLength > 8 || 2+uidLength >= len(data) {
				return fmt.Errorf("invalid Unix UID zip extra field")
			}
			gidLengthOffset := 2 + uidLength
			gidLength := int(data[gidLengthOffset])
			if gidLength < 1 || gidLength > 8 || gidLengthOffset+1+gidLength != len(data) {
				return fmt.Errorf("invalid Unix GID zip extra field")
			}
		default:
			return fmt.Errorf("unknown zip extra field 0x%04x", fieldID)
		}
	}
	return nil
}

func validateZipDataDescriptor(ctx context.Context, file *os.File, offset int64, entry strictZipEntry) (int64, error) {
	first := make([]byte, 4)
	if err := readAtContext(ctx, file, first, offset); err != nil {
		return 0, err
	}
	length := int64(12)
	data := make([]byte, length)
	if binary.LittleEndian.Uint32(first) == 0x08074b50 {
		length = 16
		data = make([]byte, length)
	}
	if err := readAtContext(ctx, file, data, offset); err != nil {
		return 0, err
	}
	start := 0
	if length == 16 {
		start = 4
	}
	if binary.LittleEndian.Uint32(data[start:start+4]) != entry.crc ||
		binary.LittleEndian.Uint32(data[start+4:start+8]) != entry.compressedSize ||
		binary.LittleEndian.Uint32(data[start+8:start+12]) != entry.uncompressedSize {
		return 0, fmt.Errorf("values do not match the central directory")
	}
	return length, nil
}

func readAtContext(ctx context.Context, reader io.ReaderAt, buffer []byte, offset int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	count, err := reader.ReadAt(buffer, offset)
	if err != nil && !(errors.Is(err, io.EOF) && count == len(buffer)) {
		return err
	}
	if count != len(buffer) {
		return io.ErrUnexpectedEOF
	}
	return ctx.Err()
}

func validateReleaseMember(member archiveMember) string {
	normalized, directory, err := normalizeArchivePath(member.Name, false)
	if err != nil {
		return err.Error()
	}
	if member.Tar {
		if member.HasMetadata {
			return "tar member contains extended metadata"
		}
		switch member.TarType {
		case tar.TypeDir:
			if !member.Type.IsDir() {
				return "tar directory has a non-directory mode"
			}
			return ""
		case tar.TypeReg, tar.TypeRegA:
			if directory || member.Type != 0 {
				return "regular tar member has a non-regular mode or directory name"
			}
		default:
			return "non-regular tar member"
		}
	}
	if directory {
		if !member.Type.IsDir() {
			return "directory name has a non-directory type"
		}
		return ""
	}
	if member.Type.IsDir() {
		return ""
	}
	if member.Type != 0 {
		return "non-regular archive member"
	}
	base := path.Base(normalized)
	if base == "license-tool" || base == "license-tool.exe" || base == "LICENSE" || base == "README" ||
		strings.HasPrefix(base, "LICENSE.") || strings.HasPrefix(base, "README.") {
		return ""
	}
	return "name is outside the release allowlist"
}

func normalizeArchivePath(name string, permitLeadingSlash bool) (normalized string, directory bool, err error) {
	if name == "" {
		return "", false, fmt.Errorf("empty member path")
	}
	for _, r := range name {
		if r == '\\' || r == ':' || unicode.IsControl(r) {
			return "", false, fmt.Errorf("member path contains a backslash, colon, or control character")
		}
	}
	if !utf8.ValidString(name) {
		return "", false, fmt.Errorf("member path is not valid UTF-8")
	}
	directory = strings.HasSuffix(name, "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if permitLeadingSlash {
		name = strings.TrimPrefix(name, "/")
		if strings.HasPrefix(name, "/") {
			return "", false, fmt.Errorf("member path has multiple leading slashes")
		}
	} else if strings.HasPrefix(name, "/") {
		return "", false, fmt.Errorf("absolute member path")
	}
	if name == "" {
		if directory {
			return "", true, nil
		}
		return "", false, fmt.Errorf("empty member path")
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned == "." && !directory {
		return "", false, fmt.Errorf("member path escapes the archive root")
	}
	if cleaned != strings.TrimSuffix(name, "/") {
		return "", false, fmt.Errorf("member path is not canonical")
	}
	if len(cleaned) > 4096 {
		return "", false, fmt.Errorf("member path is too long")
	}
	return cleaned, directory, nil
}

func checkManifestFiles(ctx context.Context, manifestName, baselineName, workDir string, stdout, stderr io.Writer) error {
	manifestPath := resolvePath(workDir, manifestName)
	members, err := parseManifestFile(ctx, manifestPath)
	if err != nil {
		return usage("read image manifest %q: %v", manifestName, err)
	}
	var baseline map[string]imageMember
	if baselineName != "" {
		baselineMembers, err := parseManifestFile(ctx, resolvePath(workDir, baselineName))
		if err != nil {
			return usage("read base manifest %q: %v", baselineName, err)
		}
		baseline = make(map[string]imageMember, len(baselineMembers))
		for _, member := range baselineMembers {
			if _, exists := baseline[member.Path]; exists {
				return usage("base manifest %q contains duplicate path %q", baselineName, member.Path)
			}
			baseline[member.Path] = member
		}
	}
	if err := checkImageMembers(manifestName, members, baseline, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "image manifest allowlist check passed: %s contains only allowlisted files\n", manifestName)
	return nil
}

func parseManifestFile(ctx context.Context, name string) ([]imageMember, error) {
	data, err := readBoundedFileContext(ctx, name, maxManifestBytes, "manifest")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > maxArchiveMembers+1 {
		return nil, fmt.Errorf("manifest contains more than %d records", maxArchiveMembers)
	}
	result := make([]imageMember, 0, len(lines))
	seen := make(map[string]struct{})
	for index, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if line == "" {
			continue
		}
		first := strings.IndexByte(line, ' ')
		if first <= 0 {
			return nil, fmt.Errorf("line %d: expected MODE SHA256 PATH", index+1)
		}
		rest := line[first+1:]
		second := strings.IndexByte(rest, ' ')
		if second <= 0 || second == len(rest)-1 {
			return nil, fmt.Errorf("line %d: expected MODE SHA256 PATH", index+1)
		}
		mode, digest, memberPath := line[:first], rest[:second], rest[second+1:]
		if mode != "-" {
			value, err := strconv.ParseUint(mode, 8, 32)
			if err != nil || value > 0o7777 {
				return nil, fmt.Errorf("line %d: invalid mode %q", index+1, mode)
			}
			mode = fmt.Sprintf("%04o", value)
		}
		if digest != "-" {
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != sha256.Size {
				return nil, fmt.Errorf("line %d: invalid sha256 %q", index+1, digest)
			}
			digest = strings.ToLower(digest)
		}
		normalized, directory, err := normalizeArchivePath(memberPath, true)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", index+1, err)
		}
		if directory || normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			return nil, fmt.Errorf("line %d: duplicate path %q", index+1, normalized)
		}
		seen[normalized] = struct{}{}
		result = append(result, imageMember{Mode: mode, SHA256: digest, Path: normalized})
	}
	return result, nil
}

func readBoundedFileContext(ctx context.Context, name string, maxSize int64, label string) ([]byte, error) {
	file, _, err := openRegularFile(ctx, name, maxSize, label)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(&boundedContextReader{ctx: ctx, reader: file, remaining: maxSize, maximum: maxSize, label: label})
}

func checkDockerImage(ctx context.Context, deps dependencies, image string, stdout, stderr io.Writer) error {
	for _, r := range image {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return usage("image reference contains whitespace or a control character")
		}
	}
	if _, err := deps.runner.LookPath("docker"); err != nil {
		fmt.Fprintf(stderr, "note: docker not available, skipping image allowlist for %s\n", image)
		return nil
	}
	cidBytes, err := runOutput(ctx, deps, "docker", "create", "--", image)
	if err != nil {
		return usage("create container for image %q: %v", image, err)
	}
	cid := strings.TrimSpace(string(cidBytes))
	if !dockerContainerID.MatchString(cid) {
		return usage("docker create returned an invalid container id")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.runner.Run(cleanupCtx, command{Name: "docker", Args: []string{"rm", "-f", cid}, Dir: deps.workDir, Stdout: io.Discard, Stderr: io.Discard})
	}()

	tmp, err := os.CreateTemp("", "ci-recipes-image-*.tar")
	if err != nil {
		return usage("create image export file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	exportWriter := &boundedContextWriter{ctx: ctx, writer: tmp, remaining: maxImageTarBytes, maximum: maxImageTarBytes, label: "exported image tar"}
	exportErr := deps.runner.Run(ctx, command{Name: "docker", Args: []string{"export", cid}, Dir: deps.workDir, Stdout: exportWriter, Stderr: stderr})
	closeErr := tmp.Close()
	if exportErr != nil {
		return usage("export image %q: %v", image, exportErr)
	}
	if exportWriter.err != nil {
		return usage("export image %q: %v", image, exportWriter.err)
	}
	if closeErr != nil {
		return usage("close image export: %v", closeErr)
	}

	members, err := readImageTar(ctx, tmpName)
	if err != nil {
		return usage("inspect exported image %q: %v", image, err)
	}
	if err := checkImageMembers(image, members, nil, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "image allowlist check passed: %s contains only allowlisted files\n", image)
	return nil
}

func readImageTar(ctx context.Context, name string) ([]imageMember, error) {
	return readImageTarWithLimit(ctx, name, maxImageTarBytes)
}

func readImageTarWithLimit(ctx context.Context, name string, maxBytes int64) ([]imageMember, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("image tar byte limit must be positive")
	}
	file, _, err := openRegularFile(ctx, name, maxBytes, "exported image tar")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	bounded := &boundedContextReader{ctx: ctx, reader: file, remaining: maxBytes, maximum: maxBytes, label: "exported image tar"}
	padding := &tarPaddingReader{reader: bounded}
	reader := tar.NewReader(padding)
	result := make([]imageMember, 0, 256)
	seen := make(map[string]struct{})
	memberCount := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if err := consumeZeroPadding(ctx, padding); err != nil {
				return nil, fmt.Errorf("verify image tar terminator: %w", err)
			}
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if memberCount >= maxArchiveMembers {
			return nil, fmt.Errorf("image contains more than %d members", maxArchiveMembers)
		}
		memberCount++
		if err := padding.expectPayload(header.Size); err != nil {
			return nil, err
		}
		if len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
			return nil, fmt.Errorf("image member %q contains extended metadata", header.Name)
		}
		if header.FileInfo().IsDir() && (header.Name == "." || header.Name == "./" || header.Name == "/") {
			continue
		}
		normalized, directory, err := normalizeArchivePath(header.Name, true)
		if err != nil {
			return nil, err
		}
		if directory || header.FileInfo().IsDir() || normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			return nil, fmt.Errorf("image contains duplicate path %q", normalized)
		}
		seen[normalized] = struct{}{}
		member := imageMember{
			Mode:      fmt.Sprintf("%04o", header.Mode&0o7777),
			SHA256:    "-",
			Path:      normalized,
			Linkname:  header.Linkname,
			Type:      header.Typeflag,
			Regular:   header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA,
			TypeKnown: true,
		}
		if member.Regular {
			hash := sha256.New()
			detector := &sensitiveMaterialWriter{}
			count, err := copyWithContext(ctx, io.MultiWriter(hash, detector), reader)
			if err != nil {
				return nil, err
			}
			if count != header.Size {
				return nil, fmt.Errorf("image member %q size is %d bytes, expected %d", header.Name, count, header.Size)
			}
			member.SHA256 = hex.EncodeToString(hash.Sum(nil))
			member.SensitiveMaterial = detector.found
		}
		result = append(result, member)
	}
}

type boundedContextWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int64
	maximum   int64
	label     string
	err       error
}

func (writer *boundedContextWriter) Write(buffer []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	if err := writer.ctx.Err(); err != nil {
		writer.err = err
		return 0, err
	}
	if int64(len(buffer)) > writer.remaining {
		writer.err = fmt.Errorf("%s exceeds %d bytes", writer.label, writer.maximum)
		return 0, writer.err
	}
	count, err := writer.writer.Write(buffer)
	writer.remaining -= int64(count)
	if err != nil {
		writer.err = err
		return count, err
	}
	if count != len(buffer) {
		writer.err = io.ErrShortWrite
		return count, writer.err
	}
	return count, nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count != 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

var privateMaterialMarkers = [][]byte{
	[]byte("-----BEGIN PRIVATE KEY-----"),
	[]byte("-----BEGIN ENCRYPTED PRIVATE KEY-----"),
	[]byte("-----BEGIN RSA PRIVATE KEY-----"),
	[]byte("-----BEGIN EC PRIVATE KEY-----"),
	[]byte("-----BEGIN DSA PRIVATE KEY-----"),
	[]byte("-----BEGIN OPENSSH PRIVATE KEY-----"),
}

type sensitiveMaterialWriter struct {
	tail  []byte
	found bool
}

func (writer *sensitiveMaterialWriter) Write(buffer []byte) (int, error) {
	if writer.found {
		return len(buffer), nil
	}
	combined := make([]byte, 0, len(writer.tail)+len(buffer))
	combined = append(combined, writer.tail...)
	combined = append(combined, buffer...)
	writer.found = containsSensitiveMaterial(combined)
	maxMarker := 0
	for _, marker := range privateMaterialMarkers {
		if len(marker) > maxMarker {
			maxMarker = len(marker)
		}
	}
	keep := maxMarker - 1
	if keep > len(combined) {
		keep = len(combined)
	}
	writer.tail = append(writer.tail[:0], combined[len(combined)-keep:]...)
	return len(buffer), nil
}

func containsSensitiveMaterial(contents []byte) bool {
	upper := bytes.ToUpper(contents)
	for _, marker := range privateMaterialMarkers {
		if bytes.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func checkImageMembers(label string, members []imageMember, baseline map[string]imageMember, stderr io.Writer) error {
	violations := 0
	for _, member := range members {
		allowed := imageMemberSafe(member) && appImagePathAllowed(member)
		if !allowed && baseline != nil {
			base, ok := baseline[member.Path]
			allowed = imageMemberSafe(member) && ok && compatibleBaselineMember(member, base)
		}
		if !allowed && baseline == nil {
			allowed = imageMemberSafe(member) && baseImageMemberAllowed(member)
		}
		if !allowed {
			fmt.Fprintf(stderr, "disallowed file in image %s: /%s (mode=%s sha256=%s)\n", label, member.Path, member.Mode, member.SHA256)
			violations++
		}
	}
	if violations != 0 {
		return rejected("image %s contains files outside the allowlist", label)
	}
	return nil
}

func compatibleBaselineMember(got, want imageMember) bool {
	if want.Mode != "-" && got.Mode != want.Mode {
		return false
	}
	if want.SHA256 != "-" && got.SHA256 != want.SHA256 {
		return false
	}
	return true
}

func imageMemberSafe(member imageMember) bool {
	if member.SensitiveMaterial || sensitiveImagePath(member.Path) {
		return false
	}
	if !member.TypeKnown {
		return true
	}
	if member.Regular {
		return true
	}
	switch member.Type {
	case tar.TypeSymlink, tar.TypeLink:
		return safeImageLink(member)
	case tar.TypeChar:
		return member.Path == "dev/console"
	default:
		return false
	}
}

func safeImageLink(member imageMember) bool {
	_, ok := normalizedImageLinkTarget(member)
	return ok
}

func normalizedImageLinkTarget(member imageMember) (string, bool) {
	link := member.Linkname
	if link == "" || len(link) > 4096 || strings.HasPrefix(link, "//") || strings.Contains(link, "//") || path.Clean(link) != link {
		return "", false
	}
	for _, r := range link {
		if r == '\\' || r == ':' || unicode.IsControl(r) {
			return "", false
		}
	}
	var target string
	if member.Type == tar.TypeLink {
		target = strings.TrimPrefix(link, "./")
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(target, "/")
		}
	} else if strings.HasPrefix(link, "/") {
		target = strings.TrimPrefix(link, "/")
	} else {
		target = path.Join(path.Dir(member.Path), link)
	}
	if target == "" || target == ".." || strings.HasPrefix(target, "../") || path.Clean(target) != target {
		return "", false
	}
	if sensitiveImagePath(target) {
		return "", false
	}
	return target, true
}

var sensitiveImageBasenames = map[string]struct{}{
	".env": {}, ".netrc": {}, "authorized_keys": {}, "config.json": {}, "credentials": {}, "credentials.json": {},
	"id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {}, "id_rsa": {}, "known_hosts": {},
	"private": {}, "private.key": {}, "secret": {}, "secrets": {}, "shadow": {}, "shadow-": {}, "source.go": {},
}

var sensitiveImageExtensions = []string{
	".key", ".pem", ".p12", ".pfx", ".jks", ".keystore",
	".go", ".rs", ".c", ".cc", ".cpp", ".h", ".py", ".rb", ".js", ".ts", ".sh",
	".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".config", ".sqlite", ".db",
}

func sensitiveImagePath(name string) bool {
	lowerName := strings.ToLower(name)
	if _, explicitlyVetted := grantsealBaseImageExactPaths[lowerName]; explicitlyVetted {
		return false
	}
	segments := strings.Split(lowerName, "/")
	for _, segment := range segments {
		if segment == ".git" || segment == ".ssh" || segment == ".aws" || segment == ".gnupg" || segment == ".kube" || segment == "secrets" {
			return true
		}
	}
	base := segments[len(segments)-1]
	if _, found := sensitiveImageBasenames[base]; found || strings.HasPrefix(base, ".env.") || strings.HasPrefix(base, "private.") || strings.HasPrefix(base, "secret.") {
		return true
	}
	for _, extension := range sensitiveImageExtensions {
		if strings.HasSuffix(base, extension) {
			return true
		}
	}
	return false
}

func appImagePathAllowed(member imageMember) bool {
	switch {
	case member.Path == "license-tool":
		if member.Regular || member.Type == 0 {
			if member.Mode == "-" {
				return true
			}
			mode, err := strconv.ParseUint(member.Mode, 8, 32)
			return err == nil && mode&0o111 != 0
		}
		return false
	case member.Path == "LICENSE", strings.HasPrefix(member.Path, "LICENSE."), member.Path == "README", strings.HasPrefix(member.Path, "README."):
		return member.Regular || member.Type == 0
	default:
		return false
	}
}

func baseImageMemberAllowed(member imageMember) bool {
	if !baseImagePathAllowed(member.Path) {
		return false
	}
	if !member.TypeKnown {
		return true
	}
	if member.Path == "dev/console" {
		return member.Type == tar.TypeChar
	}
	if member.Regular {
		_, mustBeLink := grantsealBaseImageLinkPaths[member.Path]
		return !mustBeLink
	}
	if member.Type == tar.TypeSymlink || member.Type == tar.TypeLink {
		if _, exactLink := grantsealBaseImageLinkPaths[member.Path]; exactLink {
			return safeImageLink(member)
		}
		for _, prefix := range grantsealBaseImagePrefixes {
			if strings.HasPrefix(member.Path, prefix) && len(member.Path) > len(prefix) {
				target, safe := normalizedImageLinkTarget(member)
				return safe && baseImagePathAllowed(target)
			}
		}
	}
	return false
}

var grantsealBaseImageLinkPaths = map[string]struct{}{
	"bin": {}, "sbin": {}, "lib": {}, "lib64": {}, "var/run": {}, "etc/mtab": {}, "etc/os-release": {},
}

var grantsealBaseImageExactPaths = map[string]struct{}{
	"": {}, ".dockerenv": {}, "dev/console": {}, "dev/pts": {}, "dev/shm": {},
	"etc/hostname": {}, "etc/hosts": {}, "etc/resolv.conf": {}, "etc/mtab": {},
	"bin": {}, "sbin": {}, "lib": {}, "lib64": {}, "var/run": {},
	"etc/passwd": {}, "etc/group": {}, "etc/nsswitch.conf": {}, "etc/os-release": {}, "usr/lib/os-release": {},
	"home/nonroot": {}, "tmp": {}, "etc/ssl/certs/ca-certificates.crt": {},
	"var/lib/dpkg/status.d/ca-certificates": {}, "var/lib/dpkg/status.d/ca-certificates.md5sums": {},
	"etc/debian_version": {}, "etc/host.conf": {}, "etc/issue": {}, "etc/issue.net": {}, "etc/dpkg/origins/debian": {},
	"usr/share/lintian/overrides/base-files": {}, "var/lib/dpkg/status.d/base-files": {}, "var/lib/dpkg/status.d/base-files.md5sums": {},
	"etc/ethertypes": {}, "etc/protocols": {}, "etc/rpc": {}, "etc/services": {},
	"var/lib/dpkg/status.d/netbase": {}, "var/lib/dpkg/status.d/netbase.md5sums": {},
	"etc/mime.types": {}, "var/lib/dpkg/status.d/media-types": {}, "var/lib/dpkg/status.d/media-types.md5sums": {},
	"usr/share/lintian/overrides/tzdata": {}, "var/lib/dpkg/status.d/tzdata": {}, "var/lib/dpkg/status.d/tzdata.md5sums": {},
	"var/lib/dpkg/status.d/tzdata-legacy": {}, "var/lib/dpkg/status.d/tzdata-legacy.md5sums": {},
}

var grantsealBaseImagePrefixes = []string{
	"etc/update-motd.d/", "usr/share/doc/ca-certificates/", "usr/share/base-files/", "usr/share/common-licenses/",
	"usr/share/doc/base-files/", "usr/share/doc/netbase/", "usr/share/bug/media-types/", "usr/share/doc/media-types/",
	"usr/share/zoneinfo/", "usr/share/doc/tzdata/", "usr/share/doc/tzdata-legacy/",
}

var dockerContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

func baseImagePathAllowed(name string) bool {
	if _, ok := grantsealBaseImageExactPaths[name]; ok {
		return true
	}
	for _, prefix := range grantsealBaseImagePrefixes {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}
