package formatter

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"bilibili-txt/internal/subtitle"
)

// writeSRT emits the cues in canonical SRT form, preserving the
// source [subtitle.Cue.Index] as required by the "原样透传" contract
// in docs/contracts.md §三.2. The parser already normalised the
// stream (BOM stripped, CRLF collapsed, malformed rows rejected), so
// this writer's sole job is to re-serialise deterministically:
//
//   - Line endings are CRLF (\r\n). The contract explicitly requires
//     it, and some legacy SRT parsers reject LF-only files.
//   - Timestamps use the SRT-canonical comma separator
//     ("HH:MM:SS,mmm"), never the VTT-style dot the parser accepts on
//     input. Precision is millisecond.
//   - Cue index comes straight from the source. When the parser fell
//     back to physical numbering (source omitted the index), that
//     synthesised value is already in [subtitle.Cue.Index]; we do not
//     re-number here to keep the mapping "one SRT input, one SRT
//     output with the same indices" deterministic — pipeline layers
//     that need dense re-numbering should compose a filter, not
//     hard-code it into the writer.
//   - Blocks are separated by a blank CRLF line; every block
//     (including the last) ends with a blank separator so third-party
//     players never trip over a "missing trailing blank" edge case.
func writeSRT(w io.Writer, cues []subtitle.Cue) error {
	bw := bufio.NewWriter(w)
	for _, c := range cues {
		block := fmt.Sprintf("%d\r\n%s --> %s\r\n%s\r\n\r\n",
			c.Index,
			formatTimestampSRT(c.Start),
			formatTimestampSRT(c.End),
			c.Text,
		)
		if _, err := bw.WriteString(block); err != nil {
			return fmt.Errorf("formatter: srt: write cue %d: %w", c.Index, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("formatter: srt: flush: %w", err)
	}
	return nil
}

// formatTimestampSRT renders a duration in canonical SRT form:
// "HH:MM:SS,mmm" with zero-padded fields and comma-separated
// milliseconds. Values above 99h clamp to "99:MM:SS,mmm" so the
// HH:MM:SS,mmm width stays constant — a byte-offset assumption some
// strict tools bake in. Negative durations clamp to zero; the parser
// never emits them but the guard is cheap defence-in-depth.
func formatTimestampSRT(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := d.Milliseconds()
	ms := total % 1000
	total /= 1000
	s := total % 60
	total /= 60
	m := total % 60
	total /= 60
	h := total
	if h > 99 {
		h = 99
	}
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}
