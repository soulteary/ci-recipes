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
		{name: "root prompt", text: "# htpasswd -C 10 -bn \"\" password", want: 1},
		{name: "blockquote root prompt", text: "> # htpasswd -C 10 -bn \"\" password", want: 1},
		{name: "sudo wrapper", text: `sudo htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo wrapper option", text: `sudo -u root htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo attached wrapper option", text: `sudo -uroot htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo long wrapper option", text: `sudo --user=root htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env wrapper", text: `env PASSWORD=value htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env wrapper option", text: `env -u PASSWORD htpasswd -C 10 -bn "" password`, want: 1},
		{name: "command wrapper", text: `command htpasswd -C 10 -bn "" password`, want: 1},
		{name: "command wrapper separator", text: `command -- htpasswd -C 10 -bn "" password`, want: 1},
		{name: "ionice wrapper", text: `ionice -c 2 htpasswd -C 10 -bn "" password`, want: 1},
		{name: "ionice attached option", text: `ionice -c2 htpasswd -C 10 -bn "" password`, want: 1},
		{name: "ionice abbreviated classdata", text: `ionice --classd 4 htpasswd -C 10 -bn "" password`, want: 1},
		{name: "ionice abbreviated ignore", text: `ionice --i htpasswd -C 10 -bn "" password`, want: 1},
		{name: "nested wrappers", text: `sudo env PASSWORD=value command htpasswd -C 10 -bn "" password`, want: 1},
		{name: "nested ionice wrapper", text: `sudo ionice -n 4 htpasswd -C 10 -bn "" password`, want: 1},
		{name: "uppercase bcrypt cost", text: `htpasswd -nBC 10 stargate`},
		{name: "later command", text: `htpasswd -nBC 10 stargate; printf '%s\\n' -b`},
		{name: "pipeline boundary", text: `htpasswd -nBC 10 stargate | sed -n -b`},
		{name: "shell comment", text: `htpasswd -nBC 10 stargate # never add -b`},
		{name: "comment before separator", text: `echo ok # avoid this; htpasswd -C 10 -bn "" password`},
		{name: "comment line before separator", text: `# explanation; htpasswd -C 10 -bn "" password`},
		{name: "comment after separator", text: `echo ok; # avoid this; htpasswd -C 10 -bn "" password`},
		{name: "next line after comment", text: "echo ok # comment\nhtpasswd -C 10 -bn \"\" password", want: 1},
		{name: "wrapper invokes another command", text: `sudo printf '%s\\n' htpasswd -bn`},
		{name: "command lookup", text: `command -v htpasswd -bn`},
		{name: "ionice PID query", text: `ionice -p 42 htpasswd -bn`},
		{name: "ionice PGID query", text: `ionice --pgid=42 htpasswd -bn`},
		{name: "ionice UID query", text: `ionice -u1000 htpasswd -bn`},
		{name: "ionice abbreviated PID query", text: `ionice --pi=42 htpasswd -bn`},
		{name: "ionice abbreviated PGID query", text: `ionice --pg=42 htpasswd -bn`},
		{name: "ionice abbreviated UID query", text: `ionice --u=1000 htpasswd -bn`},
		{name: "ionice abbreviated help", text: `ionice --he htpasswd -bn`},
		{name: "ionice abbreviated version", text: `ionice --ver htpasswd -bn`},
		{name: "ionice ambiguous option", text: `ionice --cl 2 htpasswd -bn`},
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
