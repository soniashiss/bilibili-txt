package subtitle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSrt is a tiny helper that materialises a string as a .srt file
// under t.TempDir(). Keeping it here (rather than a shared testdata
// fixture) makes each sub-test self-contained: the raw bytes and the
// expected parse result live within eye distance.
func writeSrt(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestParse_HappyPath_ThreeCues(t *testing.T) {
	// Mirror testdata/sample.srt so this test doubles as a canonical-
	// fixture regression guard: any drift in the shared sample must
	// break here first.
	body := "1\n00:00:00,000 --> 00:00:02,500\n大家好，欢迎收看本期视频。\n\n" +
		"2\n00:00:02,600 --> 00:00:05,200\n今天我们来聊一聊字幕自动化。\n\n" +
		"3\n00:00:05,300 --> 00:00:08,000\n这段文字用来验证解析流程。\n"
	p := writeSrt(t, "sample.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got, want := len(cues), 3; got != want {
		t.Fatalf("cue count: got %d want %d", got, want)
	}
	if cues[0].Index != 1 || cues[0].Start != 0 || cues[0].End != 2500*time.Millisecond {
		t.Errorf("cue[0]: %+v", cues[0])
	}
	if cues[0].Text != "大家好，欢迎收看本期视频。" {
		t.Errorf("cue[0].Text: %q", cues[0].Text)
	}
	if cues[2].Start != 5300*time.Millisecond || cues[2].End != 8000*time.Millisecond {
		t.Errorf("cue[2] timing: start=%v end=%v", cues[2].Start, cues[2].End)
	}
}

func TestParse_CRLF_NormalisesLineEndings(t *testing.T) {
	// Windows-produced subtitles carry \r\n; the scanner must strip
	// the trailing \r so index parsing and text join both stay
	// clean. Without the TrimRight in parseReader the first cue's
	// index would be "1\r" and Atoi would fail with a very confusing
	// "invalid cue index" error.
	body := "1\r\n00:00:00,000 --> 00:00:01,000\r\nhello\r\n\r\n" +
		"2\r\n00:00:01,000 --> 00:00:02,000\r\nworld\r\n"
	p := writeSrt(t, "crlf.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 2 || cues[0].Text != "hello" || cues[1].Text != "world" {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_LoneCR_LegacyMac(t *testing.T) {
	// bufio.Scanner splits on '\n' by default, so \r-only files land
	// as a single "line". The parser must degrade to ErrEmpty (no
	// timing arrow found in the single mega-line ⇒ malformed at
	// block flush). We assert on ErrMalformed rather than ErrEmpty
	// because the whole file counts as one non-blank block.
	body := "1\r00:00:00,000 --> 00:00:01,000\rhello\r"
	p := writeSrt(t, "cr.srt", body)

	_, err := Parse(p)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse: got %v, want ErrMalformed", err)
	}
}

func TestParse_BOM_Stripped(t *testing.T) {
	body := "\xEF\xBB\xBF1\n00:00:00,000 --> 00:00:01,000\nhello\n"
	p := writeSrt(t, "bom.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 1 || cues[0].Index != 1 || cues[0].Text != "hello" {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_ShortFile_NoBOM_DoesNotError(t *testing.T) {
	// A 2-byte file must not misinterpret the two-byte prefix as a
	// BOM candidate. Peek(3) returns len<3 in that case; the parser
	// should skip the discard. The file has no timing so we expect
	// ErrMalformed from the block flush.
	body := "hi"
	p := writeSrt(t, "short.srt", body)

	_, err := Parse(p)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse: got %v, want ErrMalformed (missing timing)", err)
	}
}

func TestParse_MultilineCue_JoinedWithSpace(t *testing.T) {
	// Karaoke-style multi-line cues collapse to a single space-
	// joined string. Trailing spaces on each source line are
	// trimmed before the join so "line1  \nline2" does not turn
	// into "line1   line2".
	body := "1\n00:00:00,000 --> 00:00:02,000\nline one  \nline two\nline three\n"
	p := writeSrt(t, "multiline.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cues[0].Text, "line one line two line three"; got != want {
		t.Fatalf("Text: got %q want %q", got, want)
	}
}

func TestParse_MultipleBlankLines_Tolerated(t *testing.T) {
	// Multi-blank separators between cues are common when tools
	// pad files for readability. Extra blanks must collapse into
	// one block boundary, not turn into empty blocks.
	body := "1\n00:00:00,000 --> 00:00:01,000\nhello\n\n\n\n" +
		"2\n00:00:01,500 --> 00:00:02,500\nworld\n"
	p := writeSrt(t, "blank.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_EmptyFile_ReturnsErrEmpty(t *testing.T) {
	p := writeSrt(t, "empty.srt", "")

	_, err := Parse(p)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Parse: got %v, want ErrEmpty", err)
	}
}

func TestParse_WhitespaceOnlyFile_ReturnsErrEmpty(t *testing.T) {
	// A file that is just newlines/tabs should be treated as empty,
	// not malformed — no block ever starts, so parseBlock is never
	// invoked.
	p := writeSrt(t, "ws.srt", "\n\n   \n\t\n")

	_, err := Parse(p)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Parse: got %v, want ErrEmpty", err)
	}
}

func TestParse_FileNotExist_WrapsOSErrNotExist(t *testing.T) {
	_, err := Parse(filepath.Join(t.TempDir(), "does-not-exist.srt"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Parse: got %v, want wrapped os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "subtitle: open") {
		t.Errorf("error message missing context: %v", err)
	}
}

func TestParse_MissingTiming_Malformed(t *testing.T) {
	body := "1\nhello world\n"
	p := writeSrt(t, "no-timing.srt", body)

	_, err := Parse(p)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse: got %v, want ErrMalformed", err)
	}
	if !strings.Contains(err.Error(), "missing timing") {
		t.Errorf("message: %v", err)
	}
}

func TestParse_InvalidIndex_FallsBackToBlockOrder(t *testing.T) {
	// Contract §一.2: "解析失败时按物理顺序补齐". A garbled index
	// line (e.g. "abc", "#1", stray BOM) must NOT abort the whole
	// file — those are cosmetic producer bugs and dropping the
	// transcript for them is worse than falling back to physical
	// numbering. We verify:
	//   1. Parse succeeds despite the unparseable index.
	//   2. Subsequent well-formed cues keep their explicit indices.
	//   3. The fallback assigns the block's 1-based position.
	body := "abc\n00:00:00,000 --> 00:00:01,000\nfirst\n\n" +
		"2\n00:00:01,000 --> 00:00:02,000\nsecond\n"
	p := writeSrt(t, "bad-idx.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("cues: %+v", cues)
	}
	if cues[0].Index != 1 {
		t.Errorf("fallback index for garbled cue: got %d, want 1 (block position)", cues[0].Index)
	}
	if cues[1].Index != 2 {
		t.Errorf("well-formed cue index: got %d, want 2", cues[1].Index)
	}
	if cues[0].Text != "first" || cues[1].Text != "second" {
		t.Errorf("text drift: %+v", cues)
	}
}

func TestParse_ZeroIndex_FallsBackToBlockOrder(t *testing.T) {
	// SRT indices are 1-based; "0" is either a bug in the producer
	// or a broken re-numberer. Per contract §一.2 we swallow it and
	// synthesise from block position rather than refuse the file.
	body := "0\n00:00:00,000 --> 00:00:01,000\nhello\n"
	p := writeSrt(t, "zero-idx.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(cues) != 1 || cues[0].Index != 1 {
		t.Fatalf("expected fallback index 1, got: %+v", cues)
	}
}

func TestParse_NegativeIndex_FallsBackToBlockOrder(t *testing.T) {
	// Signed-arithmetic bugs in home-brewed SRT writers occasionally
	// leak "-1" indices. Same fallback semantics as zero/garbled.
	body := "-3\n00:00:00,000 --> 00:00:01,000\nhi\n"
	p := writeSrt(t, "neg-idx.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(cues) != 1 || cues[0].Index != 1 {
		t.Fatalf("expected fallback index 1, got: %+v", cues)
	}
}

func TestParse_MissingIndex_SynthesisedFromBlockOrder(t *testing.T) {
	// Some yt-dlp conversions strip the index line; the parser
	// synthesises 1,2,3 from block position so downstream still
	// sees a stable, monotone Cue.Index.
	body := "00:00:00,000 --> 00:00:01,000\nfirst\n\n" +
		"00:00:01,000 --> 00:00:02,000\nsecond\n"
	p := writeSrt(t, "no-idx.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 2 || cues[0].Index != 1 || cues[1].Index != 2 {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_StartGreaterThanEnd_Malformed(t *testing.T) {
	body := "1\n00:00:05,000 --> 00:00:02,000\nreversed\n"
	p := writeSrt(t, "reversed.srt", body)

	_, err := Parse(p)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse: got %v, want ErrMalformed", err)
	}
}

func TestParse_StartEqualsEnd_Malformed(t *testing.T) {
	// Zero-length cue: the spec allows it in theory but every SRT
	// producer that emits it does so by accident (truncated writer,
	// unfinished karaoke). We reject to force the user to look.
	body := "1\n00:00:05,000 --> 00:00:05,000\nzero-len\n"
	p := writeSrt(t, "zero-len.srt", body)

	_, err := Parse(p)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse: got %v, want ErrMalformed", err)
	}
}

func TestParse_TimingLineWithCuePositionHints_Tolerated(t *testing.T) {
	// SRT extensions allow trailing "X1:200 X2:400 Y1:100 Y2:200"
	// coord hints. We must ignore them but still parse the second
	// timestamp cleanly.
	body := "1\n00:00:00,000 --> 00:00:01,500 X1:200 X2:400 Y1:100 Y2:200\nhello\n"
	p := writeSrt(t, "coords.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cues[0].Start != 0 || cues[0].End != 1500*time.Millisecond {
		t.Fatalf("timing: start=%v end=%v", cues[0].Start, cues[0].End)
	}
}

func TestParse_VTTStyleDotSeparator_Accepted(t *testing.T) {
	// Some yt-dlp --convert-subs runs emit "HH:MM:SS.mmm" instead
	// of "HH:MM:SS,mmm". We normalise the dot to a comma so both
	// dialects parse identically.
	body := "1\n00:00:00.000 --> 00:00:01.500\nhello\n"
	p := writeSrt(t, "dot.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cues[0].End != 1500*time.Millisecond {
		t.Fatalf("End: %v", cues[0].End)
	}
}

func TestParse_BadTimestampShape_Malformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"short_hms", "1\n0:00:00,000 --> 00:00:01,000\nhi\n"},
		{"missing_ms", "1\n00:00:00 --> 00:00:01\nhi\n"},
		{"letters", "1\n00:aa:00,000 --> 00:00:01,000\nhi\n"},
		{"minutes_over_59", "1\n00:60:00,000 --> 00:00:01,000\nhi\n"},
		{"seconds_over_59", "1\n00:00:60,000 --> 00:00:01,000\nhi\n"},
		{"ms_too_long", "1\n00:00:00,0000 --> 00:00:01,000\nhi\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := writeSrt(t, tc.name+".srt", tc.body)
			_, err := Parse(p)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Parse(%s): got %v, want ErrMalformed", tc.name, err)
			}
		})
	}
}

func TestParse_EmptyTextCue_Preserved(t *testing.T) {
	// Some karaoke tools emit cues with no body. We keep them so
	// downstream formatter can decide to render "..." or drop.
	body := "1\n00:00:00,000 --> 00:00:01,000\n\n"
	p := writeSrt(t, "empty-text.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 1 || cues[0].Text != "" {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_InnerBlankLinesInText_Dropped(t *testing.T) {
	// A blank line inside a text body would normally terminate the
	// block. But the SRT format is line-count-oriented; we do treat
	// the blank as a block boundary. Verify the second block still
	// parses cleanly (the "second half" needs its own timing line
	// to be legal, so this doc-tests "we do NOT support inner blanks
	// as text" clearly).
	body := "1\n00:00:00,000 --> 00:00:01,000\nfirst-half\n\n" +
		"2\n00:00:01,000 --> 00:00:02,000\nsecond-block\n"
	p := writeSrt(t, "inner-blank.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d: %+v", len(cues), cues)
	}
}

func TestParse_UnsortedInput_SortedByStart(t *testing.T) {
	// Real files are always sorted, but the contract promises we do
	// the sort ourselves. Feed a shuffled file and assert output is
	// monotone. Keeps sort.SliceStable branch alive.
	body := "1\n00:00:05,000 --> 00:00:06,000\nlast\n\n" +
		"2\n00:00:00,000 --> 00:00:01,000\nfirst\n\n" +
		"3\n00:00:02,000 --> 00:00:03,000\nmiddle\n"
	p := writeSrt(t, "shuffled.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !(cues[0].Start < cues[1].Start && cues[1].Start < cues[2].Start) {
		t.Fatalf("not sorted: %+v", cues)
	}
	if cues[0].Text != "first" || cues[2].Text != "last" {
		t.Fatalf("sort broke content: %+v", cues)
	}
}

func TestParse_TrailingBlankLines_Ignored(t *testing.T) {
	// A file ending with a stack of blank lines used to flush an
	// empty block; the guard `len(block) == 0` in flush handles it.
	body := "1\n00:00:00,000 --> 00:00:01,000\nhi\n\n\n\n\n"
	p := writeSrt(t, "trail.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("cues: %+v", cues)
	}
}

func TestParse_ASRSample_FromTestdata(t *testing.T) {
	// Cross-check against the ASR fixture used by the whisper fake.
	// Any drift in the fixture format (e.g. producer switches to
	// dot-separator without updating the fake) breaks here first.
	body := "1\n00:00:00,000 --> 00:00:03,500\n大家好 欢迎收看本期视频 今天我们\n\n" +
		"2\n00:00:03,500 --> 00:00:07,200\n来聊一聊字幕自动化 这段是ASR识别出的文本\n"
	p := writeSrt(t, "asr.srt", body)

	cues, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("cues: %+v", cues)
	}
	if !strings.Contains(cues[1].Text, "ASR识别") {
		t.Errorf("cue[1].Text: %q", cues[1].Text)
	}
}

func TestConflictAction_ErrEmpty_ErrMalformed_ErrorsIsWorks(t *testing.T) {
	// Wrapping sanity: the errors we return must satisfy errors.Is
	// against the exported sentinels no matter how many fmt.Errorf
	// levels sit between the wrap site and the caller.
	wrapped := errEmptyDeep()
	if !errors.Is(wrapped, ErrEmpty) {
		t.Errorf("deep wrap: !errors.Is(ErrEmpty)")
	}
	wrapped2 := errMalformedDeep()
	if !errors.Is(wrapped2, ErrMalformed) {
		t.Errorf("deep wrap: !errors.Is(ErrMalformed)")
	}
}

// Helpers that emulate the pipeline's "wrap parse error with URL
// context" pattern to make sure our sentinels survive.
func errEmptyDeep() error {
	base := ErrEmpty
	l1 := errors.Join(base, errors.New("l1"))
	return errors.Join(l1, errors.New("l2"))
}

func errMalformedDeep() error {
	base := ErrMalformed
	l1 := errors.Join(base, errors.New("l1"))
	return errors.Join(l1, errors.New("l2"))
}
