// Package cli contains the small, dependency-free command contract shared by recipes.
package cli

import (
	"errors"
	"fmt"
)

// ExitError carries a stable process exit code without coupling recipe logic to os.Exit.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Exit returns an error that asks the top-level CLI to terminate with code.
func Exit(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// ExitCode maps an error to a process exit code. A nil error maps to success.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) && exitErr != nil && exitErr.Code > 0 {
		return exitErr.Code
	}
	return 1
}
