package history

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file holds white-box tests for EnforceLimit's per-file removal
// bookkeeping. The removeFile seam lets us force outcomes that the real
// filesystem cannot produce on demand: a file vanishing between scan and
// remove (fs.ErrNotExist) and a per-file unlink failure while sibling
// files remain deletable (the chmod-based external test cannot isolate a
// single file, since unlink permission lives on the parent directory).

func writeTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func setTestMTime(t *testing.T, p string, m time.Time) {
	t.Helper()
	if err := os.Chtimes(p, m, m); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
}

// stubRemove replaces removeFile for the duration of the test: paths in
// notExist report fs.ErrNotExist (vanished after scan), paths in fail
// report a permission error, and every other path is really unlinked.
func stubRemove(t *testing.T, notExist, fail map[string]bool) {
	t.Helper()
	orig := removeFile
	removeFile = func(name string) error {
		switch {
		case notExist[name]:
			return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
		case fail[name]:
			return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
		default:
			return os.Remove(name)
		}
	}
	t.Cleanup(func() { removeFile = orig })
}

func TestEnforceLimit_VanishedAfterScan_NotCountedAsRemoved(t *testing.T) {
	dir := t.TempDir()
	oldTxt := writeTestFile(t, dir, "old__BV1oldxx.txt")
	oldMd := writeTestFile(t, dir, "old__BV1oldxx.md")
	mid := writeTestFile(t, dir, "mid__BV1midxx.txt")
	newest := writeTestFile(t, dir, "new__BV1newxx.txt")
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	setTestMTime(t, oldTxt, base)
	setTestMTime(t, oldMd, base)
	setTestMTime(t, mid, base.AddDate(1, 0, 0))
	setTestMTime(t, newest, base.AddDate(2, 0, 0))

	// The old group's .md file is present at scan time but is gone by the
	// time EnforceLimit unlinks it: that path must not count as removed.
	stubRemove(t, map[string]bool{oldMd: true}, nil)

	removed, err := EnforceLimit(context.Background(), dir, 1)
	if err != nil {
		t.Fatalf("EnforceLimit: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d groups, want 2 (old, mid): %+v", len(removed), removed)
	}
	byBVID := map[string][]string{}
	for _, rg := range removed {
		byBVID[rg.BVID] = rg.Paths
	}
	ps := byBVID["BV1oldxx"]
	if len(ps) != 1 || filepath.Clean(ps[0]) != filepath.Clean(oldTxt) {
		t.Fatalf("old group paths = %v, want only the actually-unlinked txt [%s]", ps, oldTxt)
	}
	if len(byBVID["BV1midxx"]) != 1 {
		t.Fatalf("mid group = %v, want its single path reported", byBVID["BV1midxx"])
	}
	if _, err := os.Stat(oldTxt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old txt should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest kept file should remain: %v", err)
	}
}

func TestEnforceLimit_GroupFullyVanished_NotReported(t *testing.T) {
	dir := t.TempDir()
	old := writeTestFile(t, dir, "old__BV1oldxx.txt")
	mid := writeTestFile(t, dir, "mid__BV1midxx.txt")
	newest := writeTestFile(t, dir, "new__BV1newxx.txt")
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	setTestMTime(t, old, base)
	setTestMTime(t, mid, base.AddDate(1, 0, 0))
	setTestMTime(t, newest, base.AddDate(2, 0, 0))

	// Whole surplus group vanished between scan and remove: it must not
	// produce an (empty) RemovedGroup at all.
	stubRemove(t, map[string]bool{old: true}, nil)

	removed, err := EnforceLimit(context.Background(), dir, 1)
	if err != nil {
		t.Fatalf("EnforceLimit: %v", err)
	}
	if len(removed) != 1 || removed[0].BVID != "BV1midxx" {
		t.Fatalf("removed = %+v, want only [BV1midxx]", removed)
	}
}

func TestEnforceLimit_PerFileFailure_AggregatesAndContinues(t *testing.T) {
	dir := t.TempDir()
	oldTxt := writeTestFile(t, dir, "old__BV1oldxx.txt")
	oldMd := writeTestFile(t, dir, "old__BV1oldxx.md")
	mid := writeTestFile(t, dir, "mid__BV1midxx.txt")
	newest := writeTestFile(t, dir, "new__BV1newxx.txt")
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	setTestMTime(t, oldTxt, base)
	setTestMTime(t, oldMd, base)
	setTestMTime(t, mid, base.AddDate(1, 0, 0))
	setTestMTime(t, newest, base.AddDate(2, 0, 0))

	stubRemove(t, nil, map[string]bool{oldTxt: true})

	var removed []RemovedGroup
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("EnforceLimit panicked on delete failure: %v", r)
			}
		}()
		removed, err = EnforceLimit(context.Background(), dir, 1)
	}()
	if err == nil {
		t.Fatal("EnforceLimit expected aggregated delete error, got nil")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want it to wrap fs.ErrPermission", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, delete failure must not masquerade as ctx error", err)
	}

	// Sweep continued past the failure: sibling file and sibling group
	// were still deleted; newest (keep=1) survives.
	if _, statErr := os.Stat(oldMd); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("old md should still have been removed, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(mid); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("mid group should still have been removed, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(newest); statErr != nil {
		t.Fatalf("newest kept file should remain: %v", statErr)
	}
	if _, statErr := os.Stat(oldTxt); statErr != nil {
		t.Fatalf("failed-to-unlink file must be left in place, stat err=%v", statErr)
	}

	byBVID := map[string][]string{}
	for _, rg := range removed {
		byBVID[rg.BVID] = rg.Paths
	}
	ps := byBVID["BV1oldxx"]
	if len(ps) != 1 || filepath.Clean(ps[0]) != filepath.Clean(oldMd) {
		t.Fatalf("old group removed paths = %v, want only successfully removed md [%s]", ps, oldMd)
	}
	if len(byBVID["BV1midxx"]) != 1 {
		t.Fatalf("mid group = %v, want its single path reported", byBVID["BV1midxx"])
	}
}
