// Package config manages user-facing configuration for bilibili-txt.
//
// A single [Config] value is assembled from three sources with strict
// precedence: CLI flags > YAML file > built-in defaults. External binary
// paths under [Binaries] are exempt from CLI flags per plan §4.6; they
// come only from YAML or $PATH resolution at preflight time.
//
// The package is deliberately I/O light: it reads one YAML file, resolves
// user-relative paths (~, relative to CWD, TrimSpace normalisation for
// empty-as-unspecified semantics), and returns a value. Everything else
// (existence checks, executable-bit validation, $PATH fallback) is left
// to [preflight].
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"bilibili-txt/internal/pathx"
)

// Binaries holds resolved paths to the external CLI tools bilibili-txt
// orchestrates. An empty string means "unspecified" (fall back to $PATH
// in preflight). A non-empty string is treated as a user-provided path;
// preflight then enforces existence + executability.
type Binaries struct {
	Ytdlp      string `yaml:"ytdlp"`
	Ffmpeg     string `yaml:"ffmpeg"`
	WhisperCLI string `yaml:"whisper_cli"`
}

// Naming captures user-visible file-naming policy. The zero value is not
// valid; use [Default] to seed sane defaults.
type Naming struct {
	// OnConflict picks the file-conflict strategy: ask | overwrite | skip | abort.
	// Values are validated at CLI parse time; config layer stores whatever
	// yaml supplies verbatim.
	OnConflict string `yaml:"on_conflict"`
}

// Auth groups credentials-adjacent knobs. Today the only knob is which
// browser yt-dlp should borrow its Bilibili cookies from — enabling
// login-gated content (大会员 / 私享稿件 / 粉丝专属). The value maps 1:1
// to yt-dlp's `--cookies-from-browser BROWSER` argument.
//
// Semantics of CookiesFromBrowser:
//   - default (via [Default]): "chrome" — the user asked for
//     login-by-default with Chrome as the default browser.
//   - empty / whitespace in yaml: treated as "unspecified", the
//     [Default] value survives (same "empty ⇒ keep default" contract
//     used by [Config.Format] and [Naming.OnConflict]).
//   - "none" (case-insensitive): sentinel that means "explicitly
//     disabled". The downloader layer detects it and omits the
//     `--cookies-from-browser` flag entirely, matching pre-login
//     behaviour for users who don't want a Keychain prompt.
//   - anything else: passed through verbatim to yt-dlp. We do not
//     enumerate the accepted browsers here because yt-dlp is the
//     authority; a bad value surfaces as a yt-dlp startup error, not
//     a config-layer validation error.
type Auth struct {
	CookiesFromBrowser string `yaml:"cookies_from_browser"`
}

// Logging groups the log-related knobs that used to be individual CLI
// flags. Level is authoritative for slog level filtering; Format picks
// text vs json. File / DebugFile are optional write-fan-out targets
// (empty means "stderr only" for File, "auto-derived under ./logs/"
// when the debug branch is on for DebugFile).
type Logging struct {
	Level     string `yaml:"level"`
	Format    string `yaml:"format"`
	File      string `yaml:"file"`
	DebugFile string `yaml:"debug_file"`
}

// Config is the fully resolved configuration passed into the pipeline.
// It is a plain value type on purpose: copies are cheap and there is no
// hidden state.
type Config struct {
	OutputDir string  `yaml:"output_dir"`
	Format    string  `yaml:"format"`
	Model     string  `yaml:"model"`
	Logging   Logging `yaml:"logging"`
	// Debug is a plain scalar (`debug: true|false`). It toggles the
	// debug branch: write the per-run external-stderr log file and
	// (unless logging.level is explicitly set) raise the log level to
	// debug. Mirrored by the CLI --debug flag.
	Debug bool `yaml:"debug"`
	// SkipPreflight skips the pre-run dependency checks (yt-dlp /
	// ffmpeg / whisper-cli / model / output dir). It is config-only
	// (no CLI flag) and lives at the top level rather than under
	// `debug:` because skipping preflight is a startup-behaviour knob,
	// not a logging one.
	SkipPreflight bool     `yaml:"skip_preflight"`
	Binaries      Binaries `yaml:"binaries"`
	Naming        Naming   `yaml:"naming"`
	Auth          Auth     `yaml:"auth"`
}

// Default returns the built-in defaults. It always returns a non-nil pointer
// on the happy path. The default OutputDir is CWD/transcripts made absolute;
// the default Model is $HOME/.local/share/whisper/ggml-large-v3-turbo.bin.
//
// HOME / CWD lookup failure surfaces as a non-nil error rather than falling
// back to root-anchored strings like "/transcripts" or
// "/ggml-large-v3-turbo.bin" (review R6). In practice both lookups always
// succeed on macOS/Linux under a normal shell; a failure here signals an
// exotic environment (chroot without /proc/self/cwd, HOME unset in a
// container, etc.) where the tool cannot pick a safe default anyway, and the
// operator must supply explicit `--output-dir` / `model:` values.
func Default() (*Config, error) {
	c := &Config{
		Format:  "txt",
		Logging: Logging{Format: "text"},
		Naming:  Naming{OnConflict: "ask"},
		Auth:    Auth{CookiesFromBrowser: "chrome"},
	}

	home, err := userHome()
	if err != nil {
		return nil, fmt.Errorf("config: resolve HOME for default model path: %w", err)
	}
	c.Model = filepath.Join(home, ".local", "share", "whisper", "ggml-large-v3-turbo.bin")

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("config: resolve CWD for default output dir: %w", err)
	}
	c.OutputDir = filepath.Join(cwd, "transcripts")
	return c, nil
}

// DefaultConfigDir / DefaultConfigName define the project-local default
// config location: a `config/` directory under the process's current
// working directory, containing `config.yaml`. The config file is
// OPTIONAL — bilibili-txt is fully usable with built-in defaults, so a
// missing default file is silently ignored (see [Load]). This is a
// deliberate departure from the XDG (`~/.config/bilibili-txt/`) sketch
// in early plan docs: keeping config next to where the tool runs makes
// per-project setup obvious and avoids surprising global state.
const (
	DefaultConfigDir  = "config"
	DefaultConfigName = "config.yaml"
)

// DefaultConfigPath returns the absolute default config path,
// `<CWD>/config/config.yaml`. It errors only if the CWD cannot be
// resolved, which should not happen in normal operation.
func DefaultConfigPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("config: resolve cwd for default config path: %w", err)
	}
	return filepath.Join(cwd, DefaultConfigDir, DefaultConfigName), nil
}

// Load reads a YAML config file and returns a [Config] merged over
// [Default]. Path resolution:
//
//   - path != "" (the hidden --config flag): that exact file is loaded.
//     A missing file is an error that wraps [fs.ErrNotExist] — the user
//     explicitly pointed at a path, so silence would hide a typo.
//   - path == "" (normal invocation): the default project-local path
//     from [DefaultConfigPath] (`<CWD>/config/config.yaml`) is used. If
//     that file does not exist, [Default] is returned unchanged (config
//     is optional). Any other stat error (permission, …) still surfaces.
//
// Parsing semantics (see applyConfigFile):
//   - malformed YAML: error mentions the file path for locality.
//   - unknown top-level keys: rejected (typo guard, KnownFields=true).
//   - binaries entries: TrimSpace; empty ⇒ preserved as "" (means "unspecified");
//     non-empty ⇒ ~ / ~user expansion, then relative → absolute via CWD.
//     ~user (other user) is explicitly rejected — we do not want to shell
//     out to /etc/passwd lookups from config-loading time.
//   - output_dir / model: same tilde-expand + abs-path rules as binaries.
//   - debug: a plain scalar bool (`debug: true|false`).
//   - skip_preflight: a plain scalar bool at the TOP level.
//
// The returned pointer is safe to mutate; it is a fresh allocation.
func Load(path string) (*Config, error) {
	c, err := Default()
	if err != nil {
		return nil, err
	}

	explicit := path != ""
	if !explicit {
		p, err := DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		// Default (optional) config missing → fall back to built-in
		// defaults. An explicit --config path that is missing, or any
		// non-ENOENT error, is a real failure.
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		return nil, err
	}

	return applyConfigFile(c, path, raw)
}

// applyConfigFile decodes raw YAML bytes into a bare [rawConfig] (so
// zero-valued fields don't clobber [Default]) and overlays the set
// fields onto c. Split out of [Load] so both the default-path and
// explicit-path branches share one parsing/merge implementation.
func applyConfigFile(c *Config, path string, raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	// Decode into a bare struct so zero-valued yaml fields don't clobber
	// our Defaults (e.g. missing `format:` must not become "").
	var file rawConfig
	if err := dec.Decode(&file); err != nil {
		// A truly empty file yields io.EOF from yaml.v3; treat that as
		// "no overrides" rather than an error.
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if file.OutputDir != nil {
		p, err := normalizePath(*file.OutputDir, "output_dir")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if p != "" {
			c.OutputDir = p
		}
	}
	if file.Format != nil {
		// Whitespace-only Format is treated as "unspecified" so the built-in
		// default ("txt") survives — same contract as [normalizeBinary].
		if v := strings.TrimSpace(*file.Format); v != "" {
			c.Format = v
		}
	}
	if file.Model != nil {
		p, err := normalizePath(*file.Model, "model")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if p != "" {
			c.Model = p
		}
	}
	if file.Logging != nil {
		if file.Logging.Level != nil {
			if v := strings.TrimSpace(strings.ToLower(*file.Logging.Level)); v != "" {
				c.Logging.Level = v
			}
		}
		if file.Logging.Format != nil {
			if v := strings.TrimSpace(strings.ToLower(*file.Logging.Format)); v != "" {
				c.Logging.Format = v
			}
		}
		if file.Logging.File != nil {
			p, err := normalizePath(*file.Logging.File, "logging.file")
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			c.Logging.File = p
		}
		if file.Logging.DebugFile != nil {
			p, err := normalizePath(*file.Logging.DebugFile, "logging.debug_file")
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			c.Logging.DebugFile = p
		}
	}
	if file.Debug != nil {
		c.Debug = *file.Debug
	}
	if file.SkipPreflight != nil {
		c.SkipPreflight = *file.SkipPreflight
	}
	if file.Binaries != nil {
		if file.Binaries.Ytdlp != nil {
			p, err := normalizeBinary(*file.Binaries.Ytdlp, "ytdlp")
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			c.Binaries.Ytdlp = p
		}
		if file.Binaries.Ffmpeg != nil {
			p, err := normalizeBinary(*file.Binaries.Ffmpeg, "ffmpeg")
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			c.Binaries.Ffmpeg = p
		}
		if file.Binaries.WhisperCLI != nil {
			p, err := normalizeBinary(*file.Binaries.WhisperCLI, "whisper_cli")
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			c.Binaries.WhisperCLI = p
		}
	}
	if file.Naming != nil && file.Naming.OnConflict != nil {
		// Whitespace-only OnConflict → keep default ("ask"). Non-empty
		// values are stored verbatim; CLI-layer validation decides if the
		// value is one of ask/overwrite/skip/abort.
		if v := strings.TrimSpace(*file.Naming.OnConflict); v != "" {
			c.Naming.OnConflict = v
		}
	}
	if file.Auth != nil && file.Auth.CookiesFromBrowser != nil {
		// Whitespace-only ⇒ keep the Default ("chrome"). Non-empty
		// values are stored verbatim; the downloader layer interprets
		// them (including the "none" sentinel — see Auth godoc).
		if v := strings.TrimSpace(*file.Auth.CookiesFromBrowser); v != "" {
			c.Auth.CookiesFromBrowser = v
		}
	}
	return c, nil
}

// Merge overlays cli on top of base. Non-zero string fields in cli win;
// zero-valued fields defer to base. Boolean debug knobs are OR-merged
// (either turns them on). base or cli may be nil; nil base is treated
// as [Default], nil cli as an empty overlay. Neither input is mutated.
//
// If base is nil AND [Default] returns an error (HOME / CWD unresolvable —
// see review R6), Merge falls through with a zero-valued Config for the
// missing paths rather than returning an error. In practice this branch is
// exercised only by tests that inject synthetic overlays; the production
// CLI always feeds a non-nil base from [Load], which surfaces the same
// Default error much earlier.
//
// This is intentionally not "reflection-driven": we want to see every
// merge decision at a glance so it's obvious when a new field forgets
// to plug in here.
//
// Note: [Binaries] fields are deliberately NOT merged from cli. Per plan
// §4.6, external binary paths have no CLI flag — they only come from
// yaml (which is already baked into `base` by [Load]) or from $PATH
// during preflight. Callers must not stuff paths into `cli.Binaries`
// expecting them to win; that would silently violate the flag contract.
func Merge(base, cli *Config) *Config {
	if base == nil {
		def, err := Default()
		if err != nil || def == nil {
			// Environment can't produce a sensible default. Fall through
			// with a zero-valued Config: the caller's overlay + downstream
			// Finalize/Validate will still get a chance to fill missing
			// fields or surface an error.
			base = &Config{}
		} else {
			base = def
		}
	}
	out := *base
	if cli == nil {
		return &out
	}

	if cli.OutputDir != "" {
		out.OutputDir = cli.OutputDir
	}
	if cli.Format != "" {
		out.Format = cli.Format
	}
	if cli.Model != "" {
		out.Model = cli.Model
	}
	if cli.Logging.Level != "" {
		out.Logging.Level = cli.Logging.Level
	}
	if cli.Logging.Format != "" {
		out.Logging.Format = cli.Logging.Format
	}
	if cli.Logging.File != "" {
		out.Logging.File = cli.Logging.File
	}
	if cli.Logging.DebugFile != "" {
		out.Logging.DebugFile = cli.Logging.DebugFile
	}
	if cli.Debug {
		out.Debug = true
	}
	if cli.SkipPreflight {
		out.SkipPreflight = true
	}
	if cli.Naming.OnConflict != "" {
		out.Naming.OnConflict = cli.Naming.OnConflict
	}
	return &out
}

// Finalize applies the debug↔level linkage documented in plan §4.5:
//
//   - if Debug is true and Logging.Level was not explicitly set,
//     Logging.Level becomes "debug" so external-tool stderr and internal
//     debug events actually surface.
//   - if Debug is true but Logging.Level is explicitly set to
//     something other than "debug", we honour the user's explicit
//     choice and write a warning to warn so the mismatch does not
//     silently swallow debug events. warn may be nil (no-op).
//   - otherwise Logging.Level falls back to "info".
//
// Format defaults to "text" if unspecified. Callers should invoke
// Finalize once after Merge, before handing the config to the logger.
func (c *Config) Finalize(warn io.Writer) {
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	if c.Debug {
		if c.Logging.Level == "" {
			c.Logging.Level = "debug"
		} else if c.Logging.Level != "debug" && warn != nil {
			fmt.Fprintf(warn, "bilibili-txt: warning: debug=true 但 logging.level=%q（显式设置优先），debug 级别日志将被过滤；如需查看 debug 日志请设 logging.level=debug 或移除该字段\n", c.Logging.Level)
		}
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
}

// Validate rejects out-of-range enum values that survived Load/Merge.
// Path-shaped fields are left alone (preflight owns filesystem checks).
func (c *Config) Validate() error {
	switch c.Format {
	case "txt", "md", "srt":
	default:
		return fmt.Errorf("format: 不支持的值 %q (可选：txt|md|srt)", c.Format)
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format: 不支持的值 %q (可选：text|json)", c.Logging.Format)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level: 不支持的值 %q (可选：debug|info|warn|error)", c.Logging.Level)
	}
	return nil
}

// rawConfig mirrors [Config] but uses pointers so we can distinguish
// "field absent" from "field present but zero-valued".
type rawConfig struct {
	OutputDir     *string      `yaml:"output_dir"`
	Format        *string      `yaml:"format"`
	Model         *string      `yaml:"model"`
	Logging       *rawLogging  `yaml:"logging"`
	Debug         *bool        `yaml:"debug"`
	SkipPreflight *bool        `yaml:"skip_preflight"`
	Binaries      *rawBinaries `yaml:"binaries"`
	Naming        *rawNaming   `yaml:"naming"`
	Auth          *rawAuth     `yaml:"auth"`
}

type rawLogging struct {
	Level     *string `yaml:"level"`
	Format    *string `yaml:"format"`
	File      *string `yaml:"file"`
	DebugFile *string `yaml:"debug_file"`
}

type rawBinaries struct {
	Ytdlp      *string `yaml:"ytdlp"`
	Ffmpeg     *string `yaml:"ffmpeg"`
	WhisperCLI *string `yaml:"whisper_cli"`
}

type rawNaming struct {
	OnConflict *string `yaml:"on_conflict"`
}

type rawAuth struct {
	CookiesFromBrowser *string `yaml:"cookies_from_browser"`
}

// normalizeBinary trims whitespace; empty ⇒ "" (means unspecified). Otherwise
// runs full path normalization (~, relative → abs). fieldName is echoed
// into error messages so users can locate the offending yaml key.
func normalizeBinary(raw, fieldName string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	return normalizePath(trimmed, "binaries."+fieldName)
}

// normalizePath expands ~ / ~user, then converts relative paths to
// absolute via CWD. ~user (any suffix after ~ before the first slash)
// is rejected with an explicit error — we deliberately don't want to
// grow a /etc/passwd dependency here. fieldName is echoed into errors.
//
// Delegates to [pathx.Normalize]; kept as a thin wrapper so callers in
// this package keep their historical call site and the shared helper
// stays discoverable via package review (R3).
func normalizePath(raw, fieldName string) (string, error) {
	return pathx.Normalize(raw, fieldName)
}

// userHome resolves the user's home directory. Wraps os.UserHomeDir with
// a defensive empty-string guard: even though the stdlib on macOS/Linux
// returns a non-nil error when $HOME is empty, other GOOS values or a
// future stdlib change could conceivably yield ("", nil). Treat an empty
// value as an error so callers never silently join paths onto "".
func userHome() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("HOME is empty")
	}
	return h, nil
}
