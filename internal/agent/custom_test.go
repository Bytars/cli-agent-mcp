package agent

import "testing"

func TestExpandArg(t *testing.T) {
	vals := map[string]string{
		"prompt":  "fix the bug",
		"cwd":     "/tmp/x",
		"model":   "",
		"session": "",
	}

	tests := []struct {
		name string
		tmpl string
		want string
		keep bool
	}{
		{"no placeholder", "--yes", "--yes", true},
		{"prompt substituted", "{{prompt}}", "fix the bug", true},
		{"embedded", "--message={{prompt}}", "--message=fix the bug", true},
		{"cwd substituted", "--dir={{cwd}}", "--dir=/tmp/x", true},
		// Empty values drop the whole argument; this is what keeps an optional
		// flag from being passed with a missing value.
		{"empty model drops arg", "--model={{model}}", "", false},
		{"empty session drops arg", "--resume={{session}}", "", false},
		{"multiple, one empty, drops", "{{prompt}}-{{model}}", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, keep := expandArg(tc.tmpl, vals)
			if keep != tc.keep {
				t.Fatalf("keep = %v, want %v (got %q)", keep, tc.keep, got)
			}
			if keep && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCustomAdapter_UnconfiguredIsUnavailable(t *testing.T) {
	a := NewCustomAdapter("", "", nil)
	if a.Name() != "custom" {
		t.Errorf("default name = %q, want %q", a.Name(), "custom")
	}
	if ok, detail := a.Available(); ok {
		t.Errorf("unconfigured custom adapter reported available: %s", detail)
	}
}

func TestCustomAdapter_BuildsArgs(t *testing.T) {
	a := NewCustomAdapter("aider", "echo", []string{"--yes", "--message={{prompt}}", "--model={{model}}"})
	cmd, err := a.Command(t.Context(), RunSpec{Prompt: "hi", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Command() error: %v", err)
	}
	joined := cmd.Args
	if !hasFlag(joined, "--yes") {
		t.Errorf("static arg missing: %v", joined)
	}
	if !hasFlag(joined, "--message=hi") {
		t.Errorf("prompt not substituted: %v", joined)
	}
	// Model was empty, so its argument must be gone entirely.
	for _, a := range joined {
		if a == "--model=" {
			t.Errorf("empty model produced a dangling flag: %v", joined)
		}
	}
}

// A plain-text CLI has no result event, so its output must become the result.
func TestCustomAdapter_UsesOutputAsResult(t *testing.T) {
	a := NewCustomAdapter("custom", "echo", []string{"{{prompt}}"})
	if !a.UseOutputAsResult() {
		t.Error("custom adapter should use its output as the task result")
	}
}

func TestCustomAdapter_ParsesPlainTextAsProgress(t *testing.T) {
	a := NewCustomAdapter("custom", "echo", nil)
	ev := a.ParseLine("building project...")
	if ev.Raw != "building project..." {
		t.Errorf("Raw not preserved: %q", ev.Raw)
	}
	if ev.Text != "building project..." {
		t.Errorf("plain text should surface as progress, got %q", ev.Text)
	}
}
