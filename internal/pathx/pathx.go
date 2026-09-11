// Package pathx centralises the ~ / relative → absolute path
// normalisation used across [config], the CLI flag layer, and
// [logger]'s debug-file resolver. Historically each of those sites
// grew its own near-identical helper (see review R3): [Normalize]
// consolidates the ~-expansion + rel→abs logic behind a single
// exported entry point; [AbsFromCwd] exposes the smaller "rel → abs"
// half for callers (like [logger]) that treat a raw ~ prefix as an
// architectural violation rather than a value to expand.
//
// The rules match config's historical behaviour so existing config
// users (yaml parsers, `--output-dir`) migrate without a semantic
// diff:
//
//   - Empty / whitespace-only input ⇒ ("", nil). Callers decide
//     whether that means "unspecified" (config) or an error (CLI).
//   - "~" or "~/..." ⇒ [os.UserHomeDir] + suffix.
//   - "~user" / "~user/..." ⇒ error (no /etc/passwd dependency).
//   - Absolute path ⇒ [filepath.Clean].
//   - Otherwise ⇒ join with [os.Getwd].
//
// fieldName is echoed into errors so users can locate the offending
// yaml key / flag name at a glance.
package pathx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Normalize expands ~ / ~user, then converts relative paths to
// absolute via CWD. ~user (any suffix after ~ before the first slash)
// is rejected with an explicit error — callers must not shell out to
// /etc/passwd lookups from path-processing time.
//
// Empty / whitespace-only input yields ("", nil) so callers can pass
// user config verbatim; treating "" as "unspecified" is the
// convention the whole codebase uses.
func Normalize(raw, fieldName string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}

	if strings.HasPrefix(trimmed, "~") {
		var rest string
		switch {
		case trimmed == "~":
			rest = ""
		case strings.HasPrefix(trimmed, "~/"):
			rest = trimmed[2:]
		default:
			return "", fmt.Errorf("%s: ~user syntax is not supported (offending value %q)", fieldName, trimmed)
		}
		home, err := userHome()
		if err != nil {
			return "", fmt.Errorf("%s: cannot resolve ~ (no HOME): %w", fieldName, err)
		}
		if rest == "" {
			return home, nil
		}
		return filepath.Join(home, rest), nil
	}

	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed), nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve relative path (no CWD): %w", fieldName, err)
	}
	return filepath.Join(cwd, trimmed), nil
}

// AbsFromCwd is the "relative → absolute via CWD" half of [Normalize],
// exposed for callers (currently [logger.resolveDebugLogPath]) that
// treat a raw ~ prefix as a caller-side bug — they receive paths that
// the config layer has already expanded, so a lingering ~ signals a
// mis-plumbed pipeline rather than a value to expand.
//
// Empty / whitespace-only input yields ("", nil). Absolute paths are
// returned unchanged after [filepath.Clean]; relative paths are joined
// with [os.Getwd].
//
// fieldName is echoed into errors so callers can pinpoint which value
// tripped the check without wrapping every call site.
func AbsFromCwd(raw, fieldName string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve relative path (no CWD): %w", fieldName, err)
	}
	return filepath.Join(cwd, trimmed), nil
}

// userHome resolves the user's home directory. Wraps os.UserHomeDir
// with a defensive empty-string guard: even though the stdlib on
// macOS/Linux returns a non-nil error when $HOME is empty, other GOOS
// values or a future stdlib change could conceivably yield ("", nil).
// Treat an empty value as an error so callers never silently join
// paths onto "".
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
