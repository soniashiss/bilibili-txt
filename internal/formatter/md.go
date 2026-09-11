package formatter

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"bilibili-txt/internal/subtitle"
)

// writeMD renders cues as GitHub-flavoured markdown. Each cue is a
// separate paragraph led by an `[mm:ss]` timestamp anchor so the file
// scrolls like a podcast transcript with jump-worthy points:
//
//	[00:00] 大家好，欢迎收看本期视频。
//
//	[00:02] 今天我们来聊一聊字幕自动化。
//
// The trailing blank line between cues is what turns paragraphs into
// distinct blocks under CommonMark — without it every cue would fuse
// into a single wrap-happy paragraph and readability collapses on
// narrow viewports.
//
// # Hour handling
//
// Videos beyond 60 minutes render as `[HH:MM:SS]` — the extra segment
// only shows up when necessary so short clips stay compact. We could
// have always emitted `[HH:MM:SS]` for a uniform grid, but a two-hour
// interview and a two-minute short living in the same output looked
// noisy in practice and the extra field trained eyes to skim over
// `[00:00:xx]` prefixes.
//
// # Empty text cues
//
// The parser preserves zero-body cues; here they render as
// `[mm:ss] _(silent)_` so the transcript still hints at the gap
// without an empty markdown block. Callers that want strict "drop
// silent cues" behaviour should filter before calling [Convert].
func writeMD(w io.Writer, cues []subtitle.Cue) error {
	bw := bufio.NewWriter(w)
	for i, c := range cues {
		stamp := formatTimestampMD(c.Start)
		text := c.Text
		if text == "" {
			text = "_(silent)_"
		}
		if _, err := bw.WriteString("[" + stamp + "] " + text); err != nil {
			return fmt.Errorf("formatter: md: write cue %d: %w", c.Index, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("formatter: md: write newline after cue %d: %w", c.Index, err)
		}
		// Paragraph separator between cues; skip the trailing one so
		// the file ends with a single LF, matching every other
		// text-format the pipeline emits.
		if i != len(cues)-1 {
			if err := bw.WriteByte('\n'); err != nil {
				return fmt.Errorf("formatter: md: write separator after cue %d: %w", c.Index, err)
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("formatter: md: flush: %w", err)
	}
	return nil
}

// formatTimestampMD renders a duration as mm:ss (< 1h) or HH:MM:SS
// (>= 1h). Millisecond precision is dropped: the markdown view is
// human-oriented and rounding to the second matches how people
// intuitively scrub video.
func formatTimestampMD(d time.Duration) string {
	if d < 0 {
		// Negative durations shouldn't reach here (parser rejects
		// them), but if they do render as "00:00" rather than
		// panicking or emitting a garbage negative prefix.
		d = 0
	}
	total := int(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
