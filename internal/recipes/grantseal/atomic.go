package grantseal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

func atomicWriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return &fs.PathError{Op: "atomic-write", Path: path, Err: errors.New("target is not a regular file")}
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	tmpName, err := stageAtomicFile(path, data, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		if restoreErr := replaceFile(path, original, info.Mode().Perm()); restoreErr != nil {
			return errors.Join(err, &fs.PathError{Op: "restore-after-cancel", Path: path, Err: restoreErr})
		}
		return err
	}
	return nil
}

func replaceFile(path string, data []byte, mode fs.FileMode) error {
	tmpName, err := stageAtomicFile(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmpName) // no-op after a successful rename
	return os.Rename(tmpName, path)
}

func stageAtomicFile(path string, data []byte, mode fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ci-recipes-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	staged := false
	defer func() {
		_ = tmp.Close()
		if !staged {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	staged = true
	return tmpName, nil
}
