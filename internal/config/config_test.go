package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func fakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// mustDefault wraps Default for tests: on the happy path (HOME set via
// fakeHome, a valid CWD) it returns the config; a HOME/CWD failure is
// fatal, matching the review R6 contract that Default surfaces such
// environment problems instead of pretending to succeed.
func mustDefault(t *testing.T) *Config {
	t.Helper()
	c, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	return c
}

func TestDefault_HasSaneDefaults(t *testing.T) {
	home := fakeHome(t)

	c, err := Default()
	if err != nil {
		t.Fatalf("Default() err=%v (happy path with fakeHome+valid CWD should not error)", err)
	}
	if c == nil {
		t.Fatal("Default returned nil")
	}
	if c.Format != "txt" {
		t.Errorf("Format=%q want %q", c.Format, "txt")
	}
	if c.Debug {
		t.Errorf("Debug=%v want false", c.Debug)
	}
	if c.SkipPreflight {
		t.Errorf("SkipPreflight=%v want false", c.SkipPreflight)
	}
	if c.Logging.Format != "text" {
		t.Errorf("Logging.Format=%q want %q", c.Logging.Format, "text")
	}
	if c.Naming.OnConflict != "ask" {
		t.Errorf("Naming.OnConflict=%q want %q", c.Naming.OnConflict, "ask")
	}
	if !filepath.IsAbs(c.OutputDir) {
		t.Errorf("OutputDir=%q should be absolute", c.OutputDir)
	}
	if !filepath.IsAbs(c.Model) {
		t.Errorf("Model=%q should be absolute", c.Model)
	}
	wantModel := filepath.Join(home, ".local", "share", "whisper", "ggml-large-v3-turbo.bin")
	if c.Model != wantModel {
		t.Errorf("Model=%q want %q", c.Model, wantModel)
	}
	if c.Binaries.Ytdlp != "" || c.Binaries.Ffmpeg != "" || c.Binaries.WhisperCLI != "" {
		t.Errorf("Binaries should be empty by default, got %+v", c.Binaries)
	}
}

// chdirTemp switches the process CWD to a fresh empty temp dir for the
// duration of the test (restored via t.Cleanup), so default-path lookups
// (<CWD>/config/config.yaml) are deterministic. Uses os.Chdir rather than
// t.Chdir (Go 1.24+) to honour the go 1.22 directive in go.mod.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

func TestLoad_EmptyPath_NoDefaultFileReturnsDefault(t *testing.T) {
	fakeHome(t)
	chdirTemp(t) // CWD has no config/ dir

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") err=%v", err)
	}
	def := mustDefault(t)
	if *c != *def {
		t.Errorf("Load(\"\") without default file=%+v want %+v", c, def)
	}
}

func TestLoad_EmptyPath_AutoLoadsDefaultFile(t *testing.T) {
	fakeHome(t)
	dir := chdirTemp(t)

	// Write <CWD>/config/config.yaml with one override; Load("") must
	// pick it up without any explicit path.
	cfgDir := filepath.Join(dir, DefaultConfigDir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	p := filepath.Join(cfgDir, DefaultConfigName)
	if err := os.WriteFile(p, []byte("format: md\n"), 0o644); err != nil {
		t.Fatalf("write default config: %v", err)
	}

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with default file err=%v", err)
	}
	if c.Format != "md" {
		t.Errorf("Format=%q want md (default config/config.yaml not loaded)", c.Format)
	}
}

func TestDefaultConfigPath_UnderCWD(t *testing.T) {
	dir := chdirTemp(t)
	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath err=%v", err)
	}
	// macOS exposes /tmp as a symlink to /private/tmp; os.Getwd() returns
	// the resolved path, so evaluate symlinks on both before comparing.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	want := filepath.Join(realDir, DefaultConfigDir, DefaultConfigName)
	if got != want {
		t.Errorf("DefaultConfigPath=%q want %q", got, want)
	}
}

func TestLoad_MissingFileReturnsError(t *testing.T) {
	fakeHome(t)

	_, err := Load(filepath.Join(t.TempDir(), "no-such-file.yaml"))
	if err == nil {
		t.Fatal("Load(missing) want error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err=%v want fs.ErrNotExist", err)
	}
}

func TestLoad_InvalidYAMLReturnsError(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	p := writeTemp(t, dir, "bad.yaml", "output_dir: [not: closed\n")

	_, err := Load(p)
	if err == nil {
		t.Fatal("Load(bad yaml) want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("err=%v should mention file path", err)
	}
}

func TestLoad_EmptyYAMLEquivalentToDefault(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	p := writeTemp(t, dir, "empty.yaml", "")

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load(empty): %v", err)
	}
	def := mustDefault(t)
	if *c != *def {
		t.Errorf("Load(empty)=%+v want %+v", c, def)
	}
}

func TestLoad_PartialOverride(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()
	body := "format: md\ndebug: true\n"
	p := writeTemp(t, dir, "part.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Format != "md" {
		t.Errorf("Format=%q want md", c.Format)
	}
	if !c.Debug {
		t.Errorf("Debug=%v want true", c.Debug)
	}
	if c.Naming.OnConflict != "ask" {
		t.Errorf("OnConflict=%q want ask (default)", c.Naming.OnConflict)
	}
	wantModel := filepath.Join(home, ".local", "share", "whisper", "ggml-large-v3-turbo.bin")
	if c.Model != wantModel {
		t.Errorf("Model=%q want default %q", c.Model, wantModel)
	}
}

func TestLoad_BinariesEmptyAndWhitespaceMeansUnspecified(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "binaries:\n  ytdlp: \"\"\n  ffmpeg: \"   \"\n  whisper_cli: \"\\t\\n\"\n"
	p := writeTemp(t, dir, "empty-bins.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Binaries.Ytdlp != "" {
		t.Errorf("Ytdlp=%q want empty", c.Binaries.Ytdlp)
	}
	if c.Binaries.Ffmpeg != "" {
		t.Errorf("Ffmpeg=%q want empty (whitespace trimmed)", c.Binaries.Ffmpeg)
	}
	if c.Binaries.WhisperCLI != "" {
		t.Errorf("WhisperCLI=%q want empty (whitespace trimmed)", c.Binaries.WhisperCLI)
	}
}

func TestLoad_BinariesAbsolutePathPreserved(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "binaries:\n  ytdlp: /opt/homebrew/bin/yt-dlp\n"
	p := writeTemp(t, dir, "abs.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Binaries.Ytdlp != "/opt/homebrew/bin/yt-dlp" {
		t.Errorf("Ytdlp=%q want /opt/homebrew/bin/yt-dlp", c.Binaries.Ytdlp)
	}
}

func TestLoad_BinariesRelativePathBecomesAbsolute(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	oldCwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	body := "binaries:\n  ffmpeg: ./tools/ffmpeg\n"
	p := writeTemp(t, dir, "rel.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(c.Binaries.Ffmpeg) {
		t.Errorf("Ffmpeg=%q should be absolute", c.Binaries.Ffmpeg)
	}
	// Normalise both sides via EvalSymlinks to sidestep macOS's
	// /var → /private/var symlink; t.TempDir() returns the /var form,
	// but os.Getwd() resolves to the /private/var canonical form.
	gotReal, err := filepath.EvalSymlinks(c.Binaries.Ffmpeg[:len(c.Binaries.Ffmpeg)-len("/tools/ffmpeg")])
	if err != nil {
		t.Fatalf("EvalSymlinks(got dir): %v", err)
	}
	wantReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(want dir): %v", err)
	}
	if gotReal != wantReal {
		t.Errorf("Ffmpeg dir=%q want %q (via EvalSymlinks)", gotReal, wantReal)
	}
	if !strings.HasSuffix(c.Binaries.Ffmpeg, filepath.Join("tools", "ffmpeg")) {
		t.Errorf("Ffmpeg=%q should end with tools/ffmpeg", c.Binaries.Ffmpeg)
	}
}

func TestLoad_BinariesTildeExpansion(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()
	body := "binaries:\n  whisper_cli: ~/bin/whisper-cli\n"
	p := writeTemp(t, dir, "tilde.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "bin", "whisper-cli")
	if c.Binaries.WhisperCLI != want {
		t.Errorf("WhisperCLI=%q want %q", c.Binaries.WhisperCLI, want)
	}
}

func TestLoad_BinariesTildeBareExpandsToHome(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()
	// YAML unquoted "~" parses as null; quote it so it stays a string.
	body := "binaries:\n  ytdlp: \"~\"\n"
	p := writeTemp(t, dir, "tilde-bare.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Binaries.Ytdlp != home {
		t.Errorf("Ytdlp=%q want %q", c.Binaries.Ytdlp, home)
	}
}

func TestLoad_BinariesBareTildeYAMLNullTreatedAsUnspecified(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	// Unquoted ~ is YAML null. Our raw* struct uses *string, so a null
	// value decodes as a nil pointer → "field absent" → default preserved.
	// This documents that "yaml null == unspecified", which matches our
	// TrimSpace-empty semantics.
	body := "binaries:\n  ytdlp: ~\n"
	p := writeTemp(t, dir, "tilde-null.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Binaries.Ytdlp != "" {
		t.Errorf("Ytdlp=%q want empty (yaml null == unspecified)", c.Binaries.Ytdlp)
	}
}

func TestLoad_BinariesTildeOtherUserRejected(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "binaries:\n  ytdlp: ~otheruser/tools/yt-dlp\n"
	p := writeTemp(t, dir, "tilde-user.yaml", body)

	_, err := Load(p)
	if err == nil {
		t.Fatal("Load should reject ~user syntax, got nil error")
	}
	if !strings.Contains(err.Error(), "~") {
		t.Errorf("err=%v should reference ~ syntax", err)
	}
	if !strings.Contains(err.Error(), "ytdlp") {
		t.Errorf("err=%v should reference the offending field name", err)
	}
}

func TestLoad_ModelAndOutputDirTildeExpansion(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()
	body := "output_dir: ~/scripts\nmodel: ~/models/whisper.bin\n"
	p := writeTemp(t, dir, "paths.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OutputDir != filepath.Join(home, "scripts") {
		t.Errorf("OutputDir=%q want %q", c.OutputDir, filepath.Join(home, "scripts"))
	}
	if c.Model != filepath.Join(home, "models", "whisper.bin") {
		t.Errorf("Model=%q want %q", c.Model, filepath.Join(home, "models", "whisper.bin"))
	}
}

func TestLoad_HomeUnavailableReturnsError(t *testing.T) {
	t.Setenv("HOME", "")
	// On macOS, os.UserHomeDir also considers PWD/whoami, but with HOME cleared
	// under a test setenv, the function should return an error.
	dir := t.TempDir()
	body := "binaries:\n  ytdlp: ~/bin/yt-dlp\n"
	p := writeTemp(t, dir, "no-home.yaml", body)

	if _, err := Load(p); err == nil {
		t.Fatal("Load should error when HOME cannot be resolved for ~ expansion")
	}
}

func TestMerge_CLINonEmptyOverridesBase(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	cli := &Config{
		OutputDir: "/tmp/out",
		Format:    "md",
		Model:     "/tmp/model.bin",
		Naming:    Naming{OnConflict: "overwrite"},
	}

	got := Merge(base, cli)
	if got == base || got == cli {
		t.Fatalf("Merge should return a new pointer, base=%p cli=%p got=%p", base, cli, got)
	}
	if got.OutputDir != "/tmp/out" {
		t.Errorf("OutputDir=%q want /tmp/out", got.OutputDir)
	}
	if got.Format != "md" {
		t.Errorf("Format=%q want md", got.Format)
	}
	if got.Model != "/tmp/model.bin" {
		t.Errorf("Model=%q want /tmp/model.bin", got.Model)
	}
	if got.Naming.OnConflict != "overwrite" {
		t.Errorf("OnConflict=%q want overwrite", got.Naming.OnConflict)
	}
}

// TestMerge_IgnoresCLIBinaries pins the contract from plan §4.6 that
// binary paths have no CLI flag and therefore must not be smuggled in
// via cli.Binaries. If someone adds a --ytdlp-path flag later this test
// will fail loudly and force them to revisit the plan.
func TestMerge_IgnoresCLIBinaries(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	base.Binaries = Binaries{Ytdlp: "/base/yt", Ffmpeg: "/base/ff", WhisperCLI: "/base/wc"}
	cli := &Config{Binaries: Binaries{Ytdlp: "/cli/yt", Ffmpeg: "/cli/ff", WhisperCLI: "/cli/wc"}}

	got := Merge(base, cli)
	if got.Binaries.Ytdlp != "/base/yt" || got.Binaries.Ffmpeg != "/base/ff" || got.Binaries.WhisperCLI != "/base/wc" {
		t.Errorf("cli.Binaries should be ignored, got=%+v", got.Binaries)
	}
}

func TestMerge_EmptyCLIKeepsBase(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	base.Format = "md"
	base.Binaries.Ytdlp = "/base/yt"

	got := Merge(base, &Config{})
	if got.Format != "md" {
		t.Errorf("Format=%q want md (from base)", got.Format)
	}
	if got.Binaries.Ytdlp != "/base/yt" {
		t.Errorf("Ytdlp=%q want /base/yt (from base)", got.Binaries.Ytdlp)
	}
	if got.Naming.OnConflict != base.Naming.OnConflict {
		t.Errorf("Naming.OnConflict=%q want %q", got.Naming.OnConflict, base.Naming.OnConflict)
	}
}

func TestMerge_DebugORLogic(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	base.Debug = true

	got := Merge(base, &Config{Debug: false})
	if !got.Debug {
		t.Errorf("Debug=%v want true (base already true, cli false must not clear)", got.Debug)
	}

	base.Debug = false
	got = Merge(base, &Config{Debug: true})
	if !got.Debug {
		t.Errorf("Debug=%v want true (cli turns on)", got.Debug)
	}

	base.SkipPreflight = true
	got = Merge(base, &Config{})
	if !got.SkipPreflight {
		t.Errorf("SkipPreflight=%v want true (base already true, cli empty must not clear)", got.SkipPreflight)
	}
	got = Merge(&Config{}, &Config{SkipPreflight: true})
	if !got.SkipPreflight {
		t.Errorf("SkipPreflight=%v want true (cli turns on)", got.SkipPreflight)
	}
}

func TestMerge_NilBaseFallsBackToDefault(t *testing.T) {
	fakeHome(t)
	got := Merge(nil, &Config{Format: "srt"})
	if got.Format != "srt" {
		t.Errorf("Format=%q want srt", got.Format)
	}
	if got.Naming.OnConflict != "ask" {
		t.Errorf("OnConflict=%q want ask (from Default)", got.Naming.OnConflict)
	}
}

func TestMerge_NilCLIReturnsCopyOfBase(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	base.Format = "md"

	got := Merge(base, nil)
	if got == base {
		t.Fatalf("Merge(base, nil) must return a copy, got same pointer")
	}
	if *got != *base {
		t.Errorf("Merge(base, nil)=%+v want equal to base=%+v", got, base)
	}
}

func TestMerge_DoesNotMutateInputs(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	baseCopy := *base
	cli := &Config{Format: "md", Binaries: Binaries{Ytdlp: "/x"}}
	cliCopy := *cli

	_ = Merge(base, cli)
	if *base != baseCopy {
		t.Errorf("Merge mutated base: got %+v want %+v", base, baseCopy)
	}
	if *cli != cliCopy {
		t.Errorf("Merge mutated cli: got %+v want %+v", cli, cliCopy)
	}
}

func TestLoad_UnknownFieldRejected(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "output_dir: /tmp\nformatt: md\n" // typo: formatt
	p := writeTemp(t, dir, "typo.yaml", body)

	_, err := Load(p)
	if err == nil {
		t.Fatal("Load should reject unknown top-level fields to catch typos")
	}
}

func TestLoad_YAMLTypeMismatchWraps(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "debug: not-a-bool\n"
	p := writeTemp(t, dir, "type.yaml", body)

	_, err := Load(p)
	if err == nil {
		t.Fatal("Load should error on yaml type mismatch")
	}
	if !strings.Contains(err.Error(), "type.yaml") {
		t.Errorf("err=%v should mention file path for locality", err)
	}
}

// TestLoad_OutputDirRelativeBecomesAbsolute mirrors the binaries-side
// test: relative output_dir in yaml must be resolved via CWD to match
// plan §4.2 which says path fields are absolute after Load.
func TestLoad_OutputDirRelativeBecomesAbsolute(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	oldCwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	// os.Getwd() returns the canonical path on macOS (/private/var/...)
	// while t.TempDir() returns /var/... — normalize both via
	// EvalSymlinks so the equality below is stable.
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	body := "output_dir: ./out\nmodel: ./model.bin\n"
	p := writeTemp(t, dir, "rel.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OutputDir != filepath.Join(cwd, "out") {
		t.Errorf("OutputDir=%q want %q", c.OutputDir, filepath.Join(cwd, "out"))
	}
	if c.Model != filepath.Join(cwd, "model.bin") {
		t.Errorf("Model=%q want %q", c.Model, filepath.Join(cwd, "model.bin"))
	}
}

// TestLoad_OutputDirWhitespaceKeepsDefault ensures whitespace-only path
// scalars (`output_dir: "   "`) don't clobber Default(). Symmetric with
// the binaries whitespace test.
func TestLoad_OutputDirWhitespaceKeepsDefault(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()

	body := "output_dir: \"   \"\nmodel: \"\t\"\n"
	p := writeTemp(t, dir, "ws.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OutputDir != mustDefault(t).OutputDir {
		t.Errorf("OutputDir=%q should keep Default", c.OutputDir)
	}
	wantModel := filepath.Join(home, ".local", "share", "whisper", "ggml-large-v3-turbo.bin")
	if c.Model != wantModel {
		t.Errorf("Model=%q want default %q", c.Model, wantModel)
	}
}

// TestLoad_NamingOnConflictEmptyKeepsDefault pins the same "empty ⇒
// keep default" rule for naming.on_conflict.
func TestLoad_NamingOnConflictEmptyKeepsDefault(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	body := "naming:\n  on_conflict: \"\"\n"
	p := writeTemp(t, dir, "onc.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Naming.OnConflict != "ask" {
		t.Errorf("OnConflict=%q want ask (default)", c.Naming.OnConflict)
	}
}

// TestLoad_NestedUnknownFieldRejected mirrors the top-level unknown
// field test but for nested maps (`binaries.foobar`). Guards against a
// yaml.v3 KnownFields quirk where nested maps could bypass strict mode.
func TestLoad_NestedUnknownFieldRejected(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	body := "binaries:\n  ytdlp: /usr/bin/yt-dlp\n  foobar: /nope\n"
	p := writeTemp(t, dir, "nested.yaml", body)

	_, err := Load(p)
	if err == nil {
		t.Fatal("Load should reject unknown nested fields")
	}
}

// TestFinalize_DebugEnabledPromotesLevel pins the plan §4.5 linkage:
// `debug: true` implicitly raises logging.level to "debug"
// unless the user has already picked something explicit.
func TestFinalize_DebugEnabledPromotesLevel(t *testing.T) {
	c := &Config{Debug: true}
	var warn bytes.Buffer
	c.Finalize(&warn)
	if c.Logging.Level != "debug" {
		t.Errorf("Level=%q want debug", c.Logging.Level)
	}
	if c.Logging.Format != "text" {
		t.Errorf("Format=%q want text (default)", c.Logging.Format)
	}
	if warn.Len() != 0 {
		t.Errorf("warn should be silent when level unset, got %q", warn.String())
	}
}

// TestFinalize_ExplicitLevelWinsAndWarns locks the mismatch-warning
// contract: explicit logging.level survives, but we shout at the user
// on stderr so the surprise doesn't silently swallow debug events.
func TestFinalize_ExplicitLevelWinsAndWarns(t *testing.T) {
	c := &Config{Debug: true, Logging: Logging{Level: "warn"}}
	var warn bytes.Buffer
	c.Finalize(&warn)
	if c.Logging.Level != "warn" {
		t.Errorf("Level=%q want warn (explicit wins)", c.Logging.Level)
	}
	if !strings.Contains(warn.String(), "debug=true") ||
		!strings.Contains(warn.String(), "logging.level=\"warn\"") {
		t.Errorf("warn missing expected message, got %q", warn.String())
	}
}

// TestFinalize_ExplicitLevelDebugNoWarn: matching values must NOT trip
// the mismatch warning. Otherwise every `debug: true` + `logging.level:
// debug` yaml would spam stderr on startup.
func TestFinalize_ExplicitLevelDebugNoWarn(t *testing.T) {
	c := &Config{Debug: true, Logging: Logging{Level: "debug"}}
	var warn bytes.Buffer
	c.Finalize(&warn)
	if c.Logging.Level != "debug" {
		t.Errorf("Level=%q want debug", c.Logging.Level)
	}
	if warn.Len() != 0 {
		t.Errorf("warn should be silent when explicit level matches, got %q", warn.String())
	}
}

// TestFinalize_DefaultsWithoutDebug: no debug flag → default level info,
// no warning, format defaults to text.
func TestFinalize_DefaultsWithoutDebug(t *testing.T) {
	c := &Config{}
	var warn bytes.Buffer
	c.Finalize(&warn)
	if c.Logging.Level != "info" {
		t.Errorf("Level=%q want info", c.Logging.Level)
	}
	if c.Logging.Format != "text" {
		t.Errorf("Format=%q want text", c.Logging.Format)
	}
	if warn.Len() != 0 {
		t.Errorf("warn should be silent, got %q", warn.String())
	}
}

// TestFinalize_NilWarnSafe: warn=nil must not panic. Callers may pass
// nil when they don't care about the mismatch message (e.g. tests).
func TestFinalize_NilWarnSafe(t *testing.T) {
	c := &Config{Debug: true, Logging: Logging{Level: "warn"}}
	c.Finalize(nil)
	if c.Logging.Level != "warn" {
		t.Errorf("Level=%q want warn", c.Logging.Level)
	}
}

// TestValidate_AcceptsCanonicalValues pins the enum contract; call sites
// upstream can rely on these four Format values and three Logging.Format
// / four Logging.Level values.
func TestValidate_AcceptsCanonicalValues(t *testing.T) {
	c := &Config{Format: "txt", Logging: Logging{Format: "text", Level: "info"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: unexpected err %v", err)
	}
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		c.Logging.Level = lvl
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(level=%q) err=%v", lvl, err)
		}
	}
	for _, f := range []string{"txt", "md", "srt"} {
		c.Format = f
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(format=%q) err=%v", f, err)
		}
	}
	for _, lf := range []string{"text", "json"} {
		c.Logging.Format = lf
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(logging.format=%q) err=%v", lf, err)
		}
	}
}

// TestValidate_RejectsUnknownEnums is the negative twin: any
// out-of-range enum must be rejected with a message that mentions the
// field so the user can locate the offending yaml key.
func TestValidate_RejectsUnknownEnums(t *testing.T) {
	cases := []struct {
		name   string
		cfg    *Config
		expect string
	}{
		{
			name:   "format",
			cfg:    &Config{Format: "docx", Logging: Logging{Format: "text", Level: "info"}},
			expect: "format",
		},
		{
			name:   "logging.format",
			cfg:    &Config{Format: "txt", Logging: Logging{Format: "xml", Level: "info"}},
			expect: "logging.format",
		},
		{
			name:   "logging.level",
			cfg:    &Config{Format: "txt", Logging: Logging{Format: "text", Level: "loud"}},
			expect: "logging.level",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate should reject %+v", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("err=%v should mention %q", err, tc.expect)
			}
		})
	}
}

// TestLoad_DebugAndSkipPreflightScalars exercises the top-level scalar
// forms `debug: true` and `skip_preflight: true`.
func TestLoad_DebugAndSkipPreflightScalars(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "debug: true\nskip_preflight: true\n"
	p := writeTemp(t, dir, "debug-scalar.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Debug {
		t.Errorf("Debug=%v want true", c.Debug)
	}
	if !c.SkipPreflight {
		t.Errorf("SkipPreflight=%v want true", c.SkipPreflight)
	}
}

// TestLoad_LoggingBlock ensures the logging.* keys map into the new
// Logging sub-struct with the same normalisation rules (trim + lower)
// as the rest of the config layer.
func TestLoad_LoggingBlock(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "logging:\n  level: DEBUG\n  format: JSON\n  file: ./logs/main.log\n  debug_file: ./logs/debug.log\n"
	p := writeTemp(t, dir, "logging.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("Logging.Level=%q want debug (lower-cased)", c.Logging.Level)
	}
	if c.Logging.Format != "json" {
		t.Errorf("Logging.Format=%q want json (lower-cased)", c.Logging.Format)
	}
	if !filepath.IsAbs(c.Logging.File) {
		t.Errorf("Logging.File=%q should be absolute", c.Logging.File)
	}
	if !filepath.IsAbs(c.Logging.DebugFile) {
		t.Errorf("Logging.DebugFile=%q should be absolute", c.Logging.DebugFile)
	}
}

// ---------------------------------------------------------------------------
// Auth (login-by-default via yt-dlp --cookies-from-browser)
// ---------------------------------------------------------------------------

// TestDefault_AuthCookiesFromBrowserIsChrome pins the product decision
// captured in the plan: login is on by default and the browser is
// Chrome. If someone edits Default() and drops this, the CLI would
// silently fall back to guest access — this test is the guardrail.
func TestDefault_AuthCookiesFromBrowserIsChrome(t *testing.T) {
	fakeHome(t)
	c, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if c.Auth.CookiesFromBrowser != "chrome" {
		t.Errorf("Auth.CookiesFromBrowser=%q want %q",
			c.Auth.CookiesFromBrowser, "chrome")
	}
}

// TestLoad_AuthOverride_Firefox verifies the user can point login at a
// different browser without touching any other field. This is the
// core "opt into another browser" path.
func TestLoad_AuthOverride_Firefox(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "auth:\n  cookies_from_browser: firefox\n"
	p := writeTemp(t, dir, "auth-ff.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.CookiesFromBrowser != "firefox" {
		t.Errorf("Auth.CookiesFromBrowser=%q want firefox", c.Auth.CookiesFromBrowser)
	}
}

// TestLoad_AuthOverride_None locks the documented escape hatch: setting
// `auth.cookies_from_browser: none` in the yaml is how a user disables
// login without deleting the block. The downstream downloader tests
// pair with this to prove no `--cookies-from-browser` flag is emitted.
func TestLoad_AuthOverride_None(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "auth:\n  cookies_from_browser: none\n"
	p := writeTemp(t, dir, "auth-none.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.CookiesFromBrowser != "none" {
		t.Errorf("Auth.CookiesFromBrowser=%q want %q (literal, downloader interprets)",
			c.Auth.CookiesFromBrowser, "none")
	}
}

// TestLoad_AuthMissingBlockKeepsDefault documents the "yaml silent about
// auth = keep default chrome" behaviour that guarantees existing users
// upgrade seamlessly into login-by-default without editing their
// config file.
func TestLoad_AuthMissingBlockKeepsDefault(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "format: md\n" // no auth: block at all
	p := writeTemp(t, dir, "no-auth.yaml", body)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.CookiesFromBrowser != "chrome" {
		t.Errorf("Auth.CookiesFromBrowser=%q want default %q",
			c.Auth.CookiesFromBrowser, "chrome")
	}
}

// TestLoad_AuthEmptyValueKeepsDefault matches the whitespace-tolerant
// treatment used elsewhere in the config layer (see BinariesEmpty…
// tests): an empty string or all-whitespace value must NOT wipe out
// the chrome default — otherwise `auth:\n  cookies_from_browser: ""`
// would silently break login on upgrade.
func TestLoad_AuthEmptyValueKeepsDefault(t *testing.T) {
	fakeHome(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty_string", "auth:\n  cookies_from_browser: \"\"\n"},
		{"whitespace", "auth:\n  cookies_from_browser: \"   \"\n"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeTemp(t, dir, tc.name+".yaml", tc.body)
			c, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.Auth.CookiesFromBrowser != "chrome" {
				t.Errorf("Auth.CookiesFromBrowser=%q want default %q",
					c.Auth.CookiesFromBrowser, "chrome")
			}
		})
	}
}

// TestLoad_AuthUnknownFieldRejected inherits the strict-yaml contract
// checked by TestLoad_NestedUnknownFieldRejected: a typo like
// `cookies_from_broswer` must fail loud, not silently disable login.
func TestLoad_AuthUnknownFieldRejected(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "auth:\n  cookies_from_broswer: chrome\n" // typo intentional
	p := writeTemp(t, dir, "auth-typo.yaml", body)

	if _, err := Load(p); err == nil {
		t.Fatal("Load should reject unknown auth.* fields (typo protection)")
	}
}

// ---------------------------------------------------------------------------
// Server (无参数启动时的本地 Web 界面)
// ---------------------------------------------------------------------------

// TestServerDefaults pins the built-in server defaults: random port and
// the auto-open-browser knob on.
func TestServerDefaults(t *testing.T) {
	fakeHome(t)
	c, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if c.Server.Port != 0 {
		t.Errorf("Server.Port=%d want 0 (random port)", c.Server.Port)
	}
	if !c.Server.OpenBrowser {
		t.Errorf("Server.OpenBrowser=%v want true", c.Server.OpenBrowser)
	}
}

// TestServerLoad covers both the explicit-override path (both fields
// written, including the bool flipped to false) and the partial-override
// path (only port written; the true-by-default bool must survive because
// rawServer uses *bool).
func TestServerLoad(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	full := "server:\n  port: 8787\n  open_browser: false\n"
	c, err := Load(writeTemp(t, dir, "full.yaml", full))
	if err != nil {
		t.Fatalf("Load(full server block): %v", err)
	}
	if c.Server.Port != 8787 {
		t.Errorf("Server.Port=%d want 8787", c.Server.Port)
	}
	if c.Server.OpenBrowser {
		t.Errorf("Server.OpenBrowser=%v want false (explicit)", c.Server.OpenBrowser)
	}

	portOnly := "server:\n  port: 8787\n"
	c2, err := Load(writeTemp(t, dir, "port-only.yaml", portOnly))
	if err != nil {
		t.Fatalf("Load(port-only server block): %v", err)
	}
	if c2.Server.Port != 8787 {
		t.Errorf("Server.Port=%d want 8787", c2.Server.Port)
	}
	if !c2.Server.OpenBrowser {
		t.Errorf("Server.OpenBrowser=%v want true (default preserved)", c2.Server.OpenBrowser)
	}
}

// TestServerUnknownFieldRejected inherits KnownFields strictness for
// the nested server map: a typo (`prt`) must fail loudly rather than
// silently starting the UI on an unexpected port.
func TestServerUnknownFieldRejected(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	body := "server:\n  prt: 1\n"
	p := writeTemp(t, dir, "server-typo.yaml", body)

	if _, err := Load(p); err == nil {
		t.Fatal("Load should reject unknown server.* fields (typo protection)")
	}
}

// TestServerMerge locks the value-semantics merge for the server block:
// a zero-valued overlay leaves base untouched; non-zero / explicitly
// true overlay values win. Today no CLI flag produces a Server overlay,
// so this pins the intended contract ahead of that work.
func TestServerMerge(t *testing.T) {
	fakeHome(t)
	base := mustDefault(t)
	base.Server = Server{Port: 9000, OpenBrowser: false}

	got := Merge(base, &Config{})
	if got.Server != base.Server {
		t.Errorf("empty overlay changed Server: got=%+v want=%+v", got.Server, base.Server)
	}

	overlay := &Config{Server: Server{Port: 8080, OpenBrowser: true}}
	got = Merge(base, overlay)
	if got.Server.Port != 8080 {
		t.Errorf("Server.Port=%d want 8080 (non-zero overlay wins)", got.Server.Port)
	}
	if !got.Server.OpenBrowser {
		t.Errorf("Server.OpenBrowser=%v want true (overlay turns on)", got.Server.OpenBrowser)
	}
}

// TestValidatePort checks the server port range: 0 (random) and
// 1..65535 are legal; anything outside is rejected.
func TestValidatePort(t *testing.T) {
	for _, p := range []int{0, 1, 8080, 65535} {
		c := &Config{Format: "txt", Logging: Logging{Format: "text", Level: "info"},
			Server: Server{Port: p}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(port=%d) unexpected err=%v", p, err)
		}
	}
	for _, p := range []int{-1, 70000} {
		c := &Config{Format: "txt", Logging: Logging{Format: "text", Level: "info"},
			Server: Server{Port: p}}
		err := c.Validate()
		if err == nil {
			t.Errorf("Validate(port=%d) want error, got nil", p)
		} else if !strings.Contains(err.Error(), "server.port") {
			t.Errorf("Validate(port=%d) err=%v should mention server.port", p, err)
		}
	}
}
