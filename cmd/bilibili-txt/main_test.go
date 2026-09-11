package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVersion_NonEmpty(t *testing.T) {
	if strings.TrimSpace(version) == "" {
		t.Fatalf("version is empty")
	}
}

func TestBuildAndRun_LDFlagWiresVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-run e2e test under -short (compiles a real binary)")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("skipping: `go` not on PATH (%v); test-binary-only environments cannot rebuild", err)
	}

	tmpDir := t.TempDir()
	if gotmp := os.Getenv("GOTMPDIR"); gotmp != "" {
		d, err := os.MkdirTemp(gotmp, "t45-e2e-*")
		if err != nil {
			t.Fatalf("MkdirTemp under GOTMPDIR=%q: %v", gotmp, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		tmpDir = d
	}
	binName := "bilibili-txt-t45-e2e"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)

	const sentinel = "t45-e2e-ldflag-injected-42"

	buildCmd := exec.Command(
		"go", "build",
		"-trimpath",
		"-ldflags", "-X main.version="+sentinel,
		"-o", binPath,
		".",
	)
	buildCmd.Env = os.Environ()
	var buildStderr bytes.Buffer
	buildCmd.Stderr = &buildStderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("go build failed: %v\nstderr:\n%s", err, buildStderr.String())
	}

	runCmd := exec.Command(binPath, "--version")
	var runStdout bytes.Buffer
	runCmd.Stdout = &runStdout
	runCmd.Stderr = &buildStderr
	if err := runCmd.Run(); err != nil {
		t.Fatalf("running built binary --version failed: %v\nstdout:\n%s\nstderr:\n%s",
			err, runStdout.String(), buildStderr.String())
	}

	got := runStdout.String()
	if !strings.Contains(got, sentinel) {
		t.Fatalf("--version output missing injected sentinel:\n"+
			"  want substring: %q\n  got:            %q\n"+
			"(this usually means the -X target symbol path drifted; "+
			"check cmd/bilibili-txt/main.go `var version` and scripts/build.sh VERSION_LDFLAGS)",
			sentinel, got)
	}
}
