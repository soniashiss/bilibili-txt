package pathx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize_EmptyIsUnspecified(t *testing.T) {
	got, err := Normalize("   ", "field")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Fatalf("empty input should yield \"\", got %q", got)
	}
}

func TestNormalize_TildeExpansion(t *testing.T) {
	fakeHome(t, "/tmp/pathx-home")

	cases := []struct {
		in, want string
	}{
		{"~", "/tmp/pathx-home"},
		{"~/logs", "/tmp/pathx-home/logs"},
		{"~/a/../b", "/tmp/pathx-home/a/../b"}, // filepath.Join cleans, so become /tmp/pathx-home/b
	}
	for _, tc := range cases {
		got, err := Normalize(tc.in, "field")
		if err != nil {
			t.Fatalf("Normalize(%q): unexpected err: %v", tc.in, err)
		}
		want := filepath.Clean(tc.want)
		if filepath.Clean(got) != want {
			t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, want)
		}
	}
}

func TestNormalize_TildeUserRejected(t *testing.T) {
	_, err := Normalize("~alice/tools", "field")
	if err == nil {
		t.Fatal("expected error for ~user syntax")
	}
	if !strings.Contains(err.Error(), "~user") {
		t.Errorf("error should mention ~user syntax, got: %v", err)
	}
	if !strings.Contains(err.Error(), "field") {
		t.Errorf("error should echo field name, got: %v", err)
	}
}

func TestNormalize_AbsoluteIsCleaned(t *testing.T) {
	got, err := Normalize("/tmp//a/b/../c", "field")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != filepath.Clean("/tmp/a/c") {
		t.Errorf("Normalize did not clean absolute path: %q", got)
	}
}

func TestNormalize_RelativeJoinsCwd(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	got, err := Normalize("sub/file", "field")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// macOS reports Getwd() with the symlink /var → /private/var already
	// resolved, so we compare against the resolved tempdir directly (no
	// EvalSymlinks call on `got`, which would try to stat a nonexistent
	// path and return "").
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	want := filepath.Join(resolvedDir, "sub", "file")
	if got != want {
		t.Errorf("Normalize(sub/file) = %q; want %q", got, want)
	}
}

func TestAbsFromCwd_RejectsTildeAsRelative(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	got, err := AbsFromCwd("~/whatever", "field")
	if err != nil {
		t.Fatalf("AbsFromCwd should not expand ~, but should treat it as a relative path; got err: %v", err)
	}
	// AbsFromCwd treats "~/whatever" as a plain relative path — the ~
	// stays verbatim as the first path segment. Callers (like logger)
	// reject ~-prefixed paths *before* invoking this helper.
	if !strings.HasSuffix(got, "~/whatever") && !strings.HasSuffix(got, filepath.FromSlash("~/whatever")) {
		t.Errorf("AbsFromCwd(~/whatever) = %q; want it to end with ~/whatever", got)
	}
}

func TestAbsFromCwd_Empty(t *testing.T) {
	got, err := AbsFromCwd("", "field")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Fatalf("empty input should yield \"\", got %q", got)
	}
}

func TestAbsFromCwd_AbsIsCleaned(t *testing.T) {
	got, err := AbsFromCwd("/tmp//a/../b", "field")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != filepath.Clean("/tmp/b") {
		t.Errorf("AbsFromCwd(/tmp//a/../b) = %q; want /tmp/b", got)
	}
}

func fakeHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows fallback; harmless on unix
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}
