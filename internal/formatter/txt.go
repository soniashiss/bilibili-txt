package formatter

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"bilibili-txt/internal/subtitle"
)

// writeTXT emits one non-empty [subtitle.Cue.Text] per output line, in
// caller-supplied order. The rendered file is a stream of paragraph-
// like lines where each line is exactly one caption row.
//
// # Design choices
//
//   - Cue boundary IS the line break. Bilibili CC / AI 字幕 (ai-zh)
//     frequently carry no sentence-terminating punctuation at all
//     (whisper punctuation restoration is not applied on the B-side),
//     so the previous "concatenate then split on 。！？!?." heuristic
//     collapsed 800+ cues into a single unreadable line. Trusting the
//     upstream cue segmentation is the only strategy that degrades
//     gracefully across the "no punctuation / sparse punctuation /
//     dense punctuation" spectrum without a language-specific
//     tokeniser.
//   - Empty [subtitle.Cue.Text] entries are skipped (they carry no
//     signal for TXT); the parser preserves them so other formats
//     can render silence markers.
//   - Each emitted line is TrimSpace'd so trailing whitespace baked
//     into CC exports doesn't leak into the transcript.
//   - A trailing newline is always appended so the file ends with a
//     LF — the POSIX convention every unix tool expects.
func writeTXT(w io.Writer, cues []subtitle.Cue) error {
	bw := bufio.NewWriter(w)

	for _, c := range cues {
		line := strings.TrimSpace(c.Text)
		if line == "" {
			continue
		}
		if _, err := bw.WriteString(line); err != nil {
			return fmt.Errorf("formatter: txt: write line: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("formatter: txt: write newline: %w", err)
		}
	}

	if err := bw.Flush(); err != nil {
		return fmt.Errorf("formatter: txt: flush: %w", err)
	}
	return nil
}
