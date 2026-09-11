// Package subtitle parses SRT files into a normalised [Cue] stream that
// the formatter layer can render to txt / md / srt without re-parsing.
//
// The public entry point is [Parse]. It reads a file from disk, strips
// transport artefacts (UTF-8 BOM, CRLF), tolerates blank-line spacing,
// merges multi-line cue text into a single space-joined string, and
// returns cues sorted by start time. The parser is deliberately lenient
// on layout (blank lines between cues are optional, trailing blank
// lines are tolerated, index lines may be missing or garbled) but
// strict on the one thing that carries semantic weight — the
// "HH:MM:SS,mmm --> HH:MM:SS,mmm" timing line and its Start < End
// invariant. A malformed timing line or a Start >= End yields
// [ErrMalformed] wrapped with the 1-based file line number where the
// parser tripped, so log messages can point the user at the offending
// block.
//
// # Contract (frozen by docs/contracts.md §一.2 and §三.1)
//
//   - Reads only; never mutates the source file.
//   - Returns nil, [ErrEmpty] iff the file contains zero well-formed
//     cues (empty file, whitespace-only file, or every block failed).
//     Empty is a normal outcome for empty subtitle tracks, not an
//     internal error — callers use `errors.Is` to detect it.
//   - Returns nil, [ErrMalformed] wrapped via fmt.Errorf iff at least
//     one block *started* parsing but failed on its timing line or the
//     Start < End invariant. A file that mixes valid and malformed
//     cues errors out rather than silently dropping data; better to
//     force the user to look at the input than to hand the pipeline a
//     partial transcript. Index-line damage does NOT count as
//     malformed — see [Cue.Index] for the fallback rule mandated by
//     the contract.
//   - Returns nil, wrapped [os.ErrNotExist] / [os.ErrPermission] when
//     the file cannot be opened. Wrappers preserve `errors.Is` matches
//     for the stdlib sentinels so callers do not need subtitle-specific
//     branches.
//
// # Normalisation rules
//
//   - UTF-8 BOM (U+FEFF) at file start is stripped once.
//   - CRLF / CR line endings are treated as LF; the parser splits on
//     any of \n, \r\n, \r so downloads produced on Windows or older
//     macOS still parse identically.
//   - Multi-line cue text is joined with a single ASCII space. This
//     matches the plan §4.4 formatter contract that renders "one cue,
//     one line" and keeps [Cue.Text] safe to embed in md tables.
//   - Trailing spaces on each cue text line are trimmed before the
//     join so wrapping quirks in the source do not leak through.
//   - Cues are sorted by [Cue.Start] ascending. Ties keep source order.
//
// # Non-goals
//
// The parser is v1-scoped: SRT only. VTT / LRC / ASS live in future
// `ParseVTT` etc. functions — extending the file format menu is a
// contract update, not a mechanical addition, because [Cue] semantics
// (single [Cue.Text] line, integer [Cue.Index]) may need to grow.
package subtitle

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cue is one subtitle line, format-agnostic. See docs/contracts.md
// §一.2 for the frozen field semantics.
type Cue struct {
	// Index is the sequence number as it appeared in the source SRT
	// (1-based). Per contract §一.2, when the source omits the index
	// line, presents an unparseable value (e.g. "abc", "#1"), or a
	// non-positive number (e.g. "0"), the parser synthesises the
	// index from the physical block order — callers should treat
	// this as an opaque identifier, not a stable key across parses
	// of mutated files.
	Index int
	// Start is the cue's onset relative to the video start.
	Start time.Duration
	// End is the cue's offset relative to the video start. The parser
	// rejects blocks where Start >= End as malformed (a zero-length
	// cue carries no information and is almost always a symptom of a
	// truncated timing line).
	End time.Duration
	// Text is the cue's text, UTF-8 normalised: multi-line source
	// text is joined with a single ASCII space, trailing whitespace
	// on each source line is trimmed, and the resulting string never
	// ends with a newline. Empty text is allowed (some SRT producers
	// emit karaoke-style empty cues); callers that care can filter.
	Text string
}

// ErrEmpty signals that the file was syntactically parseable but
// contained zero cues. Callers use `errors.Is(err, subtitle.ErrEmpty)`
// to distinguish "empty subtitle track" from "malformed file".
var ErrEmpty = errors.New("subtitle: no valid cues")

// ErrMalformed signals that at least one cue block failed to parse.
// The concrete error returned by [Parse] wraps this sentinel via
// fmt.Errorf so `errors.Is` still matches while the top-level message
// carries the 1-based file line number where parsing tripped.
var ErrMalformed = errors.New("subtitle: malformed cue")

// srtArrow is the fixed timing delimiter in the SRT spec. Matching by
// substring rather than regex keeps the hot loop allocation-free.
const srtArrow = "-->"

// utf8BOM is the byte sequence produced by encoders that mark UTF-8
// files with a leading U+FEFF. yt-dlp does not add one, but user-
// supplied subtitles sometimes do; stripping is a two-line concession
// that saves a class of "first cue index unparseable" bug reports.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse reads path and decodes it as SRT, returning the cues in
// start-time order. See the package doc for the full contract and
// error semantics.
func Parse(path string) ([]Cue, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("subtitle: open %s: %w", path, err)
	}
	defer f.Close()
	return parseReader(f, path)
}

// parseReader is the io.Reader-driven core so tests can feed strings
// without a temp-file dance. path is only used for error decoration.
func parseReader(r io.Reader, path string) ([]Cue, error) {
	br := bufio.NewReader(r)

	// Strip a leading UTF-8 BOM if present. Peek first so files
	// smaller than 3 bytes do not error.
	if head, _ := br.Peek(3); len(head) == 3 && head[0] == utf8BOM[0] && head[1] == utf8BOM[1] && head[2] == utf8BOM[2] {
		_, _ = br.Discard(3)
	}

	sc := bufio.NewScanner(br)
	// SRT cue text is short (a screenful of Han glyphs at most), but a
	// pathological "very long single line" file should still parse
	// rather than blowing bufio's default 64 KiB limit. 1 MiB is
	// arbitrary but comfortably above anything yt-dlp emits.
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	var (
		cues       []Cue
		lineNumber int
		blockIndex int
	)

	// Collect raw lines into blocks separated by one or more blank
	// lines. Split-then-parse is simpler than a state machine and
	// makes the "physical block order" fallback trivial.
	var block []string
	blockStart := 0

	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		blockIndex++
		cue, err := parseBlock(block, blockStart, blockIndex)
		block = block[:0]
		if err != nil {
			return err
		}
		if cue != nil {
			cues = append(cues, *cue)
		}
		return nil
	}

	for sc.Scan() {
		lineNumber++
		// bufio.Scanner strips the trailing \n / \r\n; a lone \r on
		// legacy Mac files would leak in, so trim it.
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			blockStart = 0
			continue
		}
		if len(block) == 0 {
			blockStart = lineNumber
		}
		block = append(block, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("subtitle: read %s: %w", path, err)
	}
	if err := flush(); err != nil {
		return nil, err
	}

	if len(cues) == 0 {
		return nil, ErrEmpty
	}

	// Stable sort by Start; tied cues keep source order so overlapping
	// captions on the same second remain readable.
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	return cues, nil
}

// parseBlock decodes a single "index? / timing / text..." SRT block.
// blockStart is the 1-based file line number of the block's first
// non-blank line (for error decoration). fallbackIndex is used when
// the block omits its own index line — some yt-dlp downloads shave
// the index off after --sub-format conversion.
//
// Returns (nil, nil) only for legitimately empty blocks (should not
// happen because [Parse] filters those upstream, defence-in-depth
// keeps the function total).
func parseBlock(lines []string, blockStart, fallbackIndex int) (*Cue, error) {
	if len(lines) == 0 {
		return nil, nil
	}

	// Locate the timing line: it is the first line containing "-->".
	// Anything before it is the (optional) index line; anything after
	// is cue text. Scanning rather than "assume line[0] is index"
	// tolerates SRT files that omit the index or embed extra header
	// noise.
	timingIdx := -1
	for i, l := range lines {
		if strings.Contains(l, srtArrow) {
			timingIdx = i
			break
		}
	}
	if timingIdx == -1 {
		return nil, fmt.Errorf("subtitle: line %d: missing timing line: %w", blockStart, ErrMalformed)
	}

	// Index resolution. Contract docs/contracts.md §一.2 mandates
	// "解析失败时按物理顺序补齐": when the source omits, mangles, or
	// non-positive-numbers the index line, we synthesise from the
	// physical block position rather than refusing the file. This
	// keeps yt-dlp / user-edited SRTs with cosmetic index damage
	// (extra BOM, "#1" prefix, "0" from a broken re-numberer) usable
	// while the malformed-cue signal stays reserved for the two
	// things that carry real semantic weight: the timing line and
	// the Start < End invariant.
	index := fallbackIndex
	if timingIdx > 0 {
		raw := strings.TrimSpace(lines[0])
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			index = n
		}
	}

	start, end, err := parseTiming(lines[timingIdx])
	if err != nil {
		return nil, fmt.Errorf("subtitle: line %d: %w", blockStart+timingIdx, err)
	}
	if start >= end {
		return nil, fmt.Errorf("subtitle: line %d: start (%s) must be < end (%s): %w",
			blockStart+timingIdx, start, end, ErrMalformed)
	}

	// Cue text: everything after the timing line, joined with single
	// spaces after per-line trailing-whitespace trim.
	textLines := lines[timingIdx+1:]
	if len(textLines) == 0 {
		return &Cue{Index: index, Start: start, End: end, Text: ""}, nil
	}
	parts := make([]string, 0, len(textLines))
	for _, l := range textLines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			// Genuine empty lines inside a cue body are rare but
			// legal (some tools emit "line1\n\nline2" for karaoke).
			// Drop them so the joined text stays clean.
			continue
		}
		parts = append(parts, l)
	}
	text := strings.Join(parts, " ")

	return &Cue{Index: index, Start: start, End: end, Text: text}, nil
}

// parseTiming decodes one "HH:MM:SS,mmm --> HH:MM:SS,mmm" line into a
// (start, end) pair. Trailing "X1:Y1:Z1:W1 ..." cue-position hints
// that some SRT variants append are tolerated by keeping only the
// tokens before/after the arrow.
func parseTiming(line string) (time.Duration, time.Duration, error) {
	i := strings.Index(line, srtArrow)
	if i < 0 {
		return 0, 0, fmt.Errorf("timing line missing %q: %w", srtArrow, ErrMalformed)
	}
	left := strings.TrimSpace(line[:i])
	right := strings.TrimSpace(line[i+len(srtArrow):])

	// Cue-position hints (e.g. "00:00:01,000 --> 00:00:02,000 X1:200
	// X2:400 Y1:100 Y2:200") tack tokens onto the right side; we
	// only care about the first whitespace-delimited timestamp.
	if sp := strings.IndexAny(right, " \t"); sp >= 0 {
		right = right[:sp]
	}

	start, err := parseTimestamp(left)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start timestamp %q: %w", left, err)
	}
	end, err := parseTimestamp(right)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end timestamp %q: %w", right, err)
	}
	return start, end, nil
}

// parseTimestamp decodes "HH:MM:SS,mmm" (SRT canonical) or
// "HH:MM:SS.mmm" (some yt-dlp conversions) into a time.Duration.
// Enforces the exact 2/2/2/3 digit widths so "1:2:3,4" is caught as
// malformed rather than accepted with silent zero-padding.
func parseTimestamp(s string) (time.Duration, error) {
	// Accept both ',' (SRT canonical) and '.' (VTT-style, occasionally
	// leaked by conversion). Normalise to ',' for the split so the
	// parse routine is single-branch.
	s = strings.Replace(s, ".", ",", 1)

	// Expected shape: HH:MM:SS,mmm — 12 chars, 2 colons, 1 comma.
	if len(s) != 12 || s[2] != ':' || s[5] != ':' || s[8] != ',' {
		return 0, fmt.Errorf("timestamp %q not in HH:MM:SS,mmm form: %w", s, ErrMalformed)
	}
	hh, err := strconv.Atoi(s[0:2])
	if err != nil || hh < 0 {
		return 0, fmt.Errorf("invalid hours in %q: %w", s, ErrMalformed)
	}
	mm, err := strconv.Atoi(s[3:5])
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("invalid minutes in %q: %w", s, ErrMalformed)
	}
	ss, err := strconv.Atoi(s[6:8])
	if err != nil || ss < 0 || ss > 59 {
		return 0, fmt.Errorf("invalid seconds in %q: %w", s, ErrMalformed)
	}
	ms, err := strconv.Atoi(s[9:12])
	if err != nil || ms < 0 || ms > 999 {
		return 0, fmt.Errorf("invalid milliseconds in %q: %w", s, ErrMalformed)
	}
	d := time.Duration(hh)*time.Hour +
		time.Duration(mm)*time.Minute +
		time.Duration(ss)*time.Second +
		time.Duration(ms)*time.Millisecond
	return d, nil
}
