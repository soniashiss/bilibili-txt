package formatter

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bilibili-txt/internal/subtitle"
)

// mkCues returns a canonical 3-cue slice used across the tests. Kept
// as a helper (not a package-level var) so each test gets its own
// copy — Cue is a small value struct, no risk of aliasing but we
// keep the pattern consistent with the parser tests.
func mkCues() []subtitle.Cue {
	return []subtitle.Cue{
		{Index: 1, Start: 0, End: 2500 * time.Millisecond, Text: "大家好，欢迎收看本期视频。"},
		{Index: 2, Start: 2600 * time.Millisecond, End: 5200 * time.Millisecond, Text: "今天我们来聊一聊字幕自动化。"},
		{Index: 3, Start: 5300 * time.Millisecond, End: 8000 * time.Millisecond, Text: "这段文字用来验证解析流程。"},
	}
}

func TestConvert_EmptyCues_ReturnsErrEmpty(t *testing.T) {
	// Reuses subtitle.ErrEmpty so pipeline error paths converge on
	// a single sentinel across parse / render. The empty guard must
	// short-circuit BEFORE os.Create for every format — otherwise a
	// zero-byte phantom file would leak into the output directory.
	cases := []struct {
		name   string
		format Format
	}{
		{"txt", FormatTXT},
		{"md", FormatMD},
		{"srt", FormatSRT},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "empty."+tc.name)
			err := Convert(nil, out, tc.format)
			if !errors.Is(err, subtitle.ErrEmpty) {
				t.Fatalf("Convert(nil, %s): got %v, want subtitle.ErrEmpty", tc.format, err)
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("format %s created a phantom file, stat err=%v", tc.format, err)
			}
		})
	}
}

func TestConvert_UnknownFormat_ReturnsErrUnknownFormat(t *testing.T) {
	// Contract: unknown format must NOT touch the filesystem.
	// [Convert] validates the enum before os.Create so we never
	// leave a phantom zero-byte file behind.
	out := filepath.Join(t.TempDir(), "bad.xyz")
	err := Convert(mkCues(), out, Format("xyz"))
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Convert(bad format): got %v, want ErrUnknownFormat", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bad format touched the filesystem: %v", err)
	}
}

func TestConvert_CreateFails_MissingParentDir(t *testing.T) {
	// Parent directory doesn't exist ⇒ os.Create fails. Contract
	// says [Convert] does NOT MkdirAll — it surfaces the raw
	// ENOENT-wrapped error so the pipeline log is unambiguous.
	out := filepath.Join(t.TempDir(), "nope-subdir", "x.txt")
	err := Convert(mkCues(), out, FormatTXT)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Convert(missing parent): got %v, want wrapped os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "formatter: create") {
		t.Errorf("error missing context: %v", err)
	}
}

func TestConvert_TXT_OneLinePerCue(t *testing.T) {
	// Contract: one non-empty cue → one output line, caller order
	// preserved. The three fixture cues each become their own line;
	// existing sentence-terminating punctuation stays inline as part
	// of the cue text.
	out := filepath.Join(t.TempDir(), "sample.txt")
	if err := Convert(mkCues(), out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "大家好，欢迎收看本期视频。\n" +
		"今天我们来聊一聊字幕自动化。\n" +
		"这段文字用来验证解析流程。\n"
	if string(b) != want {
		t.Fatalf("TXT mismatch:\n got: %q\nwant: %q", b, want)
	}
}

func TestConvert_TXT_CueBoundaryIsLineBreak(t *testing.T) {
	// Regression guard for the ai-zh "no-punctuation" case: a track
	// whose cues carry zero sentence terminators must still produce
	// one line per cue. Under the old sentenceEnders-based writer
	// this collapsed into a single unreadable line.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 1 * time.Second, Text: "第一段没有标点"},
		{Index: 2, Start: 1 * time.Second, End: 2 * time.Second, Text: "第二段也没有标点"},
		{Index: 3, Start: 2 * time.Second, End: 3 * time.Second, Text: "第三段依旧没有标点"},
	}
	out := filepath.Join(t.TempDir(), "ai-zh.txt")
	if err := Convert(cues, out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	want := "第一段没有标点\n" +
		"第二段也没有标点\n" +
		"第三段依旧没有标点\n"
	if string(got) != want {
		t.Fatalf("TXT no-punctuation mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestConvert_TXT_TrimsPerCueWhitespace(t *testing.T) {
	// CC exports occasionally pad cue text with leading / trailing
	// spaces (screen-fit wrap artefact). The TXT writer trims them
	// per line so the transcript stays clean.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 1 * time.Second, Text: "  leading and trailing spaces  "},
	}
	out := filepath.Join(t.TempDir(), "trim.txt")
	if err := Convert(cues, out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "leading and trailing spaces\n" {
		t.Fatalf("TXT trim mismatch:\n got: %q", got)
	}
}

func TestConvert_TXT_MultipleSentencesInOneCue_StayInline(t *testing.T) {
	// New contract: internal punctuation is NOT a split point.
	// Whisper batches can pack several sentences into one caption
	// row; that batching is a semantic unit we keep intact so
	// downstream chunkers see the same block the video showed.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 2 * time.Second, Text: "第一句。第二句！第三句？"},
	}
	out := filepath.Join(t.TempDir(), "multi.txt")
	if err := Convert(cues, out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	want := "第一句。第二句！第三句？\n"
	if string(got) != want {
		t.Fatalf("TXT multi-sentence mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestConvert_TXT_UnterminatedCueStillEmits(t *testing.T) {
	// A cue whose text carries no terminator must still land in the
	// output — a lost line is worse than a missing period.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 1 * time.Second, Text: "unfinished thought"},
	}
	out := filepath.Join(t.TempDir(), "trunc.txt")
	if err := Convert(cues, out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "unfinished thought\n" {
		t.Fatalf("truncated cue lost:\n got: %q", got)
	}
}

func TestConvert_TXT_EmptyCueSkipped(t *testing.T) {
	// Empty cue text carries no signal for TXT and is skipped.
	// Neighbouring cues stay on their own lines; no phantom blank
	// line appears where the empty cue was.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 1 * time.Second, Text: "开头。"},
		{Index: 2, Start: 1 * time.Second, End: 2 * time.Second, Text: ""},
		{Index: 3, Start: 2 * time.Second, End: 3 * time.Second, Text: "结尾。"},
	}
	out := filepath.Join(t.TempDir(), "gap.txt")
	if err := Convert(cues, out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	want := "开头。\n结尾。\n"
	if string(got) != want {
		t.Fatalf("TXT with empty cue:\n got: %q\nwant: %q", got, want)
	}
}

func TestConvert_MD_ChapterStubs(t *testing.T) {
	// MD: each cue is its own paragraph with an [mm:ss] anchor.
	// Blank line between cues turns them into distinct
	// CommonMark paragraphs; a single trailing LF closes the file.
	out := filepath.Join(t.TempDir(), "sample.md")
	if err := Convert(mkCues(), out, FormatMD); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, _ := os.ReadFile(out)
	want := "[00:00] 大家好，欢迎收看本期视频。\n\n" +
		"[00:02] 今天我们来聊一聊字幕自动化。\n\n" +
		"[00:05] 这段文字用来验证解析流程。\n"
	if string(b) != want {
		t.Fatalf("MD mismatch:\n got: %q\nwant: %q", b, want)
	}
}

func TestConvert_MD_HourPrefix_AppearsPastOneHour(t *testing.T) {
	// The mm:ss form collapses to HH:MM:SS once any cue crosses
	// the 1-hour mark. Videos over an hour benefit from the extra
	// segment; shorter clips stay compact.
	cues := []subtitle.Cue{
		{Index: 1, Start: 3661 * time.Second, End: 3670 * time.Second, Text: "hour+one"},
	}
	out := filepath.Join(t.TempDir(), "hour.md")
	if err := Convert(cues, out, FormatMD); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !strings.HasPrefix(string(got), "[01:01:01] hour+one") {
		t.Fatalf("MD hour prefix wrong:\n%s", got)
	}
}

func TestConvert_MD_EmptyText_RendersSilentMarker(t *testing.T) {
	// Empty cue text becomes `_(silent)_` so the markdown still has
	// a body — an empty `[mm:ss] ` block would look like a broken
	// anchor when rendered.
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 1 * time.Second, Text: ""},
	}
	out := filepath.Join(t.TempDir(), "silent.md")
	if err := Convert(cues, out, FormatMD); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "[00:00] _(silent)_\n" {
		t.Fatalf("silent marker:\n got: %q", got)
	}
}

func TestConvert_SRT_PreservesSourceIndex(t *testing.T) {
	// Contract §三.2: SRT is 原样透传. The output MUST preserve
	// the source Cue.Index rather than renumbering.
	out := filepath.Join(t.TempDir(), "sample.srt")
	if err := Convert(mkCues(), out, FormatSRT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, _ := os.ReadFile(out)
	want := "1\r\n00:00:00,000 --> 00:00:02,500\r\n大家好，欢迎收看本期视频。\r\n\r\n" +
		"2\r\n00:00:02,600 --> 00:00:05,200\r\n今天我们来聊一聊字幕自动化。\r\n\r\n" +
		"3\r\n00:00:05,300 --> 00:00:08,000\r\n这段文字用来验证解析流程。\r\n\r\n"
	if string(b) != want {
		t.Fatalf("SRT mismatch:\n got: %q\nwant: %q", b, want)
	}
}

func TestConvert_SRT_KeepsNonDenseIndicesFromSource(t *testing.T) {
	// If upstream fed us cues with sparse indices (e.g. the parser
	// synthesised numbers or a filter removed some cues while
	// keeping the originals), the SRT writer preserves them
	// verbatim — dense re-numbering is a caller-side concern.
	cues := []subtitle.Cue{
		{Index: 42, Start: 0, End: 1 * time.Second, Text: "first"},
		{Index: 99, Start: 2 * time.Second, End: 3 * time.Second, Text: "second"},
	}
	out := filepath.Join(t.TempDir(), "sparse.srt")
	if err := Convert(cues, out, FormatSRT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "42\r\n") || !strings.Contains(string(b), "99\r\n") {
		t.Fatalf("source indices dropped:\n%s", b)
	}
}

func TestConvert_SRT_HourCap(t *testing.T) {
	// Anything past 99h clamps to 99h so the HH:MM:SS,mmm width
	// stays constant — a byte-offset assumption some strict tools
	// bake in.
	cues := []subtitle.Cue{
		{Index: 1, Start: 100 * time.Hour, End: 100*time.Hour + time.Second, Text: "far future"},
	}
	out := filepath.Join(t.TempDir(), "far.srt")
	if err := Convert(cues, out, FormatSRT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "99:") {
		t.Fatalf("hour cap not applied:\n%s", b)
	}
}

func TestFormatTimestampMD_ShapesAndClamp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00"},
		{500 * time.Millisecond, "00:00"},
		{59 * time.Second, "00:59"},
		{60 * time.Second, "01:00"},
		{75*time.Second + 400*time.Millisecond, "01:15"},
		{time.Hour, "01:00:00"},
		{time.Hour + 5*time.Minute + 3*time.Second, "01:05:03"},
		{-500 * time.Millisecond, "00:00"}, // clamp
	}
	for _, c := range cases {
		if got := formatTimestampMD(c.in); got != c.want {
			t.Errorf("formatTimestampMD(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatTimestampSRT_ShapesAndClamp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00:00,000"},
		{1 * time.Millisecond, "00:00:00,001"},
		{999 * time.Millisecond, "00:00:00,999"},
		{time.Second, "00:00:01,000"},
		{time.Hour + 2*time.Minute + 3*time.Second + 456*time.Millisecond, "01:02:03,456"},
		{100 * time.Hour, "99:00:00,000"}, // clamp
		{-500 * time.Millisecond, "00:00:00,000"},
	}
	for _, c := range cases {
		if got := formatTimestampSRT(c.in); got != c.want {
			t.Errorf("formatTimestampSRT(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatConstants_MatchCLIStrings(t *testing.T) {
	// The Format enum values MUST match the strings the CLI /
	// config layer accept — otherwise --format=txt at the shell
	// won't map to FormatTXT here. This is a pure regression
	// guard; any accidental rename fires here first.
	if string(FormatTXT) != "txt" || string(FormatMD) != "md" || string(FormatSRT) != "srt" {
		t.Fatalf("Format constant drift: %q %q %q", FormatTXT, FormatMD, FormatSRT)
	}
}

func TestWriteTXT_WriterErrors_Propagate(t *testing.T) {
	// Faulty writer surfaces as a wrapped error. Uses a stub that
	// fails on every Write so we cover the error path without any
	// filesystem interaction.
	err := writeTXT(&failingWriter{}, mkCues())
	if err == nil {
		t.Fatal("writeTXT should propagate writer error")
	}
	if !strings.Contains(err.Error(), "formatter: txt") {
		t.Errorf("error missing context: %v", err)
	}
}

func TestWriteMD_WriterErrors_Propagate(t *testing.T) {
	err := writeMD(&failingWriter{}, mkCues())
	if err == nil {
		t.Fatal("writeMD should propagate writer error")
	}
	if !strings.Contains(err.Error(), "formatter: md") {
		t.Errorf("error missing context: %v", err)
	}
}

func TestWriteSRT_WriterErrors_Propagate(t *testing.T) {
	err := writeSRT(&failingWriter{}, mkCues())
	if err == nil {
		t.Fatal("writeSRT should propagate writer error")
	}
	if !strings.Contains(err.Error(), "formatter: srt") {
		t.Errorf("error missing context: %v", err)
	}
}

func TestConvert_OverwriteExisting(t *testing.T) {
	// Convert MUST truncate + rewrite existing files. The pipeline
	// runs ResolveConflict upstream — by the time we get here, the
	// caller has already agreed to overwrite.
	out := filepath.Join(t.TempDir(), "over.txt")
	if err := os.WriteFile(out, []byte("stale content that should be gone"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Convert(mkCues(), out, FormatTXT); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	b, _ := os.ReadFile(out)
	if bytes.Contains(b, []byte("stale")) {
		t.Fatalf("existing content not overwritten:\n%s", b)
	}
}

func TestConvert_PreservesInputOrder_NoResort(t *testing.T) {
	// Contract in formatter.go: "Convert does no re-sorting". The
	// pipeline layer trusts this so a subtitle.Parse → Convert
	// round-trip keeps cues in the caller's order, even when Start
	// values happen to be out-of-order (e.g. an upstream filter
	// injected a preface cue after chunking). Without this guard,
	// a well-meaning `sort.Slice(cues, ...)` inside Convert would
	// silently break TestConvert_SRT_KeepsNonDenseIndicesFromSource
	// and any future SRT-round-trip test.
	//
	// We deliberately feed cues whose Start values are NOT
	// monotone so a hidden sort would be visible in the output.
	cues := []subtitle.Cue{
		{Index: 10, Start: 5 * time.Second, End: 6 * time.Second, Text: "gamma."},
		{Index: 20, Start: 1 * time.Second, End: 2 * time.Second, Text: "alpha."},
		{Index: 30, Start: 9 * time.Second, End: 10 * time.Second, Text: "beta."},
	}

	// TXT: line order is a direct proxy for cue order — one line
	// per cue (see writeTXT), so a hidden sort would be immediately
	// visible in the emitted line sequence.
	txtOut := filepath.Join(t.TempDir(), "order.txt")
	if err := Convert(cues, txtOut, FormatTXT); err != nil {
		t.Fatalf("Convert TXT: %v", err)
	}
	got, _ := os.ReadFile(txtOut)
	wantTXT := "gamma.\nalpha.\nbeta.\n"
	if string(got) != wantTXT {
		t.Fatalf("TXT re-sorted:\n got: %q\nwant: %q", got, wantTXT)
	}

	// SRT: index appears in the exact order we fed. If Convert
	// re-sorted by Start, the "20" block would appear first.
	srtOut := filepath.Join(t.TempDir(), "order.srt")
	if err := Convert(cues, srtOut, FormatSRT); err != nil {
		t.Fatalf("Convert SRT: %v", err)
	}
	srtGot, _ := os.ReadFile(srtOut)
	firstIdx := strings.Index(string(srtGot), "10\r\n")
	secondIdx := strings.Index(string(srtGot), "20\r\n")
	thirdIdx := strings.Index(string(srtGot), "30\r\n")
	if firstIdx < 0 || secondIdx < 0 || thirdIdx < 0 {
		t.Fatalf("SRT missing indices:\n%s", srtGot)
	}
	if !(firstIdx < secondIdx && secondIdx < thirdIdx) {
		t.Fatalf("SRT re-sorted (expected order 10,20,30 by offset):\n%s", srtGot)
	}

	// MD: same story, verified by the mm:ss anchors landing in the
	// caller-supplied order (5s → 1s → 9s rather than sorted).
	mdOut := filepath.Join(t.TempDir(), "order.md")
	if err := Convert(cues, mdOut, FormatMD); err != nil {
		t.Fatalf("Convert MD: %v", err)
	}
	mdGot, _ := os.ReadFile(mdOut)
	firstMD := strings.Index(string(mdGot), "[00:05]")
	secondMD := strings.Index(string(mdGot), "[00:01]")
	thirdMD := strings.Index(string(mdGot), "[00:09]")
	if firstMD < 0 || secondMD < 0 || thirdMD < 0 {
		t.Fatalf("MD missing timestamps:\n%s", mdGot)
	}
	if !(firstMD < secondMD && secondMD < thirdMD) {
		t.Fatalf("MD re-sorted:\n%s", mdGot)
	}
}

// failingWriter is an io.Writer that always errors — used to exercise
// the write-error branch of each writer without touching disk.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}
