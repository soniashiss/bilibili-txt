// Package formatter renders a normalised [subtitle.Cue] stream into one
// of the three output formats the CLI advertises: plain text, markdown
// with timestamp chapters, or SRT pass-through.
//
// The single public entry point is [Convert]. Callers pick the target
// via [Format]; [Convert] dispatches to the per-format writer without
// forcing consumers to depend on the sub-packages directly. Keeping
// dispatch centralised means the pipeline layer stays format-agnostic:
// adding VTT tomorrow is one const and one new writer, not a rewrite
// of every caller.
//
// # Contract (frozen by docs/contracts.md §三.2)
//
//   - Input `cues` is what [subtitle.Parse] returned — sorted by
//     [subtitle.Cue.Start], multi-line source text already joined.
//     [Convert] does no re-sorting and no re-parsing.
//   - `outPath` is a caller-supplied absolute (or relative-to-CWD)
//     path. [Convert] does NOT create the parent directory; the
//     pipeline layer already ran [naming.BuildPath] which lives under
//     an [preflight]-checked output_dir. Any ENOENT surfaces raw so
//     the pipeline's conflict-resolution logs can pinpoint the caller.
//   - Empty `cues` returns [subtitle.ErrEmpty]. Reusing the sentinel
//     across the parser and the formatter keeps caller error-handling
//     simple: one `errors.Is(err, subtitle.ErrEmpty)` catches both
//     "source SRT was empty" and "you asked me to convert nothing".
//   - Unknown [Format] returns [ErrUnknownFormat] without touching
//     the filesystem — validation of `format` is the CLI/config
//     layer's job, but defence-in-depth here catches drift caused by
//     silent enum extensions.
//   - Idempotent by choice, NOT by design: [Convert] overwrites
//     `outPath` via [os.Create] (O_TRUNC). The pipeline runs
//     [naming.ResolveConflict] BEFORE calling [Convert]; by the time
//     bytes hit the disk, the caller has already agreed to
//     overwrite / skip / abort.
//
// # Format matrix
//
//	| Format    | Renderer     | Timestamps | Line breaks             |
//	| --------- | ------------ | ---------- | ----------------------- |
//	| FormatTXT | txt.go       | none       | one cue per line        |
//	| FormatMD  | md.go        | [mm:ss]    | markdown chapter blocks |
//	| FormatSRT | srt.go       | HH:MM:SS,m | canonical SRT layout    |
//
// Choices worth noting up front (details in each writer's doc):
//
//   - TXT emits one line per non-empty cue in the caller-supplied
//     order. Cue segmentation from the upstream source (Bilibili CC,
//     whisper.cpp ASR) already reflects a semantic-ish unit; forcing
//     a second pass with a sentence-terminator heuristic collapses
//     unpunctuated tracks (Bilibili `ai-zh` typically carries zero
//     `。！？`) into a single unreadable line, so we trust the cue
//     boundary instead.
//   - MD leads each cue with `[mm:ss]` so the file scrolls like a
//     podcast transcript with click-worthy jump points. Hours are
//     inlined into the minute field ("[75:03]") rather than a
//     separate "hh" segment; keeps the visual width predictable.
//   - SRT is a canonical pass-through: CRLF line endings, comma
//     separator on timestamps, and the source [subtitle.Cue.Index]
//     preserved verbatim so a "parse → format" round-trip is a
//     no-op on the visible content.
package formatter

import (
	"errors"
	"fmt"
	"os"

	"bilibili-txt/internal/subtitle"
)

// Format enumerates the target output formats the CLI supports.
// String value matches [config.Config.Format] / the `--format` flag
// so validation code can share the same strings.
type Format string

const (
	// FormatTXT emits pure UTF-8 plain text — one line per non-empty
	// cue in caller-supplied order (see [writeTXT] for the rationale).
	// No timestamps, no decoration; best target for LLM ingestion or
	// grep-friendly transcripts.
	FormatTXT Format = "txt"
	// FormatMD emits GitHub-flavoured markdown with `[mm:ss]` chapter
	// stubs. Suitable for human review or Obsidian-style archives.
	FormatMD Format = "md"
	// FormatSRT is a canonical SRT pass-through: CRLF line endings,
	// canonical timestamp form, and source [subtitle.Cue.Index]
	// preserved verbatim. Handy when the user wants to feed the
	// transcript back into a video editor or re-run through a
	// different downstream tool without re-parsing.
	FormatSRT Format = "srt"
)

// ErrUnknownFormat is returned by [Convert] when [Format] is neither
// [FormatTXT] / [FormatMD] / [FormatSRT]. The CLI validates upstream,
// so hitting this in production points at a code bug (new enum,
// forgotten switch arm), not user error.
var ErrUnknownFormat = errors.New("formatter: unknown format")

// Convert renders `cues` into `outPath` using `format`. See the
// package doc for the frozen contract and per-format renderer
// choices.
//
// The named return `err` lets the deferred [os.File.Close] surface a
// close-time error (e.g. an fsync failure on flush) when the writer
// itself succeeded — without a named return the Close error would be
// silently swallowed because the function has already committed to a
// non-error return by the time the defer runs.
func Convert(cues []subtitle.Cue, outPath string, format Format) (err error) {
	if len(cues) == 0 {
		// Reuse subtitle.ErrEmpty so pipeline error-handling
		// converges on a single sentinel across the parse and
		// render legs. The wrap keeps the "convert" verb in the
		// error message for grepability.
		return fmt.Errorf("formatter: convert: %w", subtitle.ErrEmpty)
	}

	// Validate format BEFORE touching the filesystem so an unknown
	// format never leaves a phantom zero-byte file behind.
	switch format {
	case FormatTXT, FormatMD, FormatSRT:
		// ok
	default:
		return fmt.Errorf("%w: %q", ErrUnknownFormat, format)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("formatter: create %s: %w", outPath, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("formatter: close %s: %w", outPath, cerr)
		}
	}()

	switch format {
	case FormatTXT:
		err = writeTXT(f, cues)
	case FormatMD:
		err = writeMD(f, cues)
	case FormatSRT:
		err = writeSRT(f, cues)
	}
	// Don't delete a partial file on write error — the pipeline's
	// conflict logic already accepted "we will write here", so
	// leaving the partial output on disk is less surprising than a
	// phantom deletion the user can't explain from the logs.
	return err
}
