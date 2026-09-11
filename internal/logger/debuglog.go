package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bilibili-txt/internal/pathx"
)

func resolveDebugLogPath(explicit, bvid string, now time.Time, warnSlog *slog.Logger) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		if strings.HasPrefix(p, "~") {
			return "", fmt.Errorf("logger: ~ expansion must be done by caller (got %q)", p)
		}
		abs, err := pathx.AbsFromCwd(p, "logger.debug_file")
		if err != nil {
			return "", err
		}
		if err := ensureWritableDir(filepath.Dir(abs)); err != nil {
			return "", err
		}
		return abs, nil
	}

	name := debugLogName(bvid, now)
	var primaryErr error
	primary, err := defaultLogsDir()
	if err != nil {
		primaryErr = err
	} else if err := ensureWritableDir(primary); err != nil {
		primaryErr = err
	} else {
		return filepath.Join(primary, name), nil
	}
	fallback := filepath.Join(os.TempDir(), "bilibili-txt-logs")
	if err := ensureWritableDir(fallback); err != nil {
		return "", fmt.Errorf("cannot create fallback log dir %s: %w", fallback, err)
	}
	if warnSlog != nil {
		warnSlog.Warn("cwd/logs not writable, falling back to temp dir",
			"dir", fallback,
			"primary_err", errString(primaryErr),
		)
	}
	return filepath.Join(fallback, name), nil
}

func debugLogName(bvid string, now time.Time) string {
	id := strings.TrimSpace(bvid)
	if id == "" {
		id = "nobvid"
	}
	return fmt.Sprintf("%s-%s.log", id, now.Format("20060102-150405"))
}

func defaultLogsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "logs"), nil
}

func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
