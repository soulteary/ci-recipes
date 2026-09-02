package checks

import "testing"

func TestUnsafeHTPasswdBatchInvocations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "first option", text: `htpasswd -bn "" password`, want: 1},
		{name: "reordered option", text: `htpasswd -C 10 -bn "" password`, want: 1},
		{name: "path qualified", text: `/usr/bin/htpasswd -C 10 "-nb" "" password`, want: 1},
		{name: "line continuation", text: "htpasswd -C 10 \\\n  -bn \"\" password", want: 1},
		{name: "markdown prompt", text: "$ htpasswd -C 10 -bn \"\" password", want: 1},
		{name: "uppercase bcrypt cost", text: `htpasswd -nBC 10 stargate`},
		{name: "later command", text: `htpasswd -nBC 10 stargate; printf '%s\\n' -b`},
		{name: "pipeline boundary", text: `htpasswd -nBC 10 stargate | sed -n -b`},
		{name: "shell comment", text: `htpasswd -nBC 10 stargate # never add -b`},
		{name: "prose", text: `Do not invoke htpasswd with -b because it exposes the password.`},
		{name: "long option", text: `htpasswd --batch stargate`},
		{name: "after double dash", text: `htpasswd -- -bn`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := len(unsafeHTPasswdBatchInvocations(test.text)); got != test.want {
				t.Fatalf("violations=%d, want %d", got, test.want)
			}
		})
	}
}
