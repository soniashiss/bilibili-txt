//go:build !windows

package integration_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI executes the compiled binary with the given args + env and
// returns stdout/stderr as strings plus the exit code.
//
// Exit code extraction handles both clean exits (via ExitCode()) and
// abnormal terminations (signal, oom, ...) which end up as -1 —
// runCLI itself upgrades any -1 into a hard t.Fatalf so individual
// tests never mistake an abnormal termination for a "just not 0"
// business error. Legitimate signal-driven exit codes (e.g. 130 for
// SIGINT) go through cmd.ProcessState and never surface as -1.
func runCLI(t *testing.T, args []string, extraEnv []string, tmpDir string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(binaryPath(t), args...)
	cmd.Dir = tmpDir
	// A blank stdin is important: the CLI treats os.Stdin.Stat's
	// ModeCharDevice bit as the TTY signal, and a nil Stdin field
	// makes exec inherit the test-runner's pipe, which happens to
	// look like a device on some macOS setups. Passing an empty
	// bytes.Buffer forces the "not a TTY" branch deterministically.
	cmd.Stdin = &bytes.Buffer{}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	env := append(os.Environ(), extraEnv...)
	cmd.Env = env

	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code := ee.ExitCode()
			if code == -1 {
				// Abnormal termination (signal, killed, ...) —
				// callers only ever compare exit against 0/1/3;
				// treating -1 as "not 0" would silently paper over
				// crash-style failures. Fail hard so a regression
				// that segfaults the binary is caught here rather
				// than misinterpreted downstream.
				t.Fatalf("run CLI: abnormal termination (exit=-1); args=%v\nstdout=%s\nstderr=%s",
					args, outBuf.String(), errBuf.String())
			}
			return outBuf.String(), errBuf.String(), code
		}
		// Non-ExitError (couldn't spawn, IO error) — surface as a
		// hard failure so callers can't confuse it with a normal
		// exit.
		t.Fatalf("run CLI: unexpected error type: %v\nstdout=%s\nstderr=%s",
			err, outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String(), 0
}

// TestSubtitleBranch_HappyPath drives the CLI through the full subtitle
// pipeline against the fake yt-dlp. It asserts:
//
//   - exit code 0
//   - stdout contains the success banner naming the cc-subtitle source
//   - a `<slug>__<bvid>.txt` file lands under --output-dir
//   - the txt content includes the first sample cue verbatim so we
//     know the fake srt was actually parsed + formatted
func TestSubtitleBranch_HappyPath(t *testing.T) {
	outDir := integrationTempDir(t, "subtitle-happy-out")
	tmpDir := integrationTempDir(t, "subtitle-happy-tmp")

	// URL must satisfy the CLI's own BV regex (BV + ≥8 chars), so we
	// use a synthetic 10-char id. The fake yt-dlp does not care about
	// the specific id — its metadata.json fixture always advertises
	// `BVfake123`, which is what ends up on the output filename.
	url := "https://www.bilibili.com/video/BV1abcdefghij"

	cfgArgs := writeIntegrationConfig(t, "subtitle-happy", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	// Banner assertion — the source string is frozen in
	// pipeline.SourceSubtitle. Guarding against a silent switch to
	// SourceAuto (which would signal a language-priority regression).
	if !strings.Contains(stdout, "bilibili-txt: cc-subtitle -> ") {
		t.Errorf("stdout missing success banner; got: %q", stdout)
	}

	// Output file — the fake metadata's BVID is `BVfake123`, so the
	// filename must end in `__BVfake123.txt` regardless of the slug.
	produced := findFile(t, outDir, "__BVfake123.txt")
	body, err := os.ReadFile(produced)
	if err != nil {
		t.Fatalf("read produced file %s: %v", produced, err)
	}
	if want := "大家好，欢迎收看本期视频。"; !strings.Contains(string(body), want) {
		t.Errorf("txt output missing sample cue %q; got:\n%s", want, string(body))
	}
}

// TestSubtitleBranch_ConflictNoInteractive verifies contracts.md §四
// exit-3 mapping: an existing output file + on_conflict=ask + no TTY
// (which --no-interactive forces) must fail without touching the
// downloader and return exit 3.
func TestSubtitleBranch_ConflictNoInteractive(t *testing.T) {
	outDir := integrationTempDir(t, "subtitle-conflict-out")
	tmpDir := integrationTempDir(t, "subtitle-conflict-tmp")

	// The pipeline computes its own filename from naming.Slug against
	// the metadata title/bvid. We can't reconstruct that slug here
	// without duplicating naming logic (and coupling this test to it),
	// so run the CLI once with --overwrite to *materialise* the
	// canonical filename, then rewrite it with a sentinel we can
	// prove the second run did not touch.
	preRunTmp := integrationTempDir(t, "subtitle-conflict-prerun")
	preCfg := writeIntegrationConfig(t, "subtitle-conflict-pre", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	preArgs := append([]string(nil), preCfg...)
	preArgs = append(preArgs, "--overwrite", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	if _, _, exit := runCLI(t, preArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + preRunTmp,
	}, preRunTmp); exit != 0 {
		t.Fatalf("pre-run to discover filename failed: exit=%d", exit)
	}
	canonical := findFile(t, outDir, "__BVfake123.txt")
	// Overwrite the canonical file with a well-known sentinel so
	// downstream assertions can prove the pipeline did NOT touch it.
	sentinel := []byte("SENTINEL-EXISTING-DO-NOT-OVERWRITE")
	if err := os.WriteFile(canonical, sentinel, 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	runCfg := writeIntegrationConfig(t, "subtitle-conflict-run", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	runArgs := append([]string(nil), runCfg...)
	runArgs = append(runArgs, "--no-interactive", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	stdout, stderr, exit := runCLI(t, runArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 3 {
		t.Fatalf("expected exit=3 (conflict), got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// Stderr should mention "already exists" so users can tell what
	// blocked them. The exact wording lives inside naming.ResolveConflict
	// so we only assert on the substring, not the whole phrase.
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr missing 'already exists' hint; got: %q", stderr)
	}
	// And the sentinel file must be intact — an accidental
	// overwrite would defeat the whole point of exit-3.
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read post-conflict file: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("existing file was overwritten despite exit=3; got %q", string(got))
	}
}

// TestSubtitleBranch_SkipReturnsCached — with --skip the CLI should
// short-circuit to the SourceCached banner (exit 0) and leave the
// pre-existing file untouched.
func TestSubtitleBranch_SkipReturnsCached(t *testing.T) {
	outDir := integrationTempDir(t, "subtitle-skip-out")
	tmpDir := integrationTempDir(t, "subtitle-skip-tmp")

	// Same trick as the conflict test: run once with --overwrite to
	// discover the canonical filename, then rewrite it as a
	// sentinel we can prove was not touched.
	preRunTmp := integrationTempDir(t, "subtitle-skip-prerun")
	preCfg := writeIntegrationConfig(t, "subtitle-skip-pre", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	preArgs := append([]string(nil), preCfg...)
	preArgs = append(preArgs, "--overwrite", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	if _, _, exit := runCLI(t, preArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + preRunTmp,
	}, preRunTmp); exit != 0 {
		t.Fatalf("pre-run failed: exit=%d", exit)
	}
	canonical := findFile(t, outDir, "__BVfake123.txt")
	sentinel := []byte("SENTINEL-KEEP-ME")
	if err := os.WriteFile(canonical, sentinel, 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	runCfg := writeIntegrationConfig(t, "subtitle-skip-run", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	runArgs := append([]string(nil), runCfg...)
	runArgs = append(runArgs, "--skip", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	stdout, stderr, exit := runCLI(t, runArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("expected exit=0 (cached), got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bilibili-txt: cached -> ") {
		t.Errorf("stdout missing cached banner; got: %q", stdout)
	}
	// plan §4.3:345 mandates `WARN conflict detected: existing=... action=skip`.
	// Mirrors the overwrite branch — even though skip preserves the
	// file, operators still need a WARN so log-based alerting picks
	// up "the pipeline decided not to touch this run".
	if !strings.Contains(stderr, "level=WARN") {
		t.Errorf("stderr missing WARN log under --skip; got: %q", stderr)
	}
	if !strings.Contains(stderr, `msg="conflict detected"`) {
		t.Errorf("stderr missing `conflict detected` under --skip; got: %q", stderr)
	}
	if !strings.Contains(stderr, "action=skip") {
		t.Errorf("stderr missing action=skip; got: %q", stderr)
	}
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read post-skip file: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("--skip touched the existing file; got %q", string(got))
	}
}

// TestSubtitleBranch_OverwriteReplacesExisting locks the third leg of
// plan §4.3: with `--overwrite`, an existing target file must be
// **replaced** by a fresh pipeline run (SourceSubtitle banner, sentinel
// gone, first cue back). Together with [TestSubtitleBranch_SkipReturnsCached]
// and [TestSubtitleBranch_ConflictNoInteractive] this covers all three
// production conflict strategies end-to-end via fakebin.
//
// The seed-then-overwrite dance mirrors the other two conflict tests:
// run once with --overwrite to discover the canonical filename, then
// stomp on it with a sentinel. The *second* --overwrite run then has
// to remove that sentinel by writing fresh CC content — proving the
// pipeline reached formatter.Convert instead of short-circuiting.
func TestSubtitleBranch_OverwriteReplacesExisting(t *testing.T) {
	outDir := integrationTempDir(t, "subtitle-overwrite-out")
	tmpDir := integrationTempDir(t, "subtitle-overwrite-tmp")

	preRunTmp := integrationTempDir(t, "subtitle-overwrite-prerun")
	preCfg := writeIntegrationConfig(t, "subtitle-overwrite-pre", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	preArgs := append([]string(nil), preCfg...)
	preArgs = append(preArgs, "--overwrite", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	if _, _, exit := runCLI(t, preArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + preRunTmp,
	}, preRunTmp); exit != 0 {
		t.Fatalf("pre-run to discover filename failed: exit=%d", exit)
	}
	canonical := findFile(t, outDir, "__BVfake123.txt")
	sentinel := []byte("SENTINEL-SHOULD-BE-OVERWRITTEN")
	if err := os.WriteFile(canonical, sentinel, 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	runCfg := writeIntegrationConfig(t, "subtitle-overwrite-run", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	runArgs := append([]string(nil), runCfg...)
	runArgs = append(runArgs, "--overwrite", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	stdout, stderr, exit := runCLI(t, runArgs, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("expected exit=0 (overwrite), got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// Banner: SourceCached would signal a regression where the
	// overwrite strategy silently degraded to skip. Lock the CC
	// prefix — asr / auto-caption banners would also fail this.
	if !strings.Contains(stdout, "bilibili-txt: cc-subtitle -> ") {
		t.Errorf("stdout missing subtitle banner under --overwrite; got: %q", stdout)
	}
	// plan §4.3:345 mandates `WARN conflict detected: existing=... action=overwrite`.
	// The pipeline emits this via slog through logger.Default(), which
	// writes to stderr under the CLI's default (non-JSON) format. We
	// only assert the destructive-side fields — level=WARN and
	// action=overwrite — so a formatter tweak (e.g. adding
	// component=pipeline) does not break the assertion.
	if !strings.Contains(stderr, "level=WARN") {
		t.Errorf("stderr missing WARN log under --overwrite; got: %q", stderr)
	}
	if !strings.Contains(stderr, `msg="conflict detected"`) {
		t.Errorf("stderr missing `conflict detected` under --overwrite; got: %q", stderr)
	}
	if !strings.Contains(stderr, "action=overwrite") {
		t.Errorf("stderr missing action=overwrite; got: %q", stderr)
	}
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read post-overwrite file: %v", err)
	}
	if bytes.Contains(got, sentinel) {
		t.Errorf("--overwrite left sentinel intact; got %q", string(got))
	}
	// First cue from testdata/sample.srt must land — proves the
	// pipeline actually re-ran subtitle + formatter, not just
	// truncated the file to zero bytes.
	if want := "大家好，欢迎收看本期视频。"; !strings.Contains(string(got), want) {
		t.Errorf("--overwrite output missing sample cue %q; got:\n%s", want, string(got))
	}
}

// TestConflictFlags_OverwriteAndSkipMutuallyExclusive pins plan §4.3's
// CLI-level guardrail: `--overwrite` and `--skip` name opposite
// conflict strategies, so passing both is a usage error, not a
// silent last-wins. The mutex check lives in the RunE closure of
// [newRootCmd] (see the `if overwrite && skip` branch); this test
// guards against a refactor that drops or weakens it (e.g.
// accidentally letting cobra's default flag precedence resolve the
// ambiguity by argv order).
//
// Assertions:
//
//   - exit 1 (usage error, per plan §6 / contracts.md §四; NOT exit 3,
//     which is reserved for pipeline-level ErrConflictUnresolved)
//   - stderr contains the frozen Chinese hint so a translation
//     refresh must consciously update the test
//   - no side-effects under the --output-dir: a failed usage check
//     must not leave a half-written or empty txt behind. (We only
//     assert on outDir; TMPDIR is unreliable as a "did the pipeline
//     run?" probe because pipeline.Run's defer os.RemoveAll wipes
//     its own `bilibili-txt-*` subdir on any exit path.)
func TestConflictFlags_OverwriteAndSkipMutuallyExclusive(t *testing.T) {
	outDir := integrationTempDir(t, "conflict-mutex-out")
	tmpDir := integrationTempDir(t, "conflict-mutex-tmp")

	cfgArgs := writeIntegrationConfig(t, "conflict-mutex", integrationConfig{
		SkipPreflight: true,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--overwrite", "--skip", "--output-dir", outDir,
		"https://www.bilibili.com/video/BV1abcdefghij")
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 1 {
		t.Fatalf("expected exit=1 for --overwrite + --skip mutex, got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// The wrapped message is frozen inside [newRootCmd]'s RunE:
	// `--overwrite 与 --skip 不能同时使用`; main.go's
	// `fmt.Fprintf(os.Stderr, "bilibili-txt: %s\n", err)` prepends
	// the `bilibili-txt: ` shell after cobra returns the error.
	// Substring match tolerates trailing logger output but locks
	// the human-facing half.
	if !strings.Contains(stderr, "--overwrite 与 --skip 不能同时使用") {
		t.Errorf("stderr missing mutex hint; got: %q", stderr)
	}
	// outDir must be empty — a failed usage check must not leave a
	// half-written or empty txt behind.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read outDir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("mutex usage error left files under %s: %v", outDir, names)
	}
}

// findFile locates the (single) file under dir whose basename ends in
// suffix. Fails the test if zero or more than one match — both are
// symptoms of an assertion bug rather than a legitimate ambiguity.
func findFile(t *testing.T, dir, suffix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var hits []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), suffix) {
			hits = append(hits, filepath.Join(dir, e.Name()))
		}
	}
	switch len(hits) {
	case 0:
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no file with suffix %q under %s (got: %v)", suffix, dir, names)
	case 1:
		return hits[0]
	default:
		t.Fatalf("multiple files with suffix %q under %s: %v", suffix, dir, hits)
	}
	return ""
}
