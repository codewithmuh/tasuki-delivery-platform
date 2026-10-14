package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestRunCommands(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: bad\nwards: [atlantis]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noToken := map[string]string{"STATE_FILE": filepath.Join(dir, "state.json"), "SEED_WAIT": "10ms"}

	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		wantCode int
		wantOut  string
	}{
		{"no command", nil, nil, 2, "usage: simulator"},
		{"unknown command", []string{"fly"}, nil, 2, "usage: simulator"},
		{"version", []string{"version"}, nil, 0, "dev"},
		{"validate built-ins", []string{"validate"}, nil, 0, "rainy-friday: ok"},
		{"validate a bad file", []string{"validate", bad}, nil, 1, "unknown ward"},
		{"bad flag", []string{"run", "-nope"}, nil, 2, "flag provided but not defined"},
		{"unknown scenario", []string{"run", "-scenario", "atlantis"}, nil, 1, "not a built-in scenario"},
		{"seeding needs ADMIN_TOKEN", []string{"seed", "-seed", "9"}, noToken, 1, "ADMIN_TOKEN is required"},
		{"run needs ADMIN_TOKEN", []string{"run", "-scenario", "small", "-duration", "1s"},
			map[string]string{"STATE_FILE": filepath.Join(dir, "s.json"), "PORT": "0"}, 1, "ADMIN_TOKEN is required"},
		{"healthcheck with nothing listening", []string{"healthcheck"}, map[string]string{"PORT": "1"}, 1, "probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, env(tc.env), &stdout, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if out := stdout.String() + stderr.String(); !strings.Contains(out, tc.wantOut) {
				t.Errorf("output %q does not contain %q", out, tc.wantOut)
			}
		})
	}
}
