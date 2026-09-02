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
		{name: "sudo abbreviated long wrapper option", text: `sudo --us=root htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo reset timestamp and command", text: `sudo -k htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo no-update and command", text: `sudo -N htpasswd -C 10 -bn "" password`, want: 1},
		{name: "sudo optional long operand", text: `sudo --preserve-env=HOME htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env wrapper", text: `env PASSWORD=value htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env wrapper option", text: `env -u PASSWORD htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env lone dash", text: `env - htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env split string", text: `env -S 'htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env attached split string", text: `env -S'htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env long split string", text: `env --split-string='htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env abbreviated split string", text: `env --split='htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env split options", text: `env -S '-i PASSWORD=value htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env split nested wrapper", text: `env -S 'sudo htpasswd -C 10 -bn "" password'`, want: 1},
		{name: "env empty split string", text: `env -S '' htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env optional signal operand", text: `env --block-signal=TERM htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env non-shell assignment name", text: `env FOO-BAR=value htpasswd -C 10 -bn "" password`, want: 1},
		{name: "env assignment after separator", text: `env -- -FOO=value htpasswd -C 10 -bn "" password`, want: 1},
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
		{name: "sudo list", text: `sudo -l htpasswd -bn`},
		{name: "sudo abbreviated list", text: `sudo --li htpasswd -bn`},
		{name: "sudo edit", text: `sudo -e htpasswd -bn`},
		{name: "sudo abbreviated edit", text: `sudo --ed htpasswd -bn`},
		{name: "sudo validate", text: `sudo -v htpasswd -bn`},
		{name: "sudo long validate", text: `sudo --validate htpasswd -bn`},
		{name: "sudo version", text: `sudo -V htpasswd -bn`},
		{name: "sudo abbreviated version", text: `sudo --ver htpasswd -bn`},
		{name: "sudo remove timestamp", text: `sudo -K htpasswd -bn`},
		{name: "sudo long remove timestamp", text: `sudo --remove-timestamp htpasswd -bn`},
		{name: "sudo list-mode other user", text: `sudo -U root htpasswd -bn`},
		{name: "sudo long help", text: `sudo --help htpasswd -bn`},
		{name: "sudo ambiguous long option", text: `sudo --preserve htpasswd -bn`},
		{name: "env split invokes another command", text: `env -S 'printf htpasswd -bn'`},
		{name: "env null mode", text: `env -0 htpasswd -bn`},
		{name: "env long null mode", text: `env --null htpasswd -bn`},
		{name: "env help", text: `env --help htpasswd -bn`},
		{name: "env version", text: `env --version htpasswd -bn`},
		{name: "env abbreviated version", text: `env --ver htpasswd -bn`},
		{name: "env ambiguous option", text: `env --ignore htpasswd -bn`},
		{name: "env assignment ends options", text: `env FOO=value -i htpasswd -bn`},
		{name: "shell non-assignment command", text: `FOO-BAR=value htpasswd -bn`},
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
