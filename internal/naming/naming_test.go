package naming

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Slug: whitelist (Han + Latin + ASCII digit)
// ---------------------------------------------------------------------------

func TestSlug_Empty_ReturnsEmpty(t *testing.T) {
	if got := Slug(""); got != "" {
		t.Fatalf("Slug(\"\") = %q, want empty", got)
	}
}

func TestSlug_WhitespaceOnly_ReturnsEmpty(t *testing.T) {
	if got := Slug("   \t\n  "); got != "" {
		t.Fatalf("Slug(whitespace) = %q, want empty", got)
	}
}

func TestSlug_KeepsAsciiLetters(t *testing.T) {
	if got := Slug("HelloWorld"); got != "HelloWorld" {
		t.Fatalf("Slug(HelloWorld) = %q, want HelloWorld", got)
	}
}

func TestSlug_KeepsAsciiDigits(t *testing.T) {
	if got := Slug("abc123"); got != "abc123" {
		t.Fatalf("Slug(abc123) = %q, want abc123", got)
	}
}

func TestSlug_KeepsChinese(t *testing.T) {
	if got := Slug("视频标题"); got != "视频标题" {
		t.Fatalf("Slug(视频标题) = %q, want 视频标题", got)
	}
}

func TestSlug_KeepsMixedChineseLatinDigit(t *testing.T) {
	if got := Slug("干货Rust所有权2024"); got != "干货Rust所有权2024" {
		t.Fatalf("Slug(mixed) = %q, want 干货Rust所有权2024", got)
	}
}

func TestSlug_StripsBrackets(t *testing.T) {
	if got := Slug("【干货】5分钟"); got != "干货5分钟" {
		t.Fatalf("Slug(brackets) = %q, want 干货5分钟", got)
	}
}

func TestSlug_StripsFilesystemIllegalChars(t *testing.T) {
	// plan §4.3: strip / \ ? % * : | " < >
	if got := Slug(`a/b\c?d%e*f:g|h"i<j>k`); got != "abcdefghijk" {
		t.Fatalf("Slug(illegal chars) = %q, want abcdefghijk", got)
	}
}

func TestSlug_StripsEmoji(t *testing.T) {
	if got := Slug("hello🎉world🚀"); got != "helloworld" {
		t.Fatalf("Slug(emoji) = %q, want helloworld", got)
	}
}

func TestSlug_StripsControlChars(t *testing.T) {
	if got := Slug("he\x01l\x1flo"); got != "hello" {
		t.Fatalf("Slug(control) = %q, want hello", got)
	}
}

func TestSlug_StripsPunctuation(t *testing.T) {
	if got := Slug("hello!world?foo.bar,baz;qux"); got != "helloworldfoobarbazqux" {
		t.Fatalf("Slug(punct) = %q, want helloworldfoobarbazqux", got)
	}
}

func TestSlug_StripsUnderscoreItself(t *testing.T) {
	// '_' is punctuation-connector — not in whitelist, so dropped.
	// This exercises the "首尾下划线 → 去掉" rule against literal underscores in input.
	if got := Slug("__hello__"); got != "hello" {
		t.Fatalf("Slug(__hello__) = %q, want hello", got)
	}
}

// ---------------------------------------------------------------------------
// Slug: whitespace folding
// ---------------------------------------------------------------------------

func TestSlug_FoldsSingleSpaceToUnderscore(t *testing.T) {
	if got := Slug("hello world"); got != "hello_world" {
		t.Fatalf("Slug(single space) = %q, want hello_world", got)
	}
}

func TestSlug_FoldsRunOfWhitespaceToOneUnderscore(t *testing.T) {
	if got := Slug("a   \t \n b"); got != "a_b" {
		t.Fatalf("Slug(mixed ws run) = %q, want a_b", got)
	}
}

func TestSlug_TrimsLeadingWhitespace(t *testing.T) {
	if got := Slug("   hello"); got != "hello" {
		t.Fatalf("Slug(leading ws) = %q, want hello", got)
	}
}

func TestSlug_TrimsTrailingWhitespace(t *testing.T) {
	if got := Slug("hello   "); got != "hello" {
		t.Fatalf("Slug(trailing ws) = %q, want hello", got)
	}
}

func TestSlug_TrimsWhitespaceThatBoundsStrippedChars(t *testing.T) {
	// "  !!!  hello  ???  " — leading/trailing whitespace and stripped
	// punctuation should NOT produce leading/trailing underscore.
	if got := Slug("  !!!  hello  ???  "); got != "hello" {
		t.Fatalf("Slug(padded stripped) = %q, want hello", got)
	}
}

func TestSlug_TrimsUnderscoreLeftBehindByTruncation(t *testing.T) {
	// Build a title of 51 runes where the 51st is a whitespace-induced underscore
	// preceded by 50 useful runes; make sure the truncated result does not
	// end on '_'.
	var b strings.Builder
	for i := 0; i < 49; i++ {
		b.WriteRune('a')
	}
	// 49 'a' + 1 whitespace + 'b' 'c'... — at 50 runes the truncation lands on
	// the folded '_' — the trim rule must drop it.
	b.WriteRune('a') // 50th rune = 'a'
	b.WriteRune(' ') // becomes '_' → 51st rune, must be truncated away
	b.WriteRune('b') // 52nd rune, must be truncated away
	got := Slug(b.String())
	if utf8.RuneCountInString(got) != 50 {
		t.Fatalf("truncated slug rune-count = %d, want 50", utf8.RuneCountInString(got))
	}
	if strings.HasSuffix(got, "_") {
		t.Fatalf("truncated slug %q ends in underscore, want trimmed", got)
	}
}

// ---------------------------------------------------------------------------
// Slug: length truncation (rune-based, not byte-based)
// ---------------------------------------------------------------------------

func TestSlug_TruncatesAt50Runes(t *testing.T) {
	// 60 Han characters — every rune is 3 bytes in UTF-8.
	title := strings.Repeat("字", 60)
	got := Slug(title)
	if n := utf8.RuneCountInString(got); n != 50 {
		t.Fatalf("Slug(60 han) rune-count = %d, want 50", n)
	}
	if got != strings.Repeat("字", 50) {
		t.Fatalf("Slug(60 han) = %q, want %q", got, strings.Repeat("字", 50))
	}
}

func TestSlug_ExactlyFiftyRunes_Unchanged(t *testing.T) {
	title := strings.Repeat("A", 50)
	if got := Slug(title); got != title {
		t.Fatalf("Slug(50A) = %q, want %q", got, title)
	}
}

func TestSlug_UnderFiftyRunes_Unchanged(t *testing.T) {
	title := strings.Repeat("A", 10)
	if got := Slug(title); got != title {
		t.Fatalf("Slug(10A) = %q, want %q", got, title)
	}
}

func TestSlug_TruncationIsRunesNotBytes(t *testing.T) {
	// 30 Han (90 bytes) — well under 50 runes but well over 50 bytes.
	// Result must be unchanged; a byte-based cut would corrupt UTF-8.
	title := strings.Repeat("字", 30)
	got := Slug(title)
	if got != title {
		t.Fatalf("Slug(30 han) = %q, want %q (byte-based cut leaked)", got, title)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("Slug produced invalid UTF-8: % x", got)
	}
}

// ---------------------------------------------------------------------------
// BuildPath: composition of slug + bvid + optional .pN + ext
// ---------------------------------------------------------------------------

func TestBuildPath_SinglePage_NoPageSuffix(t *testing.T) {
	got := BuildPath("/out", "hello world", "BV1xx", 1, 1, "txt")
	want := filepath.Join("/out", "hello_world__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(single page) = %q, want %q", got, want)
	}
}

func TestBuildPath_MultiPage_AddsPageSuffix(t *testing.T) {
	got := BuildPath("/out", "hello", "BV1xx", 2, 5, "md")
	want := filepath.Join("/out", "hello__BV1xx.p2.md")
	if got != want {
		t.Fatalf("BuildPath(multi page) = %q, want %q", got, want)
	}
}

func TestBuildPath_MultiPage_FirstPage_StillGetsSuffix(t *testing.T) {
	// If totalPages > 1, every page (even page=1) gets a suffix so users can
	// see at a glance the file is one of many parts.
	got := BuildPath("/out", "hello", "BV1xx", 1, 3, "srt")
	want := filepath.Join("/out", "hello__BV1xx.p1.srt")
	if got != want {
		t.Fatalf("BuildPath(multi page/p1) = %q, want %q", got, want)
	}
}

func TestBuildPath_EmptyTitle_FallsBackToBvid(t *testing.T) {
	got := BuildPath("/out", "", "BV42", 1, 1, "txt")
	want := filepath.Join("/out", "BV42__BV42.txt")
	if got != want {
		t.Fatalf("BuildPath(empty title) = %q, want %q", got, want)
	}
}

func TestBuildPath_TitleStrippedToEmpty_FallsBackToBvid(t *testing.T) {
	// All chars strippable → slug is "" → fallback engages.
	got := BuildPath("/out", "!!!🎉🎉🎉", "BV42", 1, 1, "txt")
	want := filepath.Join("/out", "BV42__BV42.txt")
	if got != want {
		t.Fatalf("BuildPath(strippable title) = %q, want %q", got, want)
	}
}

func TestBuildPath_ExtWithLeadingDot_NotDoubled(t *testing.T) {
	got := BuildPath("/out", "hello", "BV1xx", 1, 1, ".txt")
	want := filepath.Join("/out", "hello__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(ext with dot) = %q, want %q", got, want)
	}
}

func TestBuildPath_EmptyExt_NoTrailingDot(t *testing.T) {
	got := BuildPath("/out", "hello", "BV1xx", 1, 1, "")
	want := filepath.Join("/out", "hello__BV1xx")
	if got != want {
		t.Fatalf("BuildPath(empty ext) = %q, want %q", got, want)
	}
}

func TestBuildPath_UsesSlugForTitle(t *testing.T) {
	got := BuildPath("/out", "【干货】5分钟", "BV1xx", 1, 1, "txt")
	want := filepath.Join("/out", "干货5分钟__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(slugged) = %q, want %q", got, want)
	}
}

func TestBuildPath_RelativeDir_PreservesDir(t *testing.T) {
	got := BuildPath("transcripts", "hello", "BV1xx", 1, 1, "txt")
	want := filepath.Join("transcripts", "hello__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(relative dir) = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// ResolveConflict: strategy-first dispatch
// ---------------------------------------------------------------------------

func TestResolveConflict_Overwrite_Direct(t *testing.T) {
	got, err := ResolveConflict("/out/x.txt", Options{Strategy: "overwrite"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionOverwrite {
		t.Fatalf("got %v, want ActionOverwrite", got)
	}
}

func TestResolveConflict_Skip_Direct(t *testing.T) {
	got, err := ResolveConflict("/out/x.txt", Options{Strategy: "skip"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionSkip {
		t.Fatalf("got %v, want ActionSkip", got)
	}
}

func TestResolveConflict_Abort_Direct(t *testing.T) {
	got, err := ResolveConflict("/out/x.txt", Options{Strategy: "abort"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestResolveConflict_AskTTY_PromptOverwrite(t *testing.T) {
	fp := &fakePrompter{next: ActionOverwrite}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: fp,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionOverwrite {
		t.Fatalf("got %v, want ActionOverwrite", got)
	}
	if fp.calls != 1 {
		t.Fatalf("Prompter.Ask called %d times, want 1", fp.calls)
	}
	if fp.lastPath != "/out/x.txt" {
		t.Fatalf("Prompter received path %q, want /out/x.txt", fp.lastPath)
	}
}

func TestResolveConflict_AskTTY_PromptSkip(t *testing.T) {
	fp := &fakePrompter{next: ActionSkip}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: fp,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionSkip {
		t.Fatalf("got %v, want ActionSkip", got)
	}
}

func TestResolveConflict_AskTTY_PromptAbort(t *testing.T) {
	fp := &fakePrompter{next: ActionAbort}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: fp,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestResolveConflict_AskNonTTY_ReturnsAbortWithHint(t *testing.T) {
	fp := &fakePrompter{next: ActionOverwrite} // must NOT be called
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    false,
		Prompter: fp,
	})
	if err == nil {
		t.Fatalf("expected error hinting --overwrite/--skip, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
	if fp.calls != 0 {
		t.Fatalf("Prompter.Ask called %d times, want 0 (non-TTY)", fp.calls)
	}
	if !strings.Contains(err.Error(), "--overwrite") || !strings.Contains(err.Error(), "--skip") {
		t.Fatalf("error %q missing --overwrite/--skip hint", err)
	}
}

func TestResolveConflict_AskNoInteractive_ReturnsAbortWithHint(t *testing.T) {
	fp := &fakePrompter{next: ActionOverwrite} // must NOT be called
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy:      "ask",
		IsTTY:         true, // even a TTY: --no-interactive wins
		NoInteractive: true,
		Prompter:      fp,
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
	if fp.calls != 0 {
		t.Fatalf("Prompter.Ask called %d times, want 0 (--no-interactive)", fp.calls)
	}
}

func TestResolveConflict_EmptyStrategy_DefaultsToAsk(t *testing.T) {
	// config default is "ask" but be defensive: treat "" as ask.
	fp := &fakePrompter{next: ActionOverwrite}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "",
		IsTTY:    true,
		Prompter: fp,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionOverwrite {
		t.Fatalf("got %v, want ActionOverwrite", got)
	}
}

func TestResolveConflict_UnknownStrategy_ReturnsError(t *testing.T) {
	got, err := ResolveConflict("/out/x.txt", Options{Strategy: "burninate"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort on unknown strategy", got)
	}
	if !strings.Contains(err.Error(), "burninate") {
		t.Fatalf("error %q missing offending value", err)
	}
}

func TestResolveConflict_AskTTY_PrompterError_Propagated(t *testing.T) {
	sentinel := errors.New("io broken")
	fp := &fakePrompter{err: sentinel}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: fp,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping %v", err, sentinel)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort on prompter error", got)
	}
}

func TestResolveConflict_AskTTY_NoPrompter_ReturnsAbortErr(t *testing.T) {
	// Missing Prompter when we would need one — treat as programmer bug,
	// don't crash, don't silently pick an action.
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: nil,
	})
	if err == nil {
		t.Fatalf("expected error on missing Prompter, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

// ---------------------------------------------------------------------------
// StdinPrompter: parse user input from an io.Reader
// ---------------------------------------------------------------------------

func TestStdinPrompter_ReadsOverwrite(t *testing.T) {
	p := &StdinPrompter{In: strings.NewReader("O\n"), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionOverwrite {
		t.Fatalf("got %v, want ActionOverwrite", got)
	}
}

func TestStdinPrompter_ReadsSkip(t *testing.T) {
	p := &StdinPrompter{In: strings.NewReader("s\n"), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionSkip {
		t.Fatalf("got %v, want ActionSkip", got)
	}
}

func TestStdinPrompter_ReadsAbort(t *testing.T) {
	p := &StdinPrompter{In: strings.NewReader("A\n"), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestStdinPrompter_EmptyInput_DefaultsToAbort(t *testing.T) {
	// Enter with no input → default is Abort per plan §4.3.
	p := &StdinPrompter{In: strings.NewReader("\n"), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestStdinPrompter_EOF_DefaultsToAbort(t *testing.T) {
	p := &StdinPrompter{In: strings.NewReader(""), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err != nil {
		t.Fatalf("unexpected err on EOF: %v", err)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestStdinPrompter_UnrecognizedInput_ReturnsAbortErr(t *testing.T) {
	p := &StdinPrompter{In: strings.NewReader("burninate\n"), Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err == nil {
		t.Fatalf("expected error on garbage input, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
}

func TestStdinPrompter_PromptWritten(t *testing.T) {
	var buf bytes.Buffer
	p := &StdinPrompter{In: strings.NewReader("A\n"), Out: &buf}
	if _, err := p.Ask("/out/existing.txt"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "/out/existing.txt") {
		t.Fatalf("prompt %q missing existing path", out)
	}
	for _, hint := range []string{"[O]", "[S]", "[A]"} {
		if !strings.Contains(out, hint) {
			t.Fatalf("prompt %q missing hint %s", out, hint)
		}
	}
}

// ---------------------------------------------------------------------------
// Review follow-ups (P2/P3)
// ---------------------------------------------------------------------------

// P2-1 / P2-3: Latin diacritics are NOT in the whitelist. Locking this
// behaviour down explicitly so a future "generously accept Latin" refactor
// doesn't silently ship without updating the package doc.
func TestSlug_LatinDiacritics_Stripped(t *testing.T) {
	if got := Slug("café"); got != "caf" {
		t.Fatalf("Slug(café) = %q, want caf (diacritics stripped)", got)
	}
	if got := Slug("naïve"); got != "nave" {
		t.Fatalf("Slug(naïve) = %q, want nave", got)
	}
	if got := Slug("München"); got != "Mnchen" {
		t.Fatalf("Slug(München) = %q, want Mnchen", got)
	}
}

// P2-1 / P2-3: any other non-Latin, non-Han scripts (Cyrillic, Hiragana,
// Katakana, Hangul, Arabic, …) also stripped — they're outside plan §4.3's
// "中英数" whitelist.
func TestSlug_NonHanNonLatinScripts_Stripped(t *testing.T) {
	// Cyrillic + Latin
	if got := Slug("Привет hello"); got != "hello" {
		t.Fatalf("Slug(cyrillic mix) = %q, want hello", got)
	}
	// Hiragana + Han (Han kept, hiragana stripped)
	if got := Slug("こんにちは世界"); got != "世界" {
		t.Fatalf("Slug(hiragana+han) = %q, want 世界", got)
	}
}

// P2-2: BuildPath is documented to receive an ext of [A-Za-z0-9]+ (or "" /
// leading dots). Anything with a path separator or that reduces to pure
// dots should not silently escape the output directory.
func TestBuildPath_InvalidExt_Rejected(t *testing.T) {
	cases := []struct {
		name string
		ext  string
	}{
		{"path separator", "txt/evil"},
		{"backslash", "txt\\evil"},
		{"single dot", "."},
		{"dot-dot", ".."},
		{"triple dot", "..."},
		{"space", "t xt"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for ext=%q, none", c.ext)
				}
			}()
			_ = BuildPath("/out", "hello", "BV1xx", 1, 1, c.ext)
		})
	}
}

// P2-2: bvid contract — reject anything with path traversal so that
// callers can't accidentally aim at /etc/passwd via a malicious API blob.
func TestBuildPath_InvalidBvid_Rejected(t *testing.T) {
	cases := []struct {
		name string
		bvid string
	}{
		{"empty", ""},
		{"path separator", "BV/xx"},
		{"dot-dot", ".."},
		{"parent traversal", "../etc/passwd"},
		{"whitespace", "BV 1xx"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for bvid=%q, none", c.bvid)
				}
			}()
			_ = BuildPath("/out", "hello", c.bvid, 1, 1, "txt")
		})
	}
}

// P3-2: multiple leading dots on ext should still yield a single-dot join.
func TestBuildPath_MultipleLeadingDotsInExt_Normalised(t *testing.T) {
	got := BuildPath("/out", "hello", "BV1xx", 1, 1, "...txt")
	want := filepath.Join("/out", "hello__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(...txt) = %q, want %q", got, want)
	}
}

// P3-3: Strategy is case- and whitespace-insensitive so YAML/CLI callers
// don't have to bikeshed over "ask" vs "Ask" vs "ASK  ".
func TestResolveConflict_Strategy_CaseAndWhitespaceInsensitive(t *testing.T) {
	cases := []struct {
		strat string
		want  ConflictAction
	}{
		{"OVERWRITE", ActionOverwrite},
		{" Skip ", ActionSkip},
		{"Abort\t", ActionAbort},
		{"  overWRite", ActionOverwrite},
	}
	for _, c := range cases {
		c := c
		t.Run(c.strat, func(t *testing.T) {
			got, err := ResolveConflict("/out/x.txt", Options{Strategy: c.strat})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Fatalf("Strategy=%q → %v, want %v", c.strat, got, c.want)
			}
		})
	}
}

// P3-4: EOF without trailing newline must still parse the buffered choice.
func TestStdinPrompter_EOFWithContent_ParsesChoice(t *testing.T) {
	cases := []struct {
		in   string
		want ConflictAction
	}{
		{"O", ActionOverwrite},
		{"s", ActionSkip},
		{"abort", ActionAbort},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			p := &StdinPrompter{In: strings.NewReader(c.in), Out: io.Discard}
			got, err := p.Ask("/out/x.txt")
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Fatalf("in=%q → %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// P3-6: Ask + !TTY vs Ask + --no-interactive should produce distinct hints
// so users can tell "wrong terminal setup" from "flag I passed".
func TestResolveConflict_NoInteractive_DistinctHint(t *testing.T) {
	_, errNoTTY := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    false,
	})
	_, errNoInter := ResolveConflict("/out/x.txt", Options{
		Strategy:      "ask",
		IsTTY:         true,
		NoInteractive: true,
	})
	if errNoTTY == nil || errNoInter == nil {
		t.Fatalf("expected both to error; got %v / %v", errNoTTY, errNoInter)
	}
	if errNoTTY.Error() == errNoInter.Error() {
		t.Fatalf("expected distinct hints, both = %q", errNoTTY)
	}
	if !strings.Contains(errNoInter.Error(), "no-interactive") &&
		!strings.Contains(errNoInter.Error(), "--no-interactive") {
		t.Fatalf("no-interactive hint %q should mention the flag", errNoInter)
	}
}

// P3-8: NewStdinPrompter must aim at os.Stdin/os.Stderr per plan §4.3
// ("prompt via stderr, don't pollute stdout").
func TestNewStdinPrompter_WiresStdinAndStderr(t *testing.T) {
	p := NewStdinPrompter()
	if p.In != os.Stdin {
		t.Fatalf("In = %v, want os.Stdin", p.In)
	}
	if p.Out != os.Stderr {
		t.Fatalf("Out = %v, want os.Stderr", p.Out)
	}
}

// P2-1: bvid contract — reject control characters (NUL, \x01, DEL, …)
// so a malicious metadata blob can't sneak in a filename that terminals
// interpret weirdly or that filesystems refuse.
func TestBuildPath_BvidControlChars_Rejected(t *testing.T) {
	cases := []struct {
		name string
		bvid string
	}{
		{"NUL", "BV\x001xx"},
		{"SOH", "BV\x011xx"},
		{"DEL", "BV\x7f1xx"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for bvid=%q, none", c.bvid)
				}
			}()
			_ = BuildPath("/out", "hello", c.bvid, 1, 1, "txt")
		})
	}
}

// P2-2: BuildPath page contract — when totalPages > 1, page must sit in
// [1, totalPages]. Zero, negative, and overflow values are contract
// violations, not user input, and should panic.
func TestBuildPath_InvalidPage_Rejected(t *testing.T) {
	cases := []struct {
		name             string
		page, totalPages int
	}{
		{"zero page in multi", 0, 3},
		{"negative page", -1, 3},
		{"page beyond total", 4, 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for page=%d/total=%d, none", c.page, c.totalPages)
				}
			}()
			_ = BuildPath("/out", "hello", "BV1xx", c.page, c.totalPages, "txt")
		})
	}
}

// P2-2 (continued): single-page videos accept any page value because the
// `.p<n>` segment is not emitted anyway — locking down that we do NOT
// panic in the single-page path even if the caller passes junk.
func TestBuildPath_SinglePage_PageValueIgnored(t *testing.T) {
	got := BuildPath("/out", "hello", "BV1xx", 0, 1, "txt")
	want := filepath.Join("/out", "hello__BV1xx.txt")
	if got != want {
		t.Fatalf("BuildPath(page=0/total=1) = %q, want %q", got, want)
	}
}

// P2-3: a broken Prompter that returns an out-of-range ConflictAction
// must not silently propagate — ResolveConflict falls back to Abort
// with a wrapping error so callers can tell "user picked X" from "code
// bug returned garbage".
func TestResolveConflict_AskTTY_PrompterReturnsInvalidAction(t *testing.T) {
	fp := &fakePrompter{next: ConflictAction(99)}
	got, err := ResolveConflict("/out/x.txt", Options{
		Strategy: "ask",
		IsTTY:    true,
		Prompter: fp,
	})
	if err == nil {
		t.Fatalf("expected error on invalid prompter action, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort on invalid prompter action", got)
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Fatalf("error %q missing 'invalid action' phrase", err)
	}
}

// P2-5: the zero-value Options{} should degrade to Abort with the TTY
// hint — protects against future accidental "empty struct means auto
// overwrite" regression.
func TestResolveConflict_ZeroValueOpts_DegradesToAbort(t *testing.T) {
	got, err := ResolveConflict("/out/x.txt", Options{})
	if err == nil {
		t.Fatalf("expected error on zero-value opts, got nil")
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort", got)
	}
	if !strings.Contains(err.Error(), "--overwrite") || !strings.Contains(err.Error(), "--skip") {
		t.Fatalf("error %q missing --overwrite/--skip hint", err)
	}
}

// P2-6: a non-EOF read error from stdin should be wrapped, surfaced,
// and mapped to Abort — we must not silently return "user chose Abort"
// and lose the underlying io failure.
func TestStdinPrompter_ReadError_Propagated(t *testing.T) {
	sentinel := errors.New("io broken")
	p := &StdinPrompter{In: &errReader{err: sentinel}, Out: io.Discard}
	got, err := p.Ask("/out/x.txt")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping %v", err, sentinel)
	}
	if got != ActionAbort {
		t.Fatalf("got %v, want ActionAbort on read failure", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type fakePrompter struct {
	next     ConflictAction
	err      error
	calls    int
	lastPath string
}

func (f *fakePrompter) Ask(existingPath string) (ConflictAction, error) {
	f.calls++
	f.lastPath = existingPath
	if f.err != nil {
		return ActionAbort, f.err
	}
	return f.next, nil
}

// errReader is an io.Reader that always fails with a caller-supplied
// error. Used to cover StdinPrompter's non-EOF read-error branch.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
