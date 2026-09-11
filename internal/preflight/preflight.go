// Package preflight validates that all external tools and filesystem
// resources bilibili-txt depends on are present before the pipeline
// runs. It never mutates the environment and never prompts the user;
// it just returns a slice of [CheckResult] that main can decide how to
// react to.
//
// Design (plan §4.2):
//
//   - Six checks, always run and returned in the same order:
//     1. yt-dlp presence
//     2. yt-dlp version floor
//     3. ffmpeg presence
//     4. whisper-cli presence
//     5. whisper model file (existence + size floor)
//     6. output directory writability
//   - Every failure is collected — Run never stops early — so users
//     fix all environment problems at once instead of playing
//     whack-a-mole with sequential errors.
//   - Binary path resolution follows a two-tier fallback:
//     (1) [config.Binaries] value already normalised by [config.Load]
//     (~ expanded, relative → absolute). Non-empty means "user
//     supplied a specific path". Set-but-invalid is a HARD error;
//     we deliberately do not fall back to $PATH because doing so
//     silently would violate the "if I filled it in, use it or
//     tell me why not" contract.
//     (2) Empty ⇒ [exec.LookPath] against the ambient $PATH.
//   - All external interaction (exec, os.Stat, exec.LookPath) is
//     behind [Runner] fields so the entire package is unit-testable
//     without the real binaries installed. Zero-value [Runner] wires
//     up production behaviour.
package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"bilibili-txt/internal/config"
)

// defaultMinYtdlpVersion is the floor plan §4.2 pins. yt-dlp version
// strings are `YYYY.MM.DD` and sort lexicographically — no semver
// parsing needed.
const defaultMinYtdlpVersion = "2025.01.01"

// defaultMinModelSize is plan §4.2's 100 MiB floor for the whisper
// model file. Anything smaller is almost certainly a truncated /
// half-downloaded artifact.
const defaultMinModelSize int64 = 100 * 1024 * 1024

// CheckResult is the atomic result of a single preflight check.
//
// Name identifies the check in reports (e.g. "yt-dlp", "whisper model").
// OK is true iff the check passed. When OK, Message is a short summary
// (usually `<name>=<version> (<source>)` where <source> ∈ {yaml, PATH});
// when !OK, Message is a user-facing fix hint. Detail is opaque per-
// check debug context (raw command output, stat errors, etc.) surfaced
// only in --debug mode.
type CheckResult struct {
	Name    string
	OK      bool
	Message string
	Detail  string
}

// Runner encapsulates preflight's external dependencies so tests can
// substitute deterministic fakes. The zero value is usable and drives
// real os.exec / os.Stat calls.
type Runner struct {
	// LookPath overrides [exec.LookPath]. nil ⇒ real exec.LookPath.
	LookPath func(name string) (string, error)
	// VersionOf runs `<path> --version` and returns the trimmed first
	// non-empty line. nil ⇒ real exec.Command implementation.
	VersionOf func(path string) (string, error)
	// StatFile overrides [os.Stat]. nil ⇒ real os.Stat.
	StatFile func(name string) (fs.FileInfo, error)

	// MinYtdlpVersion overrides the built-in floor. Empty ⇒ default.
	// Exported for tests that want to lower/raise the calendar-date
	// floor without patching package-level state.
	MinYtdlpVersion string
	// MinModelSize overrides the built-in whisper-model size floor in
	// bytes. Zero ⇒ default (100 MiB). Exported for tests.
	MinModelSize int64
}

// Run performs all six preflight checks against cfg using a zero-value
// [Runner]. See [Runner.Run] for details.
func Run(cfg *config.Config) []CheckResult {
	return (&Runner{}).Run(cfg)
}

// Run performs all six preflight checks against cfg. Results are
// returned in a stable order matching plan §4.2; callers may pass the
// slice to [AllOK] or [FormatFailures].
//
// cfg must not be nil; a nil config is a programmer error (config.Load
// / config.Default always returns a non-nil value). Defensive guard
// below avoids a raw nil-panic in production so debugging is easier.
func (r *Runner) Run(cfg *config.Config) []CheckResult {
	if cfg == nil {
		return []CheckResult{{
			Name:    "internal",
			OK:      false,
			Message: "preflight.Run 收到 nil config（调用方 bug）",
		}}
	}
	results := make([]CheckResult, 0, 6)

	// #1 yt-dlp
	ytdlp := r.resolveBinary("yt-dlp", cfg.Binaries.Ytdlp, "binaries.ytdlp", "brew install yt-dlp")
	results = append(results, ytdlp.result)

	// #2 yt-dlp version — only meaningful if #1 succeeded
	results = append(results, r.checkYtdlpVersion(ytdlp))

	// #3 ffmpeg
	ffmpeg := r.resolveBinary("ffmpeg", cfg.Binaries.Ffmpeg, "binaries.ffmpeg", "brew install ffmpeg")
	results = append(results, ffmpeg.result)

	// #4 whisper-cli
	whisper := r.resolveBinary("whisper-cli", cfg.Binaries.WhisperCLI, "binaries.whisper_cli", "brew install whisper-cpp")
	results = append(results, whisper.result)

	// #5 whisper model
	results = append(results, r.checkModel(cfg.Model))

	// #6 output dir
	results = append(results, r.checkOutputDir(cfg.OutputDir))

	return results
}

// resolvedBinary is the outcome of one binary-path resolution: the
// user-facing CheckResult plus the resolved absolute path (for
// downstream checks such as version probing).
type resolvedBinary struct {
	result CheckResult
	// path is the absolute resolved path, empty when result.OK == false.
	path string
	// source is "yaml" or "PATH" — recorded in result.Message when OK.
	source string
}

// resolveBinary implements the two-tier fallback described in plan
// §4.2. yamlKey is the yaml field name (e.g. "binaries.ytdlp") that we
// echo into error messages so users know which knob to turn. installCmd
// is the "brew install …" hint appended to the missing-in-PATH error.
func (r *Runner) resolveBinary(name, yamlValue, yamlKey, installCmd string) resolvedBinary {
	res := CheckResult{Name: name}
	trimmed := strings.TrimSpace(yamlValue)

	if trimmed != "" {
		// yaml-supplied → strict validation, NO fallback.
		info, err := r.stat(trimmed)
		if err != nil {
			res.Message = fmt.Sprintf("config.yaml %s 指向的路径无法访问: %s (%v)", yamlKey, trimmed, err)
			res.Detail = err.Error()
			return resolvedBinary{result: res}
		}
		if info.IsDir() {
			res.Message = fmt.Sprintf("config.yaml %s 指向的是一个目录，不是可执行文件: %s", yamlKey, trimmed)
			return resolvedBinary{result: res}
		}
		if info.Mode()&0o111 == 0 {
			res.Message = fmt.Sprintf("config.yaml %s 指向的文件不可执行: %s", yamlKey, trimmed)
			return resolvedBinary{result: res}
		}
		res.OK = true
		res.Message = fmt.Sprintf("%s=%s (yaml)", name, trimmed)
		return resolvedBinary{result: res, path: trimmed, source: "yaml"}
	}

	// Unspecified → PATH fallback.
	found, err := r.lookPath(name)
	if err != nil || strings.TrimSpace(found) == "" {
		res.Message = fmt.Sprintf("缺少 %s，请 %s 或在 config.yaml 的 %s 指定路径", name, installCmd, yamlKey)
		if err != nil {
			res.Detail = err.Error()
		}
		return resolvedBinary{result: res}
	}
	// Even PATH hits get an executability sanity check — exec.LookPath
	// on some platforms will happily return a file that lost its +x bit
	// between installs.
	info, statErr := r.stat(found)
	if statErr != nil {
		res.Message = fmt.Sprintf("$PATH 中的 %s 无法访问: %s (%v)", name, found, statErr)
		res.Detail = statErr.Error()
		return resolvedBinary{result: res}
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		res.Message = fmt.Sprintf("$PATH 中的 %s 不是可执行文件: %s", name, found)
		return resolvedBinary{result: res}
	}
	res.OK = true
	res.Message = fmt.Sprintf("%s=%s (PATH)", name, found)
	return resolvedBinary{result: res, path: found, source: "PATH"}
}

// ytdlpVersionRe extracts a `YYYY.MM.DD` token. yt-dlp --version prints
// exactly one line of that shape, but packaged builds sometimes prefix
// "yt-dlp version 2025.10.22". We scan line-by-line and prefer lines
// mentioning "yt-dlp" first, then fall back to any line whose only
// non-space content is a date — this rejects noise like
// `WARNING: pip 24.0 (2024.03.14) available` that would otherwise be
// picked up by a naive whole-string search.
var ytdlpVersionRe = regexp.MustCompile(`\b(\d{4}\.\d{2}\.\d{2})\b`)

// extractYtdlpVersion picks the yt-dlp release date from raw stdout/
// stderr text. Returns "" when no plausible version token is found.
func extractYtdlpVersion(raw string) string {
	lines := strings.Split(raw, "\n")
	// Pass 1: prefer a line explicitly mentioning yt-dlp.
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.Contains(strings.ToLower(l), "yt-dlp") {
			if m := ytdlpVersionRe.FindStringSubmatch(l); m != nil {
				return m[1]
			}
		}
	}
	// Pass 2: a line whose entire content is just the version token.
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if ytdlpVersionRe.MatchString(l) && ytdlpVersionRe.FindString(l) == l {
			return l
		}
	}
	// Pass 3: last-ditch — first date-shaped token in the first
	// non-empty line only (avoids stderr noise on later lines).
	for _, line := range lines {
		if l := strings.TrimSpace(line); l != "" {
			if m := ytdlpVersionRe.FindStringSubmatch(l); m != nil {
				return m[1]
			}
			break
		}
	}
	return ""
}

// checkYtdlpVersion probes `<yt-dlp> --version` and compares against
// the configured floor. If the earlier presence check failed, this
// check auto-fails with a note referencing the parent failure so
// FormatFailures doesn't confuse users into thinking the version is
// separately wrong.
func (r *Runner) checkYtdlpVersion(yt resolvedBinary) CheckResult {
	res := CheckResult{Name: "yt-dlp version"}
	if !yt.result.OK {
		res.Message = "yt-dlp 未解析，无法检查版本（见 yt-dlp 项失败原因）"
		return res
	}
	raw, err := r.versionOf(yt.path)
	if err != nil {
		res.Message = fmt.Sprintf("获取 yt-dlp 版本失败: %v", err)
		res.Detail = err.Error()
		return res
	}
	got := extractYtdlpVersion(raw)
	if got == "" {
		res.Message = fmt.Sprintf("无法解析 yt-dlp 版本号: %q", strings.TrimSpace(raw))
		res.Detail = raw
		return res
	}
	floor := strings.TrimSpace(r.MinYtdlpVersion)
	if floor == "" {
		floor = defaultMinYtdlpVersion
	}
	// yt-dlp version strings are calendar dates in fixed-width
	// `YYYY.MM.DD` form; lexicographic comparison is correct.
	if got < floor {
		res.Message = fmt.Sprintf("yt-dlp 版本 %s 过旧（要求 ≥ %s），请 brew upgrade yt-dlp", got, floor)
		res.Detail = raw
		return res
	}
	res.OK = true
	res.Message = fmt.Sprintf("yt-dlp 版本 %s (≥ %s)", got, floor)
	return res
}

// checkModel validates the whisper model file: it must exist, be a
// regular file, and be at least MinModelSize bytes (guards against
// truncated downloads).
func (r *Runner) checkModel(path string) CheckResult {
	res := CheckResult{Name: "whisper model"}
	path = strings.TrimSpace(path)
	if path == "" {
		res.Message = "模型路径为空，请在 config.yaml 的 model 或 --model 中指定"
		return res
	}
	info, err := r.stat(path)
	if err != nil {
		res.Message = fmt.Sprintf(
			"模型文件不存在: %s\n请参考 docs/research.md §6.1 下载:\n\n  mkdir -p ~/.local/share/whisper && curl -L -o ~/.local/share/whisper/ggml-large-v3-turbo.bin https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin\n",
			path,
		)
		res.Detail = err.Error()
		return res
	}
	if info.IsDir() {
		res.Message = fmt.Sprintf("模型路径指向的是一个目录: %s", path)
		return res
	}
	floor := r.MinModelSize
	if floor <= 0 {
		floor = defaultMinModelSize
	}
	if info.Size() < floor {
		res.Message = fmt.Sprintf(
			"模型文件 size %d 过小 (< %d bytes)，可能未下载完整: %s\n请参考 docs/research.md §6.1 重新下载",
			info.Size(), floor, path,
		)
		return res
	}
	res.OK = true
	res.Message = fmt.Sprintf("model=%s size=%dMiB", path, info.Size()/(1024*1024))
	return res
}

// checkOutputDir ensures the output directory exists (creating it if
// needed) and is writable. Writability is verified by actually creating
// a small probe file — os.Stat cannot tell us that on macOS/Linux
// (`w` bit visibility isn't reliable enough to rely on).
func (r *Runner) checkOutputDir(dir string) CheckResult {
	res := CheckResult{Name: "output dir"}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		res.Message = "输出目录为空，请在 config.yaml 的 output_dir 或 --output-dir 中指定"
		return res
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Message = fmt.Sprintf("输出目录不可创建：%s，err=%v", dir, err)
		res.Detail = err.Error()
		return res
	}
	probe, err := os.CreateTemp(dir, ".bilibili-txt-preflight-*")
	if err != nil {
		res.Message = fmt.Sprintf("输出目录不可写：%s，err=%v", dir, err)
		res.Detail = err.Error()
		return res
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	res.OK = true
	res.Message = fmt.Sprintf("output_dir=%s (writable)", dir)
	return res
}

// AllOK reports whether every check in results passed.
func AllOK(results []CheckResult) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

// FormatFailures renders a human-readable, multi-line report of the
// failing checks in results, one per bullet. Passing checks are
// omitted. Returns "" when there are no failures — callers should not
// print empty banners.
func FormatFailures(results []CheckResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.OK {
			continue
		}
		b.WriteString("  ✗ ")
		b.WriteString(r.Name)
		b.WriteString(": ")
		b.WriteString(r.Message)
		b.WriteByte('\n')
	}
	return b.String()
}

// stat calls r.StatFile if set, otherwise falls back to os.Stat.
func (r *Runner) stat(name string) (fs.FileInfo, error) {
	if r.StatFile != nil {
		return r.StatFile(name)
	}
	return os.Stat(name)
}

// lookPath calls r.LookPath if set, otherwise falls back to
// [exec.LookPath].
func (r *Runner) lookPath(name string) (string, error) {
	if r.LookPath != nil {
		return r.LookPath(name)
	}
	return exec.LookPath(name)
}

// versionOf calls r.VersionOf if set, otherwise shells out to
// `<path> --version` and returns the trimmed first non-empty line of
// combined stdout+stderr (some tools print to stderr). The default
// implementation caps runtime at 10 s so a wedged binary (DNS timeout,
// interpreter cold-start, locale lock) can't hang preflight forever.
func (r *Runner) versionOf(path string) (string, error) {
	if r.VersionOf != nil {
		return r.VersionOf(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		if buf.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(buf.String()))
		}
		return "", err
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s, nil
		}
	}
	return "", errors.New("empty --version output")
}
