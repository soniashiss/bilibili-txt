//go:build !windows

package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModelPath is a bogus absolute path baked into every ASR
// integration test's per-test config.yaml (via
// integrationConfig.Model — §4.6 moved the old `--model` flag into
// the yaml). It exists to defeat runASRBranch's P2-2 fast-fail
// (which rejects empty cfg.Model) without coupling the suite to the
// developer's real config.yaml — the fakebin whisper-cli stub
// accepts any `-m` argument and never opens the file.
//
// Coupling risk: if a future pipeline change stat()s cfg.Model before
// invoking whisper-cli, every test here goes red at once. Fix by
// pointing the const at an on-disk placeholder file under testdata/.
const fakeModelPath = "/nonexistent/whisper-model.bin"

// BVIDs referenced in this file live in one of two fixtures:
//
//   - nosubBVID appears in [testdata/metadata-nosub.json] and is the
//     only value the fake yt-dlp emits for URLs matching `BVnosub*`
//     (see the case in [testdata/fakebin/yt-dlp]). Every "happy /
//     keep-intermediate / failure-injection" test uses this so the
//     branch selector lands in runASRBranch without --force-asr.
//   - urlBVIDForceASR is a synthetic BV id embedded in the URL for
//     the force-asr test only. It does NOT match `BVnosub*`, so the
//     fake yt-dlp falls back to [testdata/metadata.json] (which
//     advertises subtitles as fakeMetadataBVID). The URL BV winds up
//     on any `.m4a` / `.zh-CN.srt` filename fakebin writes (extract_bv
//     reads the URL), while the pipeline uses the metadata BVID for
//     `.wav` / `.srt`. Keeping both constants named makes the split
//     legible at the callsite.
const (
	nosubBVID          = "BVnosub999"
	urlBVIDForceASR    = "BV1abcdefghij"
	fakeMetadataBVID   = "BVfake123"
	fakeVideoURLPrefix = "https://www.bilibili.com/video/"
)

// asrCue1 / asrCue2 pin the two cues from testdata/sample-asr.srt.
// Keeping them as package-level constants means a fixture update
// only requires editing one place, and every assertion documents its
// intent at the callsite ("cue 1", "cue 2").
const (
	asrCue1 = "大家好 欢迎收看本期视频"
	asrCue2 = "ASR识别出的文本"
	// ccCue1 mirrors testdata/sample.srt cue 1 in its punctuated
	// form. Used as a *negative* guard: if this string ever leaks
	// into an ASR-branch output body, the pipeline silently
	// consulted the CC subtitle fixture.
	ccCue1 = "大家好，欢迎收看本期视频。"
)

// assertNoOutputTxt fails the test if outDir contains any *.txt file
// (or any file at all, given the pipeline only writes txt into it).
// Used by every failure-injection test to prove the CLI does NOT
// leave a partial / stale output behind when the pipeline errors
// mid-flight — a regression that first wrote to outPath and then
// failed later would otherwise pass the "exit=1 + stderr prefix"
// bar while silently corrupting user directories.
func assertNoOutputTxt(t *testing.T, outDir string) {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("assertNoOutputTxt: read %s: %v", outDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".txt") {
			t.Errorf("assertNoOutputTxt: failure path left %s under %s; pipeline must not write output on error", e.Name(), outDir)
		}
	}
}

// assertTmpDirCleaned walks tmpDir and fails if any
// `bilibili-txt-<bvid>-*` sub-directory is still present. That prefix
// is minted by [pipeline.Run] via os.MkdirTemp; a `defer os.RemoveAll`
// wipes it on both success and failure paths (see the cleanup block
// around pipeline.go L278-293). Absence proves the defer fired even
// on the error path — a bug that gates cleanup on `err == nil` would
// leak the whole subtree and this walk would catch it.
func assertTmpDirCleaned(t *testing.T, tmpDir string) {
	t.Helper()
	err := filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), "bilibili-txt-") {
			t.Errorf("assertTmpDirCleaned: leftover pipeline tmp dir %s (parent=%s); Run's defer must remove even on error", info.Name(), path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assertTmpDirCleaned: walk %s: %v", tmpDir, err)
	}
}

// assertStderrLineHasPrefix locks the whole CLI-top-level error line
// shape, not just the pipeline wrap. contracts.md §四 + cli/root.go
// promise every error hits stderr as `bilibili-txt: <wrapped err>\n`
// via [handlePipelineError]; if a future refactor drops the
// `bilibili-txt: ` shell or reorders the wraps, a substring-only
// assertion would silently pass. HasPrefix on the trimmed line is
// the tightest form that still tolerates trailing log lines from
// the debug sink appended after the fatal.
func assertStderrLineHasPrefix(t *testing.T, stderr, wantPrefix string) {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), wantPrefix) {
			return
		}
	}
	t.Errorf("assertStderrLineHasPrefix: no line begins with %q\nfull stderr:\n%s", wantPrefix, stderr)
}

// TestASRBranch_HappyPath drives the CLI end-to-end against the fake
// yt-dlp/ffmpeg/whisper-cli shims for a video whose metadata advertises
// zero subtitle tracks (BVnosub999 → testdata/metadata-nosub.json with
// empty `subtitles` + `automatic_captions` maps).
//
// The pipeline must:
//
//   - short-circuit to the ASR branch (no --force-asr needed)
//   - emit exit 0 with the `bilibili-txt: asr -> …` success banner so
//     users can tell the ASR fallback fired without grepping logs
//   - land a `<slug>__BVnosub999.txt` file under --output-dir whose body
//     contains sample-asr.srt's ASR-flavoured cues (space-separated, no
//     punctuation — deliberately distinct from sample.srt so a silent
//     fallback to the subtitle branch would fail this assertion)
//
// The per-test yaml sets `skip_preflight: true` to bypass the
// model-file existence check (fakebin whisper-cli accepts any `-m`
// path) and an explicit `model:` path so runASRBranch's P2-2 fast-fail
// (empty cfg.Model) cannot kick in even if a stray config.yaml on the
// developer's machine sets model to "". Both knobs previously lived on
// argv (`--skip-preflight`, `--model`) and §4.6 relocated them into
// the yaml.
func TestASRBranch_HappyPath(t *testing.T) {
	outDir := integrationTempDir(t, "asr-happy-out")
	tmpDir := integrationTempDir(t, "asr-happy-tmp")

	url := fakeVideoURLPrefix + nosubBVID

	cfgArgs := writeIntegrationConfig(t, "asr-happy", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
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
	// pipeline.SourceASR. A silent fallback to SourceSubtitle would
	// still exit 0 but with a different banner, so pin the exact
	// prefix.
	if !strings.Contains(stdout, "bilibili-txt: asr -> ") {
		t.Errorf("stdout missing ASR success banner; got: %q", stdout)
	}

	// Output filename: the pipeline runs Slug against
	// metadata-nosub.json's title ("No Subtitle Video — 无字幕测试样例")
	// and appends __<bvid>.txt. We only assert on the BVID suffix
	// so this test does not couple to the slugger's exact filter list.
	produced := findFile(t, outDir, "__"+nosubBVID+".txt")
	body, err := os.ReadFile(produced)
	if err != nil {
		t.Fatalf("read produced file %s: %v", produced, err)
	}
	// sample-asr.srt has two cues. Assert both landed so a
	// formatter regression that drops trailing cues (e.g. an off-by
	// -one in the cue-merge loop) is caught here, not deferred to
	// the formatter unit tests.
	if !strings.Contains(string(body), asrCue1) {
		t.Errorf("txt output missing ASR cue 1 %q; got:\n%s", asrCue1, string(body))
	}
	if !strings.Contains(string(body), asrCue2) {
		t.Errorf("txt output missing ASR cue 2 %q; got:\n%s", asrCue2, string(body))
	}
	// Negative guard: reject the subtitle-branch fixture's exact
	// punctuated form. Even if the ASR cue happened to be a
	// substring, the punctuated variant appearing would mean the
	// wrong srt got parsed.
	if strings.Contains(string(body), ccCue1) {
		t.Errorf("txt output leaked subtitle-branch fixture (%q); ASR branch must not consult sample.srt", ccCue1)
	}
}

// TestASRBranch_ForceASROverridesSubtitles verifies that --force-asr
// bypasses the subtitle discovery step even for a video whose metadata
// advertises real subtitle tracks (fakeMetadataBVID has zh-CN in
// testdata/metadata.json).
//
// Assertions:
//
//   - exit 0 with the ASR banner (NOT the subtitle banner) despite the
//     metadata having a zh-CN track
//   - output body carries the ASR-flavoured cues, not the subtitle fixture
//   - the temp tree contains **no** `*.zh-CN.srt` — this is the
//     negative sanity check that proves runSubtitleBranch was never
//     entered (a bug that first ran the subtitle branch and only
//     then decided to switch to ASR would leave the CC srt behind
//     because fakebin/yt-dlp writes it before returning).
//
// The negative walk only bites when the pipeline's own tmp sub-dir
// survives the run, so this test passes --keep-intermediate. Without
// it, [pipeline.Run]'s defer os.RemoveAll wipes the `bilibili-txt-*`
// sub-directory (where fakebin/yt-dlp writes `<bvid>.zh-CN.srt` via
// --paths) *before* Run returns, and the walk would see an empty
// TMPDIR whether or not subtitle-branch fired — silently defanging
// the assertion. --keep-intermediate is the smallest lever that
// short-circuits the defer without touching production code.
func TestASRBranch_ForceASROverridesSubtitles(t *testing.T) {
	outDir := integrationTempDir(t, "asr-force-out")
	tmpDir := integrationTempDir(t, "asr-force-tmp")

	url := fakeVideoURLPrefix + urlBVIDForceASR

	cfgArgs := writeIntegrationConfig(t, "asr-force", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--force-asr", "--keep-intermediate",
		"--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bilibili-txt: asr -> ") {
		t.Errorf("stdout missing ASR banner under --force-asr; got: %q", stdout)
	}
	// Under --force-asr against metadata.json the metadata's own
	// BVID (fakeMetadataBVID) wins over the URL slug, so the
	// filename is `__<fakeMetadataBVID>.txt`.
	produced := findFile(t, outDir, "__"+fakeMetadataBVID+".txt")
	body, err := os.ReadFile(produced)
	if err != nil {
		t.Fatalf("read produced file %s: %v", produced, err)
	}
	if !strings.Contains(string(body), asrCue1) {
		t.Errorf("txt output missing ASR cue 1 under --force-asr; got:\n%s", string(body))
	}
	if !strings.Contains(string(body), asrCue2) {
		t.Errorf("txt output missing ASR cue 2 under --force-asr; got:\n%s", string(body))
	}
	if strings.Contains(string(body), ccCue1) {
		t.Errorf("--force-asr leaked subtitle-branch fixture (%q); force-asr must not consult sample.srt", ccCue1)
	}

	// Negative sanity: walk the (retained) temp tree and prove no
	// `*.zh-CN.srt` was materialised. runSubtitleBranch is the only
	// code path that spawns fakebin's yt-dlp --write-subs, so the
	// absence of that artefact under `bilibili-txt-*` proves
	// force-asr short-circuited before subtitle discovery.
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".zh-CN.srt") {
			t.Errorf("--force-asr materialised a CC subtitle file at %s; subtitle branch must be skipped", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tmpDir for negative sanity: %v", err)
	}
}

// TestASRBranch_KeepIntermediate_KeepsAllArtefacts pins the P3 leftover
// documented in T-3.3: with --keep-intermediate the pipeline should NOT
// tear down the tmp dir, so the three ASR-branch by-products
// (`<bvid>.m4a` from yt-dlp, `<bvid>.wav` from ffmpeg, `<bvid>.srt`
// from whisper-cli) must remain reachable under TMPDIR.
//
// The test walks the whole tmp tree (pipeline mints a pkg-owned
// sub-directory inside TMPDIR, so a shallow scan would miss the
// artefacts) and asserts the three concrete filenames — matching by
// suffix alone would accept a stray `foo.m4a` and let a bug in the
// naming layer slip through. Body content is not re-checked here —
// the happy-path test already proves the srt was consumed correctly.
func TestASRBranch_KeepIntermediate_KeepsAllArtefacts(t *testing.T) {
	outDir := integrationTempDir(t, "asr-keep-out")
	tmpDir := integrationTempDir(t, "asr-keep-tmp")

	url := fakeVideoURLPrefix + nosubBVID

	cfgArgs := writeIntegrationConfig(t, "asr-keep", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--keep-intermediate",
		"--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// Banner assertion — without this, a future refactor that
	// short-circuits to SourceCached (which is exit=0 + output
	// file present) would silently pass this test. Keep-intermediate
	// only makes sense once we've proven the ASR branch actually ran.
	if !strings.Contains(stdout, "bilibili-txt: asr -> ") {
		t.Errorf("stdout missing ASR banner under --keep-intermediate; got: %q", stdout)
	}
	// Sanity: the output file must still land — --keep-intermediate
	// only touches the tmp cleanup path, not the terminal write.
	_ = findFile(t, outDir, "__"+nosubBVID+".txt")

	// Collect the concrete artefact filenames under tmpDir. The
	// pipeline creates its own `bilibili-txt-*` sub-directory (see
	// [os.MkdirTemp] usage in [pipeline.Run]), so a non-recursive
	// os.ReadDir would only see that sub-directory.
	//
	// We match on exact basenames rather than suffixes so a bug that
	// wrote e.g. `stray.m4a` alongside a missing `<bvid>.m4a` does
	// not accidentally satisfy the assertion. All three names share
	// nosubBVID because in this test URL BV == metadata BV — the
	// fakebin m4a uses the URL BV (fakebin/yt-dlp:81,157) while wav
	// / srt use meta.BVID (pipeline.go:440,452), and the two happen
	// to coincide for `BVnosub*`. If you ever refactor
	// metadata-nosub.json's `id`, keep it aligned with the URL BV or
	// switch this test to construct expectations from both sources.
	var (
		wantM4A = nosubBVID + ".m4a"
		wantWav = nosubBVID + ".wav"
		wantSRT = nosubBVID + ".srt"
	)
	haveM4A, haveWav, haveSRT := false, false, false
	err := filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		switch info.Name() {
		case wantM4A:
			haveM4A = true
		case wantWav:
			haveWav = true
		case wantSRT:
			haveSRT = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tmpDir: %v", err)
	}
	if !haveM4A || !haveWav || !haveSRT {
		t.Errorf("--keep-intermediate must preserve %s/%s/%s trio under %s; got m4a=%v wav=%v srt=%v",
			wantM4A, wantWav, wantSRT, tmpDir, haveM4A, haveWav, haveSRT)
	}
}

// TestASRBranch_YtdlpAudioFailure verifies contracts §四 exit-code
// mapping when the very first exec of the ASR branch (yt-dlp -x) fails.
// The pipeline wraps the error as `pipeline: download audio: %w`, and
// [handlePipelineError] maps a plain (non-sentinel) error to exit 1.
//
// The env var `FAKEBIN_FAIL_YTDLP=audio` is the documented failure
// hook in [testdata/fakebin/yt-dlp] and turns the -x invocation into
// an exit-1 + stderr line. Metadata mode is untouched, so the pipeline
// makes it through metadata + into runASRBranch before failing.
func TestASRBranch_YtdlpAudioFailure(t *testing.T) {
	outDir := integrationTempDir(t, "asr-ytdlp-fail-out")
	tmpDir := integrationTempDir(t, "asr-ytdlp-fail-tmp")

	url := fakeVideoURLPrefix + nosubBVID

	cfgArgs := writeIntegrationConfig(t, "asr-ytdlp-fail", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
		"FAKEBIN_FAIL_YTDLP=audio",
	}, tmpDir)

	if exit != 1 {
		t.Fatalf("expected exit=1 for yt-dlp audio failure, got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// The wrapped-error prefix is frozen: cli/root.go prints
	// `bilibili-txt: <err>\n` (see [handlePipelineError]) and
	// [pipeline.runASRBranch] wraps download-audio failures as
	// `pipeline: download audio: %w`. Lock the *full* line prefix so
	// a refactor that drops either the CLI shell or the pipeline
	// verb is caught here.
	assertStderrLineHasPrefix(t, stderr, "bilibili-txt: pipeline: download audio:")
	// Contract-hardening: failure paths must not leave a stale
	// output file behind (users expect an atomic "either the txt
	// exists and is complete, or nothing was written").
	assertNoOutputTxt(t, outDir)
	// And Run's `defer os.RemoveAll(tmpDir)` must fire even on the
	// error path (T-3.3 P2-3 change made this an invariant).
	assertTmpDirCleaned(t, tmpDir)
}

// TestASRBranch_FfmpegFailure verifies the transcode-step error path
// (`pipeline: transcode: …` → exit 1). Fakebin ffmpeg respects
// FAKEBIN_FAIL_FFMPEG=1 by exiting 1 after the arg-scan sanity check,
// so yt-dlp (metadata + -x) both succeed before ffmpeg trips the wire.
func TestASRBranch_FfmpegFailure(t *testing.T) {
	outDir := integrationTempDir(t, "asr-ffmpeg-fail-out")
	tmpDir := integrationTempDir(t, "asr-ffmpeg-fail-tmp")

	url := fakeVideoURLPrefix + nosubBVID

	cfgArgs := writeIntegrationConfig(t, "asr-ffmpeg-fail", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
		"FAKEBIN_FAIL_FFMPEG=1",
	}, tmpDir)

	if exit != 1 {
		t.Fatalf("expected exit=1 for ffmpeg failure, got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	assertStderrLineHasPrefix(t, stderr, "bilibili-txt: pipeline: transcode:")
	assertNoOutputTxt(t, outDir)
	assertTmpDirCleaned(t, tmpDir)
}

// TestASRBranch_WhisperFailure covers the final leg: whisper-cli exit 1
// after ffmpeg has already produced the wav. Wrapped as
// `pipeline: transcribe: …` and mapped to exit 1.
func TestASRBranch_WhisperFailure(t *testing.T) {
	outDir := integrationTempDir(t, "asr-whisper-fail-out")
	tmpDir := integrationTempDir(t, "asr-whisper-fail-tmp")

	url := fakeVideoURLPrefix + nosubBVID

	cfgArgs := writeIntegrationConfig(t, "asr-whisper-fail", integrationConfig{
		SkipPreflight: true,
		Model:         fakeModelPath,
		Format:        "txt",
	})
	args := append([]string(nil), cfgArgs...)
	args = append(args, "--no-interactive", "--output-dir", outDir, url)
	stdout, stderr, exit := runCLI(t, args, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
		"FAKEBIN_FAIL_WHISPER=1",
	}, tmpDir)

	if exit != 1 {
		t.Fatalf("expected exit=1 for whisper failure, got exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	assertStderrLineHasPrefix(t, stderr, "bilibili-txt: pipeline: transcribe:")
	assertNoOutputTxt(t, outDir)
	assertTmpDirCleaned(t, tmpDir)
}
