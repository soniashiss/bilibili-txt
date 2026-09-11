// Package naming builds filesystem-safe output paths for transcripts and
// resolves conflicts when a target file already exists.
//
// The two public entry points are [BuildPath] and [ResolveConflict]:
//
//   - [BuildPath] renders `<slug>__<bvid>[.p<n>].<ext>` under a caller-
//     supplied directory. See plan §4.3 for the exact grammar. The
//     `.p<n>` segment is only appended when the video has more than one
//     page, so single-page videos stay clutter-free.
//   - [ResolveConflict] decides — for a pre-existing target path — whether
//     to overwrite, skip, abort, or ask the user interactively. The
//     `ask` branch is delegated to a [Prompter] so that non-interactive
//     callers (or unit tests) can inject their own decision without
//     depending on os.Stdin.
//
// The slug rules deliberately keep only three character classes:
//
//   - Han (Unicode script "Han"): all CJK ideographs used in Chinese titles
//   - ASCII letters A-Z / a-z (English only; letters with diacritics such
//     as café / naïve / München are stripped — plan §4.3 speaks of "英文"
//     which we interpret strictly as ASCII to avoid drifting into a full
//     Unicode Latin script and dragging in Latin Extended forms that
//     confuse filename search)
//   - ASCII digits 0-9
//
// Everything else — filesystem-illegal chars (/\\?%*:|"<>), emoji, control
// chars, punctuation, non-Latin/non-Han scripts (Cyrillic, kana, Hangul,
// Arabic, Devanagari, …) — is stripped. Runs of whitespace collapse to a
// single '_'; leading/trailing whitespace or underscore is trimmed.
// Length is capped at 50 runes (not bytes) so paths stay far below the
// 255-byte APFS/most-Linux filesystem limit even when every rune is
// 3-byte Han.
//
// Empty slug (empty title, or title that reduces to nothing) falls back
// to the bvid itself, guaranteeing the caller always gets a non-empty
// basename.
//
// # Caller contract for BuildPath
//
// [BuildPath] is a low-level string builder, not an input validator: it
// assumes bvid comes from a trusted upstream (yt-dlp `-J` on a bilibili
// URL) and that ext is one of the supported output formats. As a defence
// against propagating malicious data unchecked, it panics on obviously
// unsafe inputs (empty bvid, path separators, `..`, whitespace, empty
// ext, non-alphanumeric ext). Panics here indicate a programmer error,
// not user input we should try to normalise silently.
package naming

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSlugRunes is the plan §4.3 slug length cap, in Unicode code points
// (runes), not bytes. Chosen so worst-case 50 × 3-byte Han = 150 bytes
// leaves ~100 bytes for the "__<bvid>.pNN.<ext>" suffix under the 255-
// byte APFS filename limit.
const maxSlugRunes = 50

// ConflictAction enumerates what the pipeline should do when the target
// output path already exists on disk.
type ConflictAction int

const (
	// ActionAbort — bail out with a non-zero exit and let the user
	// re-run with an explicit choice. This is the zero value on
	// purpose: an unset action must not silently overwrite anything.
	ActionAbort ConflictAction = iota
	// ActionOverwrite — proceed and replace the existing file.
	ActionOverwrite
	// ActionSkip — leave the existing file alone and treat the run as
	// a no-op (the caller decides how to report this).
	ActionSkip
)

// String helps test failures print a meaningful diff.
func (a ConflictAction) String() string {
	switch a {
	case ActionOverwrite:
		return "overwrite"
	case ActionSkip:
		return "skip"
	case ActionAbort:
		return "abort"
	default:
		return fmt.Sprintf("ConflictAction(%d)", int(a))
	}
}

// Prompter asks the user what to do when the strategy is "ask" and the
// process is attached to a TTY. Implementations must return a fully
// resolved [ConflictAction]; returning an error causes [ResolveConflict]
// to fall back to [ActionAbort] with the error wrapped.
type Prompter interface {
	// Ask reports the existing path and blocks until the user picks
	// overwrite / skip / abort. Implementations must treat an empty
	// line and EOF as [ActionAbort] to match plan §4.3.
	Ask(existingPath string) (ConflictAction, error)
}

// Strategy* are the four legal values for [Options.Strategy] /
// config.yaml `naming.on_conflict` / CLI-derived overrides. Exported so
// the CLI and config layers can reference the canonical spellings by
// symbol instead of hard-coding strings. Lower-case matches plan §4.5
// / §4.6 and [ResolveConflict]'s case-insensitive input handling.
const (
	StrategyAsk       = "ask"
	StrategyOverwrite = "overwrite"
	StrategySkip      = "skip"
	StrategyAbort     = "abort"
)

// Options bundles the runtime knobs [ResolveConflict] needs. It exists
// as a struct (rather than a long argument list) so we can extend it
// without touching every call site.
type Options struct {
	// Strategy is one of "ask" / "overwrite" / "skip" / "abort". Empty
	// string means "ask" (defensive default; matches config default).
	Strategy string
	// IsTTY signals whether stdin/stdout are attached to a terminal.
	// When false, "ask" degrades to "abort" with a --overwrite/--skip
	// hint instead of hanging on a Prompter that has no user.
	IsTTY bool
	// NoInteractive forces "ask" to behave as if IsTTY were false even
	// on a real TTY. Wired to the CLI's --no-interactive flag.
	NoInteractive bool
	// Prompter is invoked when Strategy resolves to "ask" and the
	// environment is interactive. May be nil in non-interactive
	// contexts; passing nil while an interactive prompt is required
	// yields an error rather than a nil-deref panic.
	Prompter Prompter
}

// Slug renders a filesystem-safe basename component from a raw video
// title following the rules described in the package doc. Empty
// output is a legal return value; callers ([BuildPath]) fall back to
// the bvid in that case.
func Slug(title string) string {
	if title == "" {
		return ""
	}

	// Pass 1: filter runes, folding whitespace to '_'. We keep the
	// collapsing behaviour ("run of whitespace → one '_'") in the
	// same pass to avoid an extra string alloc.
	var b strings.Builder
	b.Grow(len(title))
	lastWasUnderscore := false
	for _, r := range title {
		switch {
		case unicode.IsSpace(r):
			if !lastWasUnderscore {
				b.WriteRune('_')
				lastWasUnderscore = true
			}
		case isSlugAllowed(r):
			b.WriteRune(r)
			lastWasUnderscore = false
		default:
			// Silently strip: filesystem-illegal, emoji, control,
			// punctuation, non-Han Unicode letters, etc. We do NOT
			// insert a separator here — "a?b" becomes "ab", not
			// "a_b", to keep results predictable regardless of the
			// exact stripped-char class.
			continue
		}
	}
	s := b.String()

	// Pass 2: rune-based truncation to maxSlugRunes. Byte-based
	// slicing would corrupt Han (3 bytes/rune).
	if utf8.RuneCountInString(s) > maxSlugRunes {
		i := 0
		count := 0
		for i < len(s) {
			_, size := utf8.DecodeRuneInString(s[i:])
			count++
			if count > maxSlugRunes {
				break
			}
			i += size
		}
		s = s[:i]
	}

	// Pass 3: trim any leading/trailing '_' left behind by whitespace
	// folding or truncation. Also collapses "____" tail into nothing.
	s = strings.Trim(s, "_")
	return s
}

// isSlugAllowed returns true iff r should survive slug filtering. The
// whitelist is deliberately tight: any doubt → strip.
func isSlugAllowed(r rune) bool {
	// ASCII digits are the only digit class we allow. `unicode.IsDigit`
	// would also let through Arabic-Indic digits etc. — reject those
	// for filesystem sanity.
	if r >= '0' && r <= '9' {
		return true
	}
	// ASCII letters ONLY. We deliberately do NOT expand to unicode.Latin
	// because plan §4.3's "英文" is best interpreted as ASCII (a-z, A-Z):
	// Latin script would also admit café / naïve / München which mix
	// awkwardly with filename search and case-folding on some
	// filesystems. If your title has diacritics, they are stripped.
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
		return true
	}
	// Han (Chinese ideographs).
	if unicode.Is(unicode.Han, r) {
		return true
	}
	return false
}

// BuildPath assembles the full output path for a transcript file. See
// package doc / plan §4.3 for the grammar. `page` is 1-based; the
// `.p<n>` segment is only emitted when `totalPages > 1`. `ext` may or
// may not include a leading dot — both forms are accepted.
//
// It panics if bvid or ext contain values that would let a caller escape
// `dir` or produce filesystem-illegal names, or if `page` is outside
// [1, totalPages] when totalPages > 1 — see the package doc's "Caller
// contract" section. These are contract violations, not user input
// errors; the pipeline layer is expected to have sanitised all three
// long before we get here.
func BuildPath(dir, title, bvid string, page, totalPages int, ext string) string {
	if err := validateBvid(bvid); err != nil {
		panic(fmt.Sprintf("naming.BuildPath: invalid bvid: %v", err))
	}
	if err := validateExt(ext); err != nil {
		panic(fmt.Sprintf("naming.BuildPath: invalid ext: %v", err))
	}
	// Normalise ext AFTER validation. validateExt has already rejected
	// pure-dot strings (".", "..") and non-alnum bodies; stripping the
	// leading dots here lets callers pass either "txt" or ".txt" or
	// "..txt" and get the same rendered basename.
	ext = strings.TrimLeft(ext, ".")
	if totalPages > 1 && (page < 1 || page > totalPages) {
		panic(fmt.Sprintf("naming.BuildPath: page=%d out of [1,%d]", page, totalPages))
	}

	slug := Slug(title)
	if slug == "" {
		slug = bvid
	}

	basename := slug + "__" + bvid
	if totalPages > 1 {
		basename = fmt.Sprintf("%s.p%d", basename, page)
	}
	if ext != "" {
		basename += "." + ext
	}
	return filepath.Join(dir, basename)
}

// validateBvid enforces the minimal invariants BuildPath needs: no empty
// string, no path separators, no traversal, no whitespace, no control
// characters. We do NOT enforce the full `^BV[0-9A-Za-z]{10}$` pattern
// here — that's the downloader's job — because tests and future formats
// may legitimately pass shorter placeholders.
func validateBvid(bvid string) error {
	if bvid == "" {
		return errors.New("empty bvid")
	}
	if strings.ContainsAny(bvid, `/\`) {
		return fmt.Errorf("bvid contains path separator: %q", bvid)
	}
	if bvid == "." || strings.Contains(bvid, "..") {
		return fmt.Errorf("bvid contains parent traversal: %q", bvid)
	}
	for _, r := range bvid {
		if unicode.IsSpace(r) {
			return fmt.Errorf("bvid contains whitespace: %q", bvid)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("bvid contains control char: %q", bvid)
		}
	}
	return nil
}

// validateExt accepts three shapes:
//   - "" (no extension)
//   - "<alnum>+"           e.g. "txt", "md", "srt"
//   - "."*"<alnum>+"       e.g. ".txt", "..txt", "...srt"
//
// Anything else — pure dots ("." / "..." with no tail), path separators,
// whitespace, punctuation, or unicode letters — is rejected before
// [BuildPath] does any trimming. Rejecting on the raw string closes a
// path-traversal loophole where `..` would otherwise be silently trimmed
// to `""` and slip through unnoticed.
func validateExt(ext string) error {
	if ext == "" {
		return nil
	}
	trimmed := strings.TrimLeft(ext, ".")
	if trimmed == "" {
		return fmt.Errorf("ext must contain at least one non-dot char, got %q", ext)
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		default:
			return fmt.Errorf("ext must match [A-Za-z0-9]+ after any leading dots, got %q", ext)
		}
	}
	return nil
}

// ResolveConflict decides what to do when the target path already
// exists. The strategy string is what the CLI/config eventually settles
// on (plan §4.5 flag precedence lives in the CLI layer, not here).
func ResolveConflict(existingPath string, opts Options) (ConflictAction, error) {
	strat := strings.ToLower(strings.TrimSpace(opts.Strategy))
	if strat == "" {
		strat = "ask"
	}

	switch strat {
	case "overwrite":
		return ActionOverwrite, nil
	case "skip":
		return ActionSkip, nil
	case "abort":
		return ActionAbort, nil
	case "ask":
		// Interactive requires both a TTY AND the user not forcing
		// non-interactive mode. Missing either → degrade gracefully,
		// with a hint that names the culprit so users can tell "wrong
		// terminal" from "flag I set".
		if opts.NoInteractive {
			return ActionAbort, fmt.Errorf(
				"output already exists: %s (--no-interactive is set; add --overwrite to replace or --skip to keep)",
				existingPath)
		}
		if !opts.IsTTY {
			return ActionAbort, fmt.Errorf(
				"output already exists: %s (stdin is not a TTY; add --overwrite to replace or --skip to keep)",
				existingPath)
		}
		if opts.Prompter == nil {
			return ActionAbort, fmt.Errorf(
				"output already exists: %s (interactive prompt required but no Prompter wired)",
				existingPath)
		}
		action, err := opts.Prompter.Ask(existingPath)
		if err != nil {
			return ActionAbort, fmt.Errorf("prompt for %s: %w", existingPath, err)
		}
		switch action {
		case ActionAbort, ActionOverwrite, ActionSkip:
			return action, nil
		default:
			return ActionAbort, fmt.Errorf("prompter returned invalid action %v for %s", action, existingPath)
		}
	default:
		return ActionAbort, fmt.Errorf("unknown on_conflict strategy %q (want ask|overwrite|skip|abort)", opts.Strategy)
	}
}

// StdinPrompter is the production [Prompter] implementation. It reads a
// single line from `In` and writes the prompt to `Out`. Both fields are
// exported so tests can substitute buffers; zero-value StdinPrompter is
// not useful — [NewStdinPrompter] wires up the real os.Stdin/os.Stderr.
type StdinPrompter struct {
	In  io.Reader
	Out io.Writer
}

// NewStdinPrompter returns a [StdinPrompter] wired to the real stdin and
// stderr. Stderr (not stdout) is used for the prompt so pipelines that
// consume stdout aren't polluted.
func NewStdinPrompter() *StdinPrompter {
	return &StdinPrompter{In: os.Stdin, Out: os.Stderr}
}

// Ask implements [Prompter]. The prompt lists the three options; the
// default (empty line or EOF) is Abort — safest choice per plan §4.3.
func (p *StdinPrompter) Ask(existingPath string) (ConflictAction, error) {
	if p.Out != nil {
		if _, err := fmt.Fprintf(p.Out,
			"output already exists: %s\n[O]verwrite / [S]kip / [A]bort (default A): ",
			existingPath); err != nil {
			return ActionAbort, fmt.Errorf("write prompt: %w", err)
		}
	}

	if p.In == nil {
		return ActionAbort, nil
	}
	reader := bufio.NewReader(p.In)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ActionAbort, fmt.Errorf("read stdin: %w", err)
	}
	// EOF with no bytes read → default Abort.
	choice := strings.ToLower(strings.TrimSpace(line))
	switch choice {
	case "", "a", "abort":
		return ActionAbort, nil
	case "o", "overwrite":
		return ActionOverwrite, nil
	case "s", "skip":
		return ActionSkip, nil
	default:
		return ActionAbort, fmt.Errorf("unrecognized choice %q (expected O/S/A)", strings.TrimSpace(line))
	}
}
