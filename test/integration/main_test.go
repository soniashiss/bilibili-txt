//go:build !windows

// Package integration_test drives the compiled `bilibili-txt` binary
// end-to-end against the fake yt-dlp/ffmpeg/whisper-cli shims under
// [testdata/fakebin]. It is intentionally in its own package so it
// only exercises the public CLI surface — anything reachable only via
// unexported helpers belongs in unit tests, not here.
//
// TestMain compiles the CLI once (into `.tmp/integration/bilibili-txt`)
// and every test then shells out to that binary. The compiled path is
// exposed to individual tests through [binaryPath]; the fakebin dir is
// exposed through [fakebinDir] so tests that need to prepend it to
// PATH don't hard-code the layout.
//
// Layout choices explained:
//
//   - We do NOT use `t.TempDir()` for the binary or the run dirs. The
//     macOS sandbox blocks exec of binaries living under /var/folders,
//     and the fake shell scripts we shell out to (bash + cp) hit the
//     same wall on writes. Everything therefore lives inside the repo
//     under `.tmp/integration/*`, matching the pattern the pipeline
//     unit tests already use.
//   - `go build -o …` is invoked with `-trimpath` so debug builds
//     don't embed absolute host paths into the binary (matters for
//     reproducibility only; not a test requirement).
package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// binPath holds the absolute path of the freshly-built CLI binary. It
// is populated by TestMain before any Test* runs and read (never
// written) from individual tests via [binaryPath].
var binPath string

// TestMain builds the CLI once per `go test` invocation. Sharing the
// binary across tests is safe because Execute has no shared global
// state — each run gets its own os.Args / os.Stdin / cwd via exec.
func TestMain(m *testing.M) {
	root := repoRootStatic()
	if root == "" {
		fmt.Fprintln(os.Stderr, "integration: cannot locate repo root")
		os.Exit(2)
	}

	buildDir := filepath.Join(root, ".tmp", "integration")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "integration: mkdir build dir: %v\n", err)
		os.Exit(2)
	}
	binPath = filepath.Join(buildDir, "bilibili-txt")

	// Route the build's own scratch space into the repo-local cache
	// too, so a sandboxed environment (which may block
	// $HOME/Library/Caches or /var/folders) can still complete the
	// compile. These env vars only affect the child `go build`; the
	// current process's cache setup is untouched.
	env := append(os.Environ(),
		"GOCACHE="+filepath.Join(root, ".tmp", "gocache"),
		"GOTMPDIR="+filepath.Join(root, ".tmp", "gotmp"),
	)
	for _, p := range []string{"gocache", "gotmp"} {
		if err := os.MkdirAll(filepath.Join(root, ".tmp", p), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "integration: mkdir %s: %v\n", p, err)
			os.Exit(2)
		}
	}

	cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "./cmd/bilibili-txt")
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: go build failed: %v\n", err)
		os.Exit(2)
	}

	os.Exit(m.Run())
}

// binaryPath returns the compiled CLI binary path. Tests should always
// go through this accessor so a future migration to per-test binaries
// doesn't force a shotgun edit.
func binaryPath(t *testing.T) string {
	t.Helper()
	if binPath == "" {
		t.Fatal("integration: binPath not set — TestMain did not run?")
	}
	return binPath
}

// fakebinDir returns the absolute path to the shell-script shims. It
// is deliberately a helper (not a constant) so tests can log the
// value on failure without a `path/filepath` import.
func fakebinDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", "fakebin")
}

// repoRoot returns the module root by climbing two levels up from this
// file (`test/integration/main_test.go`). Using runtime.Caller keeps
// us decoupled from the CWD `go test` picks (usually the package dir,
// but IDE test runners sometimes differ).
func repoRoot(t *testing.T) string {
	t.Helper()
	root := repoRootStatic()
	if root == "" {
		t.Fatal("integration: cannot locate repo root")
	}
	return root
}

// repoRootStatic is the TestMain-safe variant of [repoRoot] (no *testing.T).
func repoRootStatic() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// integrationTempDir mints a fresh output/tmp dir under `.tmp/integration/`.
// The base dir is created if missing and the concrete run dir is
// registered for cleanup at test end.
//
// Sub-paths are namespaced by test name (`.tmp/integration/<test>-<rand>`)
// so a failing parallel test's leftover output does not confuse the
// next run.
func integrationTempDir(t *testing.T, tag string) string {
	t.Helper()
	base := filepath.Join(repoRoot(t), ".tmp", "integration", tag)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir tmp base %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "run-*")
	if err != nil {
		t.Fatalf("mktemp under %s: %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// prependPath returns a `PATH=...` env var with `dir` at the head of
// the caller's current PATH. Used by every test to force fakebin ahead
// of any real yt-dlp the developer might have installed system-wide.
func prependPath(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// integrationConfig captures the fields §4.6 relocated from CLI flags
// into config.yaml. Tests that used to write `--skip-preflight`,
// `--format`, `--model`, `--log-format`, `--log-file`,
// `--debug-log-file` on argv now pass those knobs through here and
// let writeIntegrationConfig persist them to a per-test yaml plus a
// `--config <path>` fragment on argv. Zero-valued fields are omitted
// from the yaml so a fixture that only needs `skip_preflight` doesn't
// accidentally over-specify the other keys.
type integrationConfig struct {
	SkipPreflight bool
	Format        string
	Model         string
	OutputDir     string
	LogFormat     string
	LogFile       string
	DebugLogFile  string
	// Extra lines appended verbatim (e.g. `binaries:` fragments) for
	// tests that need shape yaml can't express in a Go struct alone.
	Extra []string
}

// writeIntegrationConfig materialises cfg as a yaml file under a
// fresh integrationTempDir and returns argv fragments the caller can
// splice ahead of the URL positional. Every §4.6 relocated flag is
// covered so a test can migrate by replacing e.g. `"--skip-preflight",
// "--format", "txt"` with `writeIntegrationConfig(t, tag,
// integrationConfig{SkipPreflight: true, Format: "txt"})`.
//
// The returned slice is always of the form `[]string{"--config", path}`
// so callers can splice it verbatim; if cfg is empty (no fields set,
// no Extra) the returned slice is nil so the argv stays clean.
func writeIntegrationConfig(t *testing.T, tag string, cfg integrationConfig) []string {
	t.Helper()
	var lines []string
	if cfg.OutputDir != "" {
		lines = append(lines, "output_dir: "+cfg.OutputDir)
	}
	if cfg.Format != "" {
		lines = append(lines, "format: "+cfg.Format)
	}
	if cfg.Model != "" {
		lines = append(lines, "model: "+cfg.Model)
	}
	if cfg.LogFormat != "" || cfg.LogFile != "" || cfg.DebugLogFile != "" {
		lines = append(lines, "logging:")
		if cfg.LogFormat != "" {
			lines = append(lines, "  format: "+cfg.LogFormat)
		}
		if cfg.LogFile != "" {
			lines = append(lines, "  file: "+cfg.LogFile)
		}
		if cfg.DebugLogFile != "" {
			lines = append(lines, "  debug_file: "+cfg.DebugLogFile)
		}
	}
	if cfg.SkipPreflight {
		lines = append(lines, "skip_preflight: true")
	}
	lines = append(lines, cfg.Extra...)
	if len(lines) == 0 {
		return nil
	}
	dir := integrationTempDir(t, tag+"-cfg")
	body := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write integration yaml %s: %v", path, err)
	}
	return []string{"--config", path}
}
