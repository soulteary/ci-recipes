package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCode(t *testing.T) {
	t.Parallel()
	var typedNil *ExitError
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "ordinary error", err: errors.New("boom"), want: 1},
		{name: "typed", err: Exit(2, "usage"), want: 2},
		{name: "wrapped", err: fmt.Errorf("wrapped: %w", Exit(3, "bad")), want: 3},
		{name: "invalid code", err: &ExitError{Code: 0, Err: errors.New("bad")}, want: 1},
		{name: "typed nil", err: typedNil, want: 1},
		{name: "wrapped typed nil", err: fmt.Errorf("wrapped: %w", typedNil), want: 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}
