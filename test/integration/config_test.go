//go:build !windows

package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemOnlyPath is a deliberately minimal PATH used by the yaml-wins
// case. It keeps /usr/bin and /bin so `#!/usr/bin/env bash` at the top
// of every fakebin script still resolves (macOS ships bash at
// /bin/bash; the fakebin scripts only need bash + coreutils, both of
// which live under those two dirs). yt-dlp / ffmpeg / whisper-cli are
// intentionally absent — the whole point of Case A is proving the
// yaml-supplied path is honored even when $PATH cannot find the tool.
const systemOnlyPath = "/usr/bin:/bin"

// writeYAML dumps lines to <dir>/config.yaml and returns the file path.
// Kept intentionally string-based (no yaml.Marshal) so the fixture is
// readable inline and mirrors what a user would hand-write.
func writeYAML(t *testing.T, dir string, lines []string) string {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml %s: %v", path, err)
	}
	return path
}

// TestConfigYAML_ExplicitBinaryPathWinsOverPATH covers plan §4.6 tier
// one: an absolute `binaries.ytdlp` in config.yaml MUST be honored end
// to end even when the same tool cannot be located on $PATH.
//
// The test pins two independent invariants in a single run:
//
//  1. Binary resolution honours yaml: PATH is scrubbed to /usr/bin:/bin
//     (no yt-dlp anywhere) so the ONLY way the pipeline can spawn the
//     fake yt-dlp is via the absolute path baked into the yaml. If the
//     wire-up regressed to "use PATH regardless", the child spawn
//     would fail with exec.ErrNotFound and the exit code would flip
//     to 1.
//  2. YAML is actually consulted: `output_dir` is set only in yaml
//     (no --output-dir on the CLI). If yaml were silently ignored the
//     transcript would land in the built-in default (./transcripts
//     relative to the process cwd, which is `tmpDir` here) and the
//     findFile assertion below would fail.
//
// `skip_preflight: true` is baked into the yaml because preflight
// also validates the whisper model file (≥100 MiB), which the fake
// environment cannot satisfy. §4.6 relocated the old
// `--skip-preflight` flag into this config knob; the yaml→binary
// plumbing under test lives inside the pipeline layer regardless.
func TestConfigYAML_ExplicitBinaryPathWinsOverPATH(t *testing.T) {
	cfgDir := integrationTempDir(t, "config-yaml-explicit-cfg")
	outDir := integrationTempDir(t, "config-yaml-explicit-out")
	tmpDir := integrationTempDir(t, "config-yaml-explicit-tmp")

	fb := fakebinDir(t)
	cfgPath := writeYAML(t, cfgDir, []string{
		"output_dir: " + outDir,
		"format: txt",
		"skip_preflight: true",
		"binaries:",
		"  ytdlp: " + filepath.Join(fb, "yt-dlp"),
	})

	stdout, stderr, exit := runCLI(t, []string{
		"--no-interactive",
		"--config", cfgPath,
		"https://www.bilibili.com/video/BV1abcdefghij",
	}, []string{
		"PATH=" + systemOnlyPath,
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bilibili-txt: cc-subtitle -> ") {
		t.Errorf("stdout missing subtitle success banner; got: %q", stdout)
	}
	// yaml-only `output_dir` must have won — the produced file lives
	// there, not in some ambient default.
	findFile(t, outDir, "__BVfake123.txt")
}

// TestConfigYAML_EmptyBinaryFallsBackToPATH covers plan §4.6 tier two
// and the T-4.2 spec's explicit "binaries.ytdlp: ” → PATH fallback"
// requirement.
//
// yaml intentionally sets every binaries.* to the empty string. That
// pins two branches:
//
//   - The load path preserves "unspecified" semantics (an empty string
//     round-trips through config.Load as "" and does not accidentally
//     shadow the default).
//   - The pipeline wire-up therefore hands YtdlpDownloader.Binary=""
//     and exec.LookPath resolves via $PATH, which we've prepended
//     with fakebin. If the fallback broke, the child spawn would
//     surface as exit=1 well before any file could land in outDir.
//
// output_dir is still yaml-only, same "yaml was actually consulted"
// witness as the explicit-path case.
func TestConfigYAML_EmptyBinaryFallsBackToPATH(t *testing.T) {
	cfgDir := integrationTempDir(t, "config-yaml-fallback-cfg")
	outDir := integrationTempDir(t, "config-yaml-fallback-out")
	tmpDir := integrationTempDir(t, "config-yaml-fallback-tmp")

	cfgPath := writeYAML(t, cfgDir, []string{
		"output_dir: " + outDir,
		"format: txt",
		"skip_preflight: true",
		"binaries:",
		`  ytdlp: ""`,
		`  ffmpeg: ""`,
		`  whisper_cli: ""`,
	})

	stdout, stderr, exit := runCLI(t, []string{
		"--no-interactive",
		"--config", cfgPath,
		"https://www.bilibili.com/video/BV1abcdefghij",
	}, []string{
		prependPath(fakebinDir(t)),
		"TMPDIR=" + tmpDir,
	}, tmpDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bilibili-txt: cc-subtitle -> ") {
		t.Errorf("stdout missing subtitle success banner; got: %q", stdout)
	}
	findFile(t, outDir, "__BVfake123.txt")
}

// TestConfigYAML_DefaultPathAutoLoaded proves the project-local default
// config path `<CWD>/config/config.yaml` is consulted WITHOUT an explicit
// --config flag (docs/review-2026-09-08.md F1).
//
// The CLI is run with workDir as its CWD. We create workDir/config/config.yaml
// with a yaml-only `output_dir` and a yaml-only ytdlp binary path; PATH is
// scrubbed so the fake yt-dlp is reachable only via the absolute path in the
// yaml. A green run (exit 0 + transcript landing in the yaml output_dir) can
// only happen if the default-path file was actually discovered and loaded.
func TestConfigYAML_DefaultPathAutoLoaded(t *testing.T) {
	workDir := integrationTempDir(t, "config-default-cwd")
	outDir := integrationTempDir(t, "config-default-out")

	fb := fakebinDir(t)
	// Default location: <CWD>/config/config.yaml
	cfgDir := filepath.Join(workDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir default config dir: %v", err)
	}
	writeYAML(t, cfgDir, []string{
		"output_dir: " + outDir,
		"format: txt",
		"skip_preflight: true",
		"binaries:",
		"  ytdlp: " + filepath.Join(fb, "yt-dlp"),
	})

	// Note: no --config flag passed.
	stdout, stderr, exit := runCLI(t, []string{
		"--no-interactive",
		"https://www.bilibili.com/video/BV1abcdefghij",
	}, []string{
		"PATH=" + systemOnlyPath,
	}, workDir)

	if exit != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bilibili-txt: cc-subtitle -> ") {
		t.Errorf("stdout missing subtitle success banner; got: %q", stdout)
	}
	findFile(t, outDir, "__BVfake123.txt")
}
