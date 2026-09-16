package biliurl

import "testing"

func TestNormalize_Strict(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantURL string
		wantBV  string
		wantErr bool
	}{
		{
			name:    "video_www_with_query_and_surrounding_whitespace",
			raw:     " https://www.bilibili.com/video/BV1Dstq6ZEFu/?x=1 ",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/?x=1",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_mobile",
			raw:     "https://m.bilibili.com/video/BV1Dstq6ZEFu",
			wantURL: "https://m.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_no_scheme_www",
			raw:     "www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_no_scheme_bare_host",
			raw:     "bilibili.com/video/BV1Dstq6ZEFu",
			wantURL: "https://bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "bare_bvid",
			raw:     "BV1Dstq6ZEFu",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "short_link",
			raw:     "https://b23.tv/abcd123",
			wantURL: "https://b23.tv/abcd123",
			wantBV:  "",
		},
		{
			name:    "short_link_no_scheme",
			raw:     "b23.tv/abcd123",
			wantURL: "https://b23.tv/abcd123",
			wantBV:  "",
		},
		{
			name:    "wrapped_single_quotes",
			raw:     "'https://www.bilibili.com/video/BV1Dstq6ZEFu/'",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "bvid_only_in_query_is_rejected",
			raw:     "https://www.bilibili.com/video/av123?x=BV1Dstq6ZEFu",
			wantURL: "",
			wantErr: true,
		},
		{
			name:    "short_link_with_trailing_garbage_is_rejected",
			raw:     "https://b23.tv/abc garbage",
			wantURL: "",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Normalize(%q) expected error, got %+v", tc.raw, got)
				}
				if got.URL != "" || got.BVID != "" {
					t.Errorf("Normalize(%q) error case must return zero Input, got %+v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q) unexpected err = %v", tc.raw, err)
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

func TestNormalize_Reject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "only_spaces", raw: "   "},
		{name: "quoted_whitespace", raw: `"  "`},
		{name: "other_host_with_bvid", raw: "https://youtube.com/watch?v=BV1Dstq6ZEFu"},
		{name: "bvid_with_trailing_text", raw: "BV1Dstq6ZEFu extra"},
		{name: "bilibili_without_bvid", raw: "https://www.bilibili.com/video/av12345"},
		{name: "plain_text", raw: "hello world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw)
			if err == nil {
				t.Fatalf("Normalize(%q) expected error, got %+v", tc.raw, got)
			}
		})
	}
}

func TestExtract_Loose(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantURL string
		wantBV  string
	}{
		{
			name:    "share_text_with_video_url_and_spm",
			raw:     "快来看看【某标题】 https://www.bilibili.com/video/BV1Dstq6ZEFu/?spm=x ，复制打开",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/?spm=x",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_url_no_scheme_embedded",
			raw:     "链接 bilibili.com/video/BV1Dstq6ZEFu 记得看",
			wantURL: "https://bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "bare_bvid_embedded",
			raw:     "帮我转下 BV1Dstq6ZEFu 这个",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "short_link_no_scheme_embedded",
			raw:     "短链 b23.tv/abcd123 速来",
			wantURL: "https://b23.tv/abcd123",
			wantBV:  "",
		},
		{
			name:    "pure_url_passthrough",
			raw:     "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "wrapped_in_chinese_parens",
			raw:     "（https://www.bilibili.com/video/BV1Dstq6ZEFu/）",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_url_touching_chinese_on_left",
			raw:     "请看bilibili.com/video/BV1Dstq6ZEFu 谢谢",
			wantURL: "https://bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "video_url_no_scheme_at_start",
			raw:     "www.bilibili.com/video/BV1Dstq6ZEFu/ 后面",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu/",
			wantBV:  "BV1Dstq6ZEFu",
		},
		{
			name:    "short_link_no_scheme_at_start",
			raw:     "b23.tv/abcd123 速来",
			wantURL: "https://b23.tv/abcd123",
			wantBV:  "",
		},
		{
			name:    "video_link_wins_over_short_link",
			raw:     "https://b23.tv/abcd123 还有 https://www.bilibili.com/video/BV1Dstq6ZEFu",
			wantURL: "https://www.bilibili.com/video/BV1Dstq6ZEFu",
			wantBV:  "BV1Dstq6ZEFu",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Extract(tc.raw)
			if err != nil {
				t.Fatalf("Extract(%q) unexpected err = %v", tc.raw, err)
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

func TestExtract_Reject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "only_spaces", raw: "   "},
		{name: "text_without_link", raw: "没有链接的文字"},
		{name: "other_host", raw: "https://youtube.com/x"},
		{name: "sibling_domain_evilbilibili", raw: "evilbilibili.com/video/BV1Dstq6ZEFu"},
		{name: "sibling_domain_xwww", raw: "xwww.bilibili.com/video/BV1Dstq6ZEFu"},
		{name: "sibling_domain_ab23", raw: "ab23.tv/abcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Extract(tc.raw)
			if err == nil {
				t.Fatalf("Extract(%q) expected error, got %+v", tc.raw, got)
			}
		})
	}
}
