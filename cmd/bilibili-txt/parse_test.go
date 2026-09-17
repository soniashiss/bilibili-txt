package main

import (
	"bilibili-txt/internal/config"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
		{
			name:    "no_scheme_www",
			raw:     "www.bilibili.com/video/BV1Dstq6ZEFu",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  bv,
		},
		{
			name:    "no_scheme_mobile",
			raw:     "m.bilibili.com/video/BV1Dstq6ZEFu",
			wantURL: "https://m.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  bv,
		},
		{
			name:    "no_scheme_b23",
			raw:     "b23.tv/abcd12",
			wantURL: "https://b23.tv/abcd12",
			wantBV:  "",
		},
		{
			name:    "no_scheme_with_padding",
			raw:     "  www.bilibili.com/video/BV1Dstq6ZEFu  ",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu",
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

func TestNormalizeInput_RejectErrorHint(t *testing.T) {
	_, err := normalizeInput("not a url at all")
	if err == nil {
		t.Fatal("normalizeInput(not a url at all) expected error, got nil")
	}
	if !strings.Contains(err.Error(), "支持 bilibili.com/video/BVxxx") {
		t.Fatalf("error %q must contain supported-format hint", err.Error())
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

// Task 14: 无参数启动图形界面。

func newTestParser() (*cobra.Command, *bytes.Buffer) {
	parser := newRootCmd()
	var buf bytes.Buffer
	parser.SetOut(&buf)
	parser.SetErr(&buf)
	return parser, &buf
}

func TestNoArgs_FlagsRejected(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "force_asr", args: []string{"--force-asr"}},
		{name: "keep_intermediate", args: []string{"--keep-intermediate"}},
		{name: "overwrite", args: []string{"--overwrite"}},
		{name: "skip", args: []string{"--skip"}},
		{name: "output_dir", args: []string{"--output-dir", "out"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser, _ := newTestParser()
			parser.SetArgs(tc.args)
			err := parser.Execute()
			if err == nil {
				t.Fatalf("无参数 + %v 应报错，实际 nil", tc.args)
			}
			for _, want := range []string{"--force-asr", "--keep-intermediate", "--overwrite", "--skip", "--output-dir"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息 %q 必须列出 %q", err.Error(), want)
				}
			}
		})
	}
}

func TestNoArgs_StartsUI(t *testing.T) {
	parser, _ := newTestParser()
	parser.SetArgs(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origCtx := uiContext
	uiContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { uiContext = origCtx })

	called := 0
	var gotCfg *config.Config
	origStart := startWebUI
	startWebUI = func(ctx context.Context, cfg *config.Config) error {
		called++
		gotCfg = cfg
		cancel()
		return nil
	}
	t.Cleanup(func() { startWebUI = origStart })

	if err := parser.Execute(); err != nil {
		t.Fatalf("无参数启动 UI 不应报错，实际 %v", err)
	}
	if called != 1 {
		t.Fatalf("startWebUI 应被调用 1 次，实际 %d 次", called)
	}
	if gotCfg == nil {
		t.Fatal("startWebUI 收到的 cfg 为 nil")
	}
	if !gotCfg.Server.OpenBrowser {
		t.Fatalf("默认 server 段应开启 OpenBrowser：%+v", gotCfg.Server)
	}
}

func TestNoArgs_StartsUI_ConfigPassthrough(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := "server:\n  port: 8765\n  open_browser: false\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}

	parser, _ := newTestParser()
	parser.SetArgs([]string{"--config", cfgPath})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origCtx := uiContext
	uiContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { uiContext = origCtx })

	var gotCfg *config.Config
	origStart := startWebUI
	startWebUI = func(ctx context.Context, cfg *config.Config) error {
		gotCfg = cfg
		cancel()
		return nil
	}
	t.Cleanup(func() { startWebUI = origStart })

	if err := parser.Execute(); err != nil {
		t.Fatalf("加载配置启动 UI 不应报错，实际 %v", err)
	}
	if gotCfg == nil {
		t.Fatal("startWebUI 收到的 cfg 为 nil")
	}
	if gotCfg.Server.Port != 8765 || gotCfg.Server.OpenBrowser {
		t.Fatalf("server 段未透传：%+v", gotCfg.Server)
	}
}

func TestNoArgs_TwoArgsRejected(t *testing.T) {
	parser, _ := newTestParser()
	parser.SetArgs([]string{"BV1xx411c7mD", "BV1Dstq6ZEFu"})
	err := parser.Execute()
	if err == nil {
		t.Fatal("两个位置参数应报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "最多接受 1 个视频链接") {
		t.Fatalf("错误信息 %q 必须沿用现有文案", err.Error())
	}
}
