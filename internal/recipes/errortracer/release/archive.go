package release

import (
	"archive/tar"
	"archive/zip"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func writeTarGzip(ctx context.Context, destination, sourceDirectory string, timestamp time.Time) (returnErr error) {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()

	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = time.Time{}
	gzipWriter.Header.Name = ""
	gzipWriter.Header.Comment = ""
	gzipWriter.Header.Extra = nil
	gzipWriter.Header.OS = 3 // GNU gzip on the Linux release runner.

	tarWriter := tar.NewWriter(gzipWriter)
	paths, err := archivePaths(sourceDirectory)
	if err != nil {
		_ = tarWriter.Close()
		_ = gzipWriter.Close()
		return err
	}
	baseParent := filepath.Dir(sourceDirectory)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return fmt.Errorf("unsupported archive entry %s", path)
		}
		relativeName, err := filepath.Rel(baseParent, path)
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		header.Name = filepath.ToSlash(relativeName)
		if info.IsDir() {
			header.Name += "/"
		}
		header.ModTime = timestamp
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.Mode = int64(releaseArchiveMode(info).Perm())
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		if info.Mode().IsRegular() {
			if err := copyArchiveFile(ctx, tarWriter, path); err != nil {
				_ = tarWriter.Close()
				_ = gzipWriter.Close()
				return err
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	return nil
}

func writeZip(ctx context.Context, destination, sourceDirectory string, timestamp time.Time) (returnErr error) {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()

	zipWriter := zip.NewWriter(output)
	zipWriter.RegisterCompressor(zip.Deflate, func(writer io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(writer, flate.BestCompression)
	})
	paths, err := archivePaths(sourceDirectory)
	if err != nil {
		_ = zipWriter.Close()
		return err
	}
	baseParent := filepath.Dir(sourceDirectory)
	modifiedDate, modifiedTime := msDOSTime(timestamp)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			_ = zipWriter.Close()
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			_ = zipWriter.Close()
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			_ = zipWriter.Close()
			return fmt.Errorf("unsupported archive entry %s", path)
		}
		relativeName, err := filepath.Rel(baseParent, path)
		if err != nil {
			_ = zipWriter.Close()
			return err
		}
		header := &zip.FileHeader{
			Name:         filepath.ToSlash(relativeName),
			Method:       zip.Deflate,
			ModifiedDate: modifiedDate,
			ModifiedTime: modifiedTime,
		}
		if info.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
		}
		header.SetMode(releaseArchiveMode(info))
		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			_ = zipWriter.Close()
			return err
		}
		if info.Mode().IsRegular() {
			if err := copyArchiveFile(ctx, writer, path); err != nil {
				_ = zipWriter.Close()
				return err
			}
		}
	}
	if err := zipWriter.Close(); err != nil {
		return err
	}
	return nil
}

// releaseArchiveMode deliberately does not inherit permission bits from the
// host filesystem. Windows exposes only a small, synthetic subset of POSIX
// permissions, so doing so would make archives host-dependent and could strip
// the executable bit from release binaries. These modes also match the normal
// output of the original Linux release script without preserving accidental
// write or execute permissions from a checkout.
func releaseArchiveMode(info fs.FileInfo) fs.FileMode {
	if info.IsDir() {
		return fs.ModeDir | 0o755
	}
	if info.Name() == programName || info.Name() == programName+".exe" {
		return 0o755
	}
	return 0o644
}

func archivePaths(sourceDirectory string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(sourceDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func copyArchiveFile(ctx context.Context, destination io.Writer, source string) (returnErr error) {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	_, err = io.Copy(destination, contextReader{ctx: ctx, reader: file})
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func msDOSTime(value time.Time) (uint16, uint16) {
	value = value.UTC()
	minimum := time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
	maximum := time.Date(2107, time.December, 31, 23, 59, 58, 0, time.UTC)
	if value.Before(minimum) {
		value = minimum
	} else if value.After(maximum) {
		value = maximum
	}
	date := uint16(value.Day() + int(value.Month())<<5 + (value.Year()-1980)<<9)
	clock := uint16(value.Second()/2 + value.Minute()<<5 + value.Hour()<<11)
	return date, clock
}
