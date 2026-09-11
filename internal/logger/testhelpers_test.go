package logger

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
)

func newTestSlog(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func decodeLines(t *testing.T, lines [][]byte) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(lines))
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Fatalf("line %d: %v: %s", i, err, ln)
		}
		out = append(out, m)
	}
	return out
}

func chdirTemp(t *testing.T, dir string) string {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd after chdir: %v", err)
	}
	return cwd
}
