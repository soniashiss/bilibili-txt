package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeInput_ArgvCompat(t *testing.T) {
	const bv = "BV1Dstq6ZEFu"
	const plainURL = "https://www.bilibili.com/video/BV1Dstq6ZEFu/?trackid=abc&vd_source=xyz"

	cases := []struct {
		name    string
		raw     string
		wantURL string
		wantBV  string
	}{
		{
			name:    "chrome_plain_url",
			raw:     plainURL,
			wantURL: plainURL,
			wantBV:  bv,
		},
		{
			name:    "backslash_escaped_query",
			raw:     `https://www.bilibili.com/video/BV1Dstq6ZEFu/\?trackid\=abc\&vd_source\=xyz`,
			wantURL: plainURL,
			wantBV:  bv,
		},
		{
			name:    "wrapped_backticks_and_escapes",
			raw:     "`https://www.bilibili.com/video/BV1Dstq6ZEFu/\\?trackid\\=abc\\&vd_source\\=xyz`",
			wantURL: plainURL,
			wantBV:  bv,
		},
		{
			name:    "wrapped_double_quotes",
			raw:     `"https://www.bilibili.com/video/BV1Dstq6ZEFu/?trackid=abc&vd_source=xyz"`,
			wantURL: plainURL,
			wantBV:  bv,
		},
		{
			name:    "wrapped_single_quotes_and_escapes",
			raw:     `'https://www.bilibili.com/video/BV1Dstq6ZEFu/\?trackid\=abc\&vd_source\=xyz'`,
			wantURL: plainURL,
			wantBV:  bv,
		},
		{
			name:    "nested_wrapping",
			raw:     "\"`https://www.bilibili.com/video/BV1Dstq6ZEFu/\\?trackid\\=abc`\"",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/?trackid=abc",
			wantBV:  bv,
		},
		{
			name:    "surrounding_whitespace",
			raw:     "   https://www.bilibili.com/video/BV1Dstq6ZEFu/   ",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  bv,
		},
		{
			name:    "plain_bvid",
			raw:     bv,
			wantURL: "https://www.bilibili.com/" + "video/" + bv,
			wantBV:  bv,
		},
		{
			name:    "b23_short_link",
			raw:     "https://b23.tv/abcd123",
			wantURL: "https://b23.tv/abcd123",
			wantBV:  "",
		},
		{
			name:    "escaped_fragment_and_percent_encoding",
			raw:     `'https://www.bilibili.com/video/BV1Dstq6ZEFu/\?title=%E4%B8%AD\#reply'`,
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/?title=%E4%B8%AD#reply",
			wantBV:  bv,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeInput(tc.raw)
			if err != nil {
				t.Fatalf("normalizeInput(%q) unexpected err = %v", tc.raw, err)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL: got %q, want %q", got.URL, tc.wantURL)
			}
			if got.BVID != tc.wantBV {
				t.Errorf("BVID: got %q, want %q", got.BVID, tc.wantBV)
			}
		})
	}
}

func TestNormalizeInput_Reject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "only_whitespace_and_quotes", raw: `"   "`},
		{name: "not_bilibili_host", raw: "https://youtube.com/watch?v=BV1Dstq6ZEFu"},
		{name: "bilibili_without_bvid", raw: "https://www.bilibili.com/video/av12345"},
		{name: "bvid_with_trailing_garbage", raw: "BV1Dstq6ZEFu extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeInput(tc.raw); err == nil {
				t.Fatalf("normalizeInput(%q) expected error, got nil", tc.raw)
			}
		})
	}
}

func TestSanitizeArgvURL_LeavesPercentEncodingAlone(t *testing.T) {
	in := `https://www.bilibili.com/video/BV1Dstq6ZEFu/?path=%2Fsome%2Fdir&q=a%3Db`
	got := sanitizeArgvURL(in)
	if got != in {
		t.Errorf("sanitizeArgvURL must not touch percent-encoded sequences.\n got: %q\nwant: %q", got, in)
	}
}

func TestHelp_MentionsEveryPlan45Flag(t *testing.T) {
	wantFlags := []string{
		"--force-asr",
		"--keep-intermediate",
		"--no-interactive",
		"--output-dir",
		"--overwrite",
		"--skip",
		"--version",
	}
	help := renderHelp(t)
	for _, f := range wantFlags {
		if !strings.Contains(help, f) {
			t.Errorf("--help output missing flag %q\n%s", f, help)
		}
	}
}

// TestHelp_HidesConfigDrivenFlags pins the §4.6 CLI slim-down: the flags
// we relocated into config.yaml (format / model / log-* / skip-preflight)
// must NOT appear in `--help`. `--config` stays in the flag set for
// integration tests but is marked hidden. If any of these strings ever
// leak back into help, the reviewer will see the failure here instead
// of in a downstream user-facing regression.
func TestHelp_HidesConfigDrivenFlags(t *testing.T) {
	help := renderHelp(t)
	hidden := []string{
		"--format",
		"--model",
		"--log-format",
		"--log-file",
		"--debug-log-file",
		"--skip-preflight",
		"--config",
	}
	for _, f := range hidden {
		if strings.Contains(help, f) {
			t.Errorf("--help must not expose relocated/hidden flag %q\n%s", f, help)
		}
	}
}

func TestHelp_MentionsExplicitlyForbiddenBinaryFlags_Not(t *testing.T) {
	help := renderHelp(t)
	forbidden := []string{"--ytdlp", "--ffmpeg", "--whisper"}
	for _, f := range forbidden {
		if strings.Contains(help, f) {
			t.Errorf("--help must not expose %q (paths belong in config.yaml)\n%s", f, help)
		}
	}
}

func renderHelp(t *testing.T) string {
	t.Helper()
	parser := newRootCmd()
	var buf bytes.Buffer
	parser.SetOut(&buf)
	parser.SetErr(&buf)
	parser.SetArgs([]string{"--help"})
	if err := parser.Execute(); err != nil {
		t.Fatalf("parser.Execute(--help) unexpected err = %v", err)
	}
	return buf.String()
}
