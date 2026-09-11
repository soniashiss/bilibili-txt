//go:build e2e

// Package e2e_test drives the compiled `bilibili-txt` binary against
// the *real* yt-dlp / ffmpeg / whisper-cli triple. It exists so that
// once a developer has installed the external toolchain (macOS:
// `brew install yt-dlp ffmpeg whisper-cpp` plus a whisper model), they
// can run a single smoke test to prove the full pipeline still lands
// a transcript.
//
// Isolation contract (plan §7.3, tasks.md T-4.3):
//
//   - `//go:build e2e` — the file is invisible to a plain
//     `go test ./...`; it only compiles under `-tags=e2e`.
//   - `BILIBILI_TXT_E2E=1` — even under `-tags=e2e`, the test t.Skips
//     unless this env var is set. This lets CI safely run
//     `go test -tags=e2e ./...` without accidentally hitting the
//     network on machines without the toolchain installed.
//   - `BILIBILI_TXT_E2E_URL` — the concrete video URL / BV id to
//     process. Required; the test does NOT embed a hard-coded URL
//     because bilibili video IDs are ephemeral (deletions, region
//     locks) and a baked-in id would rot the smoke test.
//
// Optional knobs (leave unset for defaults):
//
//   - BILIBILI_TXT_E2E_OUTDIR   — where to write the transcript.
//     Default: `.tmp/e2e/out-<pid>` under the repo root. Cleaned up
//     via t.Cleanup on success; kept on failure so the operator can
//     inspect the artefacts.
//   - BILIBILI_TXT_E2E_FORCE_ASR=1 — force the ASR branch even if the
//     video advertises CC subtitles. Useful when smoke-testing the
//     whisper wire-up after touching audio/asr code.
//   - BILIBILI_TXT_E2E_MODEL     — override the whisper model path.
//     Passed through as `--model`. Default is whatever the CLI
//     resolves from config.yaml / built-in default.
//   - BILIBILI_TXT_E2E_TIMEOUT   — max wall-clock budget for the CLI
//     run (Go duration syntax, e.g. `5m`). Default: 10 minutes,
//     covering a ~5-minute video going through whisper large-v3-turbo
//     on a 2020-era laptop.
//
// This is a smoke test, not an assertion suite: it exists to catch
// gross regressions (spawn failure, empty transcript, wrong exit
// code). Fine-grained pipeline behaviour lives in the integration
// package where the fakebin makes deterministic assertions possible.
package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// e2eEnv guards which environment variables gate/parametrise the
// smoke test. Keeping them in one place makes it easy to spot in code
// review whether a new knob was added ad-hoc.
const (
	envGate     = "BILIBILI_TXT_E2E"
	envURL      = "BILIBILI_TXT_E2E_URL"
	envOutDir   = "BILIBILI_TXT_E2E_OUTDIR"
	envForceASR = "BILIBILI_TXT_E2E_FORCE_ASR"
	envModel    = "BILIBILI_TXT_E2E_MODEL"
	envTimeout  = "BILIBILI_TXT_E2E_TIMEOUT"
)

// defaultTimeout bounds how long the CLI is allowed to run. whisper
// large-v3-turbo on a modern MacBook does ~1x realtime, so a
// 10-minute ceiling comfortably covers a 5-minute video plus the
// yt-dlp download + ffmpeg transcode overhead. Operators smoke
// testing longer videos should raise it via BILIBILI_TXT_E2E_TIMEOUT.
const defaultTimeout = 10 * time.Minute

// TestSmoke_RealToolchain runs the compiled CLI against the real
// yt-dlp/ffmpeg/whisper-cli triple. On success it proves that:
//
//   - the CLI binary builds under the current tree,
//   - preflight passes on the operator's machine (real tool detection),
//   - the pipeline reaches the success banner,
//   - a non-empty transcript lands in the requested output directory.
//
// The test deliberately does NOT assert on transcript content: the
// upstream video may change subtitles or captions between runs, and
// pinning a substring would turn a working pipeline into red CI
// noise. The integration package owns content-shape assertions.
func TestSmoke_RealToolchain(t *testing.T) {
	if os.Getenv(envGate) != "1" {
		t.Skipf("%s not set to 1; skipping real-toolchain smoke test", envGate)
	}
	url := strings.TrimSpace(os.Getenv(envURL))
	if url == "" {
		t.Fatalf("%s is empty; e2e smoke test requires an operator-supplied BV URL / id", envURL)
	}

	bin := buildBinary(t)
	outDir := resolveOutDir(t)
	timeout := resolveTimeout(t)

	t.Logf("e2e smoke: url=%s outDir=%s timeout=%s", url, outDir, timeout)

	// preRun snapshots the outDir's existing `.txt` files by absolute
	// path so that findNewTranscript (post-run) can insist on a file
	// that appeared *this* run. Without this guard, an operator-set
	// BILIBILI_TXT_E2E_OUTDIR that already has a stale transcript
	// would let the test PASS even if the CLI silently produced
	// nothing.
	preRun := snapshotFiles(t, outDir, ".txt")
	runStart := time.Now()

	// Debug / format / model / debug-log-path are all config.yaml
	// knobs now (no CLI flags). The CLI runs with cmd.Dir=outDir, so it
	// auto-loads <outDir>/config/config.yaml. We pin debug=true so the
	// run emits a full external-stderr log, and pin the debug log
	// inside outDir so it travels with the run's artefacts and gets
	// t.Cleanup'd together with the default outDir.
	debugLog := filepath.Join(outDir, "bilibili-txt.debug.log")
	writeSmokeConfig(t, outDir, smokeConfig{
		OutputDir:    outDir,
		Format:       "txt",
		Debug:        true,
		DebugLogFile: debugLog,
		Model:        strings.TrimSpace(os.Getenv(envModel)),
	})

	args := []string{}
	if os.Getenv(envForceASR) == "1" {
		args = append(args, "--force-asr")
	}
	args = append(args, url)

	stdout, stderr, exit := runBinary(t, bin, outDir, args, timeout)

	if exit != 0 {
		t.Fatalf("bilibili-txt exited with %d\nargs=%v\nstdout=%s\nstderr=%s",
			exit, args, stdout, stderr)
	}

	// The success banner ("bilibili-txt: <source> -> <path> …") is a
	// stable public contract — see printSuccess in
	// internal/cli/root.go and the T-4.3 acceptance clause in
	// docs/tasks.md. Anything else means the CLI took a code path it
	// should not have (e.g. printed an error to stdout).
	if !strings.HasPrefix(stdout, "bilibili-txt: ") || !strings.Contains(stdout, " -> ") {
		t.Fatalf("stdout missing success banner:\n%s", stdout)
	}

	// Reject SourceCached: if the operator re-runs against a
	// persistent BILIBILI_TXT_E2E_OUTDIR, the pipeline may print
	// `bilibili-txt: cached -> ...` and exit 0 without actually
	// running yt-dlp/ffmpeg/whisper. That still matches the generic
	// banner shape above, but downstream findNewTranscript would
	// only detect the mtime-unchanged pre-existing file and raise a
	// confusing "no fresh .txt" failure. Fail here with a clearer
	// hint so the operator knows to reset the outDir.
	if strings.Contains(stdout, "bilibili-txt: cached -> ") {
		t.Fatalf("CLI short-circuited via SourceCached; e2e smoke requires a fresh pipeline run.\n"+
			"Hint: clear %s (or unset it to use the default per-run dir).\nstdout=%s",
			envOutDir, stdout)
	}

	// findNewTranscript closes the loop with the pre-run snapshot:
	// only files that either did not exist before or were modified
	// after runStart count. This defends against (a) stale artefacts
	// in operator-supplied outDirs and (b) PID recycling hitting a
	// leftover `out-<pid>` directory from a prior test run whose
	// t.Cleanup failed to fire.
	produced := findNewTranscript(t, outDir, ".txt", preRun, runStart)
	if !strings.HasSuffix(produced, ".txt") {
		t.Fatalf("produced artefact %s does not end with .txt", produced)
	}
	info, err := os.Stat(produced)
	if err != nil {
		t.Fatalf("stat produced transcript %s: %v", produced, err)
	}
	if info.Size() == 0 {
		t.Fatalf("produced transcript %s is empty (0 bytes)", produced)
	}
	t.Logf("e2e smoke: transcript=%s size=%dB debugLog=%s", produced, info.Size(), debugLog)
}

// smokeConfig carries the knobs the e2e run writes into its per-run
// config.yaml (the debug/format/model/log-path flags no longer exist on
// the CLI; they live in config.yaml).
type smokeConfig struct {
	OutputDir    string
	Format       string
	Debug        bool
	DebugLogFile string
	Model        string
}

// writeSmokeConfig materialises cfg as <outDir>/config/config.yaml. The
// CLI auto-discovers that default path because runBinary pins
// cmd.Dir=outDir.
func writeSmokeConfig(t *testing.T, outDir string, cfg smokeConfig) {
	t.Helper()
	dir := filepath.Join(outDir, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir e2e config dir %s: %v", dir, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "output_dir: %s\n", cfg.OutputDir)
	fmt.Fprintf(&b, "format: %s\n", cfg.Format)
	fmt.Fprintf(&b, "debug: %t\n", cfg.Debug)
	if cfg.DebugLogFile != "" {
		fmt.Fprintf(&b, "logging:\n  debug_file: %s\n", cfg.DebugLogFile)
	}
	if cfg.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", cfg.Model)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write e2e config %s: %v", path, err)
	}
}

// buildBinary compiles `bilibili-txt` into `.tmp/e2e/bilibili-txt` and
// returns the absolute path. Kept out of TestMain because a build tag
// switch already prevents this file from touching a plain
// `go test ./...`; wiring the build into the test itself keeps the
// helper list tight.
//
// Uses -trimpath so the produced binary is reproducible across
// developer machines (matters for anyone diffing binaries; not a
// correctness requirement).
func buildBinary(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	buildDir := filepath.Join(root, ".tmp", "e2e")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", buildDir, err)
	}
	bin := filepath.Join(buildDir, "bilibili-txt")

	// Redirect the Go build's own scratch space too. The integration
	// package already documents why this is necessary under sandboxed
	// environments (`.tmp/gocache` + `.tmp/gotmp` under repo root
	// dodge /var/folders restrictions). Harmless on a plain macOS
	// developer setup.
	cacheDir := filepath.Join(root, ".tmp", "gocache")
	goTmp := filepath.Join(root, ".tmp", "gotmp")
	for _, d := range []string{cacheDir, goTmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	env := append(os.Environ(),
		"GOCACHE="+cacheDir,
		"GOTMPDIR="+goTmp,
	)

	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/bilibili-txt")
	cmd.Dir = root
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, buf.String())
	}
	return bin
}

// resolveOutDir returns the transcript output directory to hand the
// CLI. If BILIBILI_TXT_E2E_OUTDIR is set the operator gets full
// control (useful when hunting a bug — the artefacts survive the test
// process). Otherwise we mint a fresh `.tmp/e2e/out-<pid>` dir and
// register it for cleanup on success only, so a failing run's
// evidence is not silently wiped.
func resolveOutDir(t *testing.T) string {
	t.Helper()
	if custom := strings.TrimSpace(os.Getenv(envOutDir)); custom != "" {
		// Absolutise up front. snapshotFiles keys its pre-run map by
		// filepath.Join(dir, name), and findNewTranscript compares
		// against those keys after the CLI has spawned with
		// cmd.Dir=outDir. If `custom` were relative, an intervening
		// chdir (test framework, future refactor, …) could make the
		// two ends resolve against different bases and mis-classify
		// a pre-existing file as "produced this run". Fail loudly
		// on Abs errors — a relative path we can't absolutise is a
		// hint that the operator's env is broken.
		abs, err := filepath.Abs(custom)
		if err != nil {
			t.Fatalf("filepath.Abs %s (%s): %v", custom, envOutDir, err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatalf("mkdir %s (%s): %v", abs, envOutDir, err)
		}
		return abs
	}
	root := repoRoot(t)
	dir := filepath.Join(root, ".tmp", "e2e", fmt.Sprintf("out-%d", os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("e2e smoke: preserving %s for post-mortem", dir)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

// resolveTimeout parses BILIBILI_TXT_E2E_TIMEOUT (Go duration syntax)
// or returns defaultTimeout. A zero/empty env value uses the default;
// a malformed value fails the test loudly rather than silently
// choosing the default — the operator's intent was clearly "override",
// so a silent fallback would hide the typo.
func resolveTimeout(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envTimeout))
	if raw == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", envTimeout, raw, err)
	}
	if d <= 0 {
		t.Fatalf("invalid %s=%q: must be positive", envTimeout, raw)
	}
	return d
}

// runBinary spawns the built CLI with the given args, streaming
// stdout/stderr into in-memory buffers. It enforces the timeout by
// killing the CLI's main process on expiry — child processes spawned
// by the CLI (yt-dlp / ffmpeg / whisper-cli) may be orphaned to
// launchd/init and continue briefly. Full process-group teardown is
// intentionally not implemented in the v1 skeleton: it would require
// syscall.Setpgid + a signed negative-PID kill, and the outer
// `go test -timeout` already provides the ultimate wall-clock hammer.
// If a stuck child ever becomes a real operational problem, tighten
// this in T-4.5. For now we prioritise a clean stderr snapshot before
// the outer timeout drops.
//
// cmd.Dir is pinned to outDir so any CLI code path that resolves a
// relative default path (notably logger.defaultLogsDir → `./logs/`)
// lands inside outDir alongside the transcript, keeping the working
// tree clean even if the operator forgot to pass --debug-log-file.
//
// stdin is a blank buffer, mirroring integration_test.runCLI, so the
// CLI's TTY probe deterministically flips to "not a TTY" — same
// reason: on-conflict=ask must not silently pop a prompt inside a
// non-interactive smoke test.
func runBinary(t *testing.T, bin, outDir string, args []string, timeout time.Duration) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = outDir
	cmd.Stdin = &bytes.Buffer{}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Env = os.Environ() // inherit the operator's PATH / config.yaml discovery

	if err := cmd.Start(); err != nil {
		t.Fatalf("start CLI: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return outBuf.String(), errBuf.String(), ee.ExitCode()
			}
			t.Fatalf("wait CLI: unexpected error: %v\nstdout=%s\nstderr=%s",
				err, outBuf.String(), errBuf.String())
		}
		return outBuf.String(), errBuf.String(), 0
	case <-time.After(timeout):
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("kill CLI after timeout: %v", err)
		}
		// Drain the goroutine so we don't leak it — but bound the
		// wait. If the child sits in uninterruptible IO (e.g.
		// whisper-cli mid-mmap), `<-done` could block long enough
		// for the outer `go test -timeout` to fire, which prints
		// "test timed out …" and drops the stdout/stderr snapshot we
		// carefully buffered. Cap the drain at 5s so we always land
		// our own t.Fatalf with the buffered output; if the goroutine
		// does leak, the test process exits shortly after anyway.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("CLI wait did not return within 5s after Kill; abandoning goroutine")
		}
		t.Fatalf("CLI timed out after %s\nstdout=%s\nstderr=%s",
			timeout, outBuf.String(), errBuf.String())
	}
	return "", "", -1 // unreachable; keeps the compiler happy
}

// snapshotFiles records the absolute paths of every regular file
// under dir whose name ends in suffix, alongside the file's ModTime.
// The returned map lets findNewTranscript distinguish "brand new
// file" from "existing file that was not touched this run", which is
// the only way to keep an operator-supplied BILIBILI_TXT_E2E_OUTDIR
// honest — otherwise a stale transcript from a prior run would let
// the current test PASS even if the CLI produced nothing.
//
// suffix must match the suffix later handed to findNewTranscript;
// mismatch would silently under-populate the pre-run set and let a
// pre-existing file be misclassified as "produced this run".
// Caller-supplied so both ends stay in lockstep.
//
// A missing dir yields an empty snapshot (not an error): callers
// pass this before the CLI has had a chance to create the dir.
func snapshotFiles(t *testing.T, dir, suffix string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("snapshotFiles read dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("snapshotFiles stat %s: %v", e.Name(), err)
		}
		out[filepath.Join(dir, e.Name())] = info.ModTime()
	}
	return out
}

// findNewTranscript scans dir for regular files ending in suffix that
// either did not exist in preRun or were modified on or after
// runStart, and returns the most recently modified one. It fails the
// test if no such file exists, listing what *was* in the directory so
// the operator can spot "CLI wrote to a different path than expected"
// vs "CLI wrote nothing at all".
//
// The two-layer guard (pre-run snapshot + runStart mtime floor)
// defends against:
//
//   - stale transcripts in operator-supplied outDirs
//   - PID recycling hitting a leftover `out-<pid>` from a prior test
//     run whose t.Cleanup failed to fire
//   - filesystems with second-granularity mtime where "same second"
//     ambiguity could otherwise let a stale file win the tie-break
//     (we accept "modTime >= runStart" and rely on the pre-run
//     snapshot to filter out same-second-yet-untouched siblings).
func findNewTranscript(t *testing.T, dir, suffix string, preRun map[string]time.Time, runStart time.Time) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var (
		best    string
		bestMod time.Time
		seen    []string
	)
	// Truncate runStart to the same resolution most filesystems
	// expose (1 second on macOS HFS+/APFS mtime is nanosecond, on
	// some CI ext4 mounts it may be 1s). Compare with Before instead
	// of Equal-and-Before to be inclusive.
	floor := runStart.Truncate(time.Second)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		seen = append(seen, e.Name())
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		mod := info.ModTime()
		if prevMod, existed := preRun[full]; existed && !mod.After(prevMod) {
			// Untouched file from a prior run — skip.
			continue
		}
		if mod.Before(floor) {
			// Predates this run; not our artefact.
			continue
		}
		if best == "" || mod.After(bestMod) {
			best = full
			bestMod = mod
		}
	}
	if best == "" {
		t.Fatalf("no fresh %s file under %s produced during this run (preRun snapshot had %d entries, dir now has: %v)",
			suffix, dir, len(preRun), seen)
	}
	return best
}

// repoRoot climbs two levels up from this source file
// (`test/e2e/smoke_test.go`) using runtime.Caller. Same approach the
// integration package uses; avoids depending on the CWD `go test`
// happens to pick.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
