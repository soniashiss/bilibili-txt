// Package biliurl 对 B 站视频链接做归一化与提取。
//
// Normalize 是严格入口（CLI）：整串必须就是一条可接受的链接或裸 BV 号；
// Extract 是宽松入口（界面/分享文案）：允许夹带文字，只抽出其中的链接片段。
// 两者共用同一套正则与预处理。
package biliurl

import (
	"fmt"
	"regexp"
	"strings"
)

// Input 是归一化后的输入。
type Input struct {
	URL  string // 规范化后的完整 URL（无协议头的补 https://）
	BVID string // 视频页 URL 解析出的 BV 号；短链为空串
}

// 链接尾部允许吞掉的字符：直到空白、引号、尖括号、ASCII/中文标点为止。
const tail = `[^\s'"<>（）()，。、；！？]*`

// leftBoundary is the scheme-less patterns' left anchor: either the
// start of the text or one character outside the host/path token class
// [A-Za-z0-9._-]. A custom class is used instead of \b because RE2's \w
// is ASCII-only: \b would treat an adjacent CJK character as a word
// boundary the other way around, while the explicit class keeps CJK
// text a legal boundary (e.g. "请看bilibili.com/…" still matches) yet
// rejects sibling hosts such as "evilbilibili.com" or "xwww.bilibili…".
// The boundary is a zero-or-one-byte non-capturing prefix; callers slice
// the URL out of the "url" submatch so the preceding byte is never
// included in the result.
const leftBoundary = `(?:^|[^A-Za-z0-9._-])`

var (
	videoWithSchemeRe = regexp.MustCompile(`https?://(?:www\.|m\.)?bilibili\.com/video/(BV[0-9A-Za-z]{8,})` + tail)
	videoNoSchemeRe   = regexp.MustCompile(leftBoundary + `(?P<url>(?:www\.|m\.)?bilibili\.com/video/(?P<bv>BV[0-9A-Za-z]{8,})` + tail + `)`)
	shortWithSchemeRe = regexp.MustCompile(`https?://b23\.tv/[0-9A-Za-z]+` + tail)
	shortNoSchemeRe   = regexp.MustCompile(leftBoundary + `(?P<url>b23\.tv/[0-9A-Za-z]+` + tail + `)`)
	// bvidRe shares leftBoundary and additionally rejects a preceding
	// '/': a bare BV id sitting in some other host's URL path
	// ("evilbilibili.com/video/BV…") must not be extracted as a
	// standalone BV. The right edge stays an ASCII \b.
	bvidRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9._/-])(BV[0-9A-Za-z]{8,})\b`)
)

// trimAndUnquote 去掉首尾空白，并循环剥掉成对的单引号/双引号/反引号。
func trimAndUnquote(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		n := len(s)
		if n < 2 {
			break
		}
		first, last := s[0], s[n-1]
		switch first {
		case '\'', '"', '`':
			if last != first {
				return s
			}
		default:
			return s
		}
		s = strings.TrimSpace(s[1 : n-1])
	}
	return s
}

func fail(raw string) (Input, error) {
	return Input{}, fmt.Errorf("biliurl: 无法识别的视频链接: %q", raw)
}

// Normalize 严格归一化：预处理后的整串必须完整匹配一条视频页链接、短链或裸 BV 号。
func Normalize(raw string) (Input, error) {
	s := trimAndUnquote(raw)
	if s == "" {
		return fail(raw)
	}

	// Group positions: in the scheme-less patterns group 1 is "url"
	// (group 2 is "bv" for the video pattern); for bvidRe group 1 is the
	// bare BV id and group 0 may carry one preceding boundary byte.
	if m := videoWithSchemeRe.FindStringSubmatch(s); m != nil && m[0] == s {
		return Input{URL: m[0], BVID: m[1]}, nil
	}
	if m := videoNoSchemeRe.FindStringSubmatch(s); m != nil && m[1] == s {
		return Input{URL: "https://" + m[1], BVID: m[2]}, nil
	}
	if m := shortWithSchemeRe.FindStringSubmatch(s); m != nil && m[0] == s {
		return Input{URL: m[0]}, nil
	}
	if m := shortNoSchemeRe.FindStringSubmatch(s); m != nil && m[1] == s {
		return Input{URL: "https://" + m[1]}, nil
	}
	if m := bvidRe.FindStringSubmatch(s); m != nil && m[0] == s {
		return Input{URL: "https://www.bilibili.com/video/" + m[1], BVID: m[1]}, nil
	}

	return fail(raw)
}

// match 描述一次宽松查找的结果。
type match struct {
	url  string
	bvid string
}

// findFirst 按给定正则顺序在 text 中查找第一个命中。返回的 url 取自
// "url" 命名子匹配（无协议头正则的整体匹配 loc[0:2] 会多包含一个左边界
// 字符，不能直接当链接），bvid 取自 "bv" 子匹配（带协议头视频正则没有
// 命名组，回退到编号组 1）。
func findFirst(text string, patterns []*regexp.Regexp) (match, bool) {
	for _, re := range patterns {
		loc := re.FindStringSubmatchIndex(text)
		if loc == nil {
			continue
		}
		urlStart, urlEnd := loc[0], loc[1]
		if i := re.SubexpIndex("url"); i >= 0 && loc[2*i] >= 0 {
			urlStart, urlEnd = loc[2*i], loc[2*i+1]
		}
		bvid := ""
		if i := re.SubexpIndex("bv"); i >= 0 {
			if loc[2*i] >= 0 {
				bvid = text[loc[2*i]:loc[2*i+1]]
			}
		} else if len(loc) >= 4 && loc[2] >= 0 {
			bvid = text[loc[2]:loc[3]]
		}
		return match{url: text[urlStart:urlEnd], bvid: bvid}, true
	}
	return match{}, false
}

// Extract 宽松提取：从夹带文字的文本中按优先级抽出链接片段；
// 视频页（带协议→无协议）→ 短链（带协议→无协议）→ 裸 BV 号。
func Extract(raw string) (Input, error) {
	s := trimAndUnquote(raw)
	if s == "" {
		return fail(raw)
	}

	if m, ok := findFirst(s, []*regexp.Regexp{videoWithSchemeRe, videoNoSchemeRe}); ok {
		prefix := ""
		if !strings.HasPrefix(m.url, "http://") && !strings.HasPrefix(m.url, "https://") {
			prefix = "https://"
		}
		return Input{URL: prefix + m.url, BVID: m.bvid}, nil
	}
	if m, ok := findFirst(s, []*regexp.Regexp{shortWithSchemeRe, shortNoSchemeRe}); ok {
		prefix := ""
		if !strings.HasPrefix(m.url, "http://") && !strings.HasPrefix(m.url, "https://") {
			prefix = "https://"
		}
		return Input{URL: prefix + m.url}, nil
	}
	// Group 0 may carry one preceding boundary byte; the BV id is group 1.
	if m := bvidRe.FindStringSubmatch(s); m != nil {
		return Input{URL: "https://www.bilibili.com/video/" + m[1], BVID: m[1]}, nil
	}

	return fail(raw)
}
