package history_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bilibili-txt/internal/history"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func setMTime(t *testing.T, p string, m time.Time) {
	t.Helper()
	if err := os.Chtimes(p, m, m); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(" %s should have been removed, stat err=%v", p, err)
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s should still exist: %v", p, err)
	}
}

// ---------------------------------------------------------------------------
// List: empty / missing directory
// ---------------------------------------------------------------------------

func TestList_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List empty dir: unexpected err %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty dir = %d entries, want 0", len(got))
	}
}

func TestList_NonExistentDir_ReturnsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List missing dir: unexpected err %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List missing dir = %d entries, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// List: single file / field correctness
// ---------------------------------------------------------------------------

func TestList_SingleFile_FieldsCorrect(t *testing.T) {
	dir := t.TempDir()
	m := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	p := writeFile(t, dir, "t__BV1xx.txt", "hello")
	setMTime(t, p, m)

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	e := got[0]
	if e.BVID != "BV1xx" {
		t.Fatalf("BVID = %q, want BV1xx", e.BVID)
	}
	if e.Name != "t__BV1xx" {
		t.Fatalf("Name = %q, want t__BV1xx (extension trimmed)", e.Name)
	}
	if !e.MTime.Equal(m) {
		t.Fatalf("MTime = %v, want %v", e.MTime, m)
	}
	if !filepath.IsAbs(e.Path) {
		t.Fatalf("Path = %q, want absolute", e.Path)
	}
	if filepath.Base(e.Path) != "t__BV1xx.txt" {
		t.Fatalf("Path base = %q, want t__BV1xx.txt", filepath.Base(e.Path))
	}
	if filepath.Clean(filepath.Dir(e.Path)) != filepath.Clean(dir) {
		t.Fatalf("Path dir = %q, want %q", filepath.Dir(e.Path), dir)
	}
}

// ---------------------------------------------------------------------------
// List: format priority txt > md > srt
// ---------------------------------------------------------------------------

func TestList_SamePage_MultipleFormats_PrefersTxt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "t__BV1xx.md", "md")
	writeFile(t, dir, "t__BV1xx.srt", "srt")
	pTxt := writeFile(t, dir, "t__BV1xx.txt", "txt")
	setMTime(t, filepath.Join(dir, "t__BV1xx.srt"),
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Path != pTxt {
		t.Fatalf("representative = %q, want txt %q", got[0].Path, pTxt)
	}
	if got[0].Name != "t__BV1xx" {
		t.Fatalf("Name = %q, want t__BV1xx", got[0].Name)
	}
	// Group mtime is the max across all files in the group, even when
	// the representative itself is older.
	if got[0].MTime.Unix() != time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("MTime = %v, want group max (2030)", got[0].MTime)
	}
}

// ---------------------------------------------------------------------------
// List: page ordering
// ---------------------------------------------------------------------------

func TestList_NoSuffixBeatsP2(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "t__BV1xx.txt", "p1")
	writeFile(t, dir, "t__BV1xx.p2.txt", "p2")
	writeFile(t, dir, "t__BV1xx.p3.md", "p3")

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Path != p1 {
		t.Fatalf("representative = %q, want no-suffix P1 %q", got[0].Path, p1)
	}
	if got[0].Name != "t__BV1xx" {
		t.Fatalf("Name = %q, want t__BV1xx", got[0].Name)
	}
}

func TestList_OnlyP2P3_PicksP2EvenWhenP3IsTxt(t *testing.T) {
	dir := t.TempDir()
	p2 := writeFile(t, dir, "t__BV1xx.p2.md", "p2md")
	writeFile(t, dir, "t__BV1xx.p2.srt", "p2srt")
	writeFile(t, dir, "t__BV1xx.p3.txt", "p3txt")

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Path != p2 {
		t.Fatalf("representative = %q, want p2 md %q (page beats format)", got[0].Path, p2)
	}
	if got[0].Name != "t__BV1xx.p2" {
		t.Fatalf("Name = %q, want t__BV1xx.p2 (.pN preserved, ext trimmed)", got[0].Name)
	}
}

func TestList_NoSuffixAndExplicitP1_BothTxt_PicksNoSuffix(t *testing.T) {
	dir := t.TempDir()
	noSuffix := writeFile(t, dir, "t__BV1xx.txt", "p1-nosuffix")
	writeFile(t, dir, "t__BV1xx.p1.txt", "p1-explicit")

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Path != noSuffix {
		t.Fatalf("representative = %q, want no-suffix %q", got[0].Path, noSuffix)
	}
}

// ---------------------------------------------------------------------------
// List: group ordering (max mtime desc, BVID asc tie-break)
// ---------------------------------------------------------------------------

func TestList_OrderedByGroupMaxMTimeDesc(t *testing.T) {
	dir := t.TempDir()
	old := writeFile(t, dir, "old__BV1oldxx.txt", "old")
	new := writeFile(t, dir, "new__BV1newxx.txt", "new")
	setMTime(t, old, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	setMTime(t, new, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	if got[0].BVID != "BV1newxx" || got[1].BVID != "BV1oldxx" {
		t.Fatalf("order = [%s %s], want [BV1newxx BV1oldxx]", got[0].BVID, got[1].BVID)
	}
}

func TestList_EqualMTime_TieBreakByBVIDAsc(t *testing.T) {
	dir := t.TempDir()
	m := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	a := writeFile(t, dir, "b__BV1aaa2xx.txt", "a")
	b := writeFile(t, dir, "a__BV1aaa1xx.txt", "b")
	setMTime(t, a, m)
	setMTime(t, b, m)

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	if got[0].BVID != "BV1aaa1xx" || got[1].BVID != "BV1aaa2xx" {
		t.Fatalf("tie order = [%s %s], want [BV1aaa1xx BV1aaa2xx]", got[0].BVID, got[1].BVID)
	}
}

// ---------------------------------------------------------------------------
// List: non-matching names / directories are ignored, never deleted
// ---------------------------------------------------------------------------

func TestList_IgnoresNonMatchingNamesAndDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "notes.txt", "n")
	writeFile(t, dir, "x.md", "x")
	writeFile(t, dir, "t__BV1xx.log", "log")
	writeFile(t, dir, "t__BV1xx", "noext")
	writeFile(t, dir, "BVxxonly.txt", "no-separator")
	if err := os.Mkdir(filepath.Join(dir, "t__BV1dirxx.txt"), 0o755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d groups, want 0 (all names non-matching): %+v", len(got), got)
	}
}

// ---------------------------------------------------------------------------
// EnforceLimit
// ---------------------------------------------------------------------------

func TestEnforceLimit_SevenGroups_KeepsNewestFive(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	type g struct {
		bvid  string
		paths []string
	}
	var groups []g
	for i := 0; i < 7; i++ {
		bvid := "BV1g" + string(rune('0'+i)) + "xxx"
		var paths []string
		paths = append(paths, writeFile(t, dir, "g"+string(rune('0'+i))+"__"+bvid+".txt", "txt"))
		if i >= 5 {
			// Oldest two groups carry extra formats / pages so we can
			// prove the whole group is wiped.
			paths = append(paths, writeFile(t, dir, "g"+string(rune('0'+i))+"__"+bvid+".md", "md"))
			paths = append(paths, writeFile(t, dir, "g"+string(rune('0'+i))+"__"+bvid+".srt", "srt"))
			paths = append(paths, writeFile(t, dir, "g"+string(rune('0'+i))+"__"+bvid+".p2.txt", "p2"))
		}
		for _, p := range paths {
			setMTime(t, p, base.AddDate(0, 0, i))
		}
		groups = append(groups, g{bvid: bvid, paths: paths})
	}
	notes := writeFile(t, dir, "notes.txt", "notes")
	xmd := writeFile(t, dir, "x.md", "x")
	logf := writeFile(t, dir, "g0__"+groups[0].bvid+".log", "log")
	subdir := filepath.Join(dir, "somedir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	removed, err := history.EnforceLimit(context.Background(), dir, 5)
	if err != nil {
		t.Fatalf("EnforceLimit: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d groups, want 2", len(removed))
	}

	byBVID := map[string][]string{}
	for _, r := range removed {
		byBVID[r.BVID] = r.Paths
	}
	for _, i := range []int{0, 1} {
		ps, ok := byBVID[groups[i].bvid]
		if !ok {
			t.Fatalf("removed groups %v missing %s", removed, groups[i].bvid)
		}
		if len(ps) != len(groups[i].paths) {
			t.Fatalf("group %s: removed %d paths, want %d", groups[i].bvid, len(ps), len(groups[i].paths))
		}
		for _, want := range groups[i].paths {
			mustNotExist(t, want)
			found := false
			for _, got := range ps {
				if filepath.Clean(got) == filepath.Clean(want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("RemovedGroup(%s).Paths missing %s (got %v)", groups[i].bvid, want, ps)
			}
		}
	}
	for _, i := range []int{2, 3, 4, 5, 6} {
		for _, p := range groups[i].paths {
			mustExist(t, p)
		}
	}
	for _, p := range []string{notes, xmd, logf, subdir} {
		mustExist(t, p)
	}

	got, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List after enforce: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("after enforce: %d groups, want 5", len(got))
	}
	if got[0].BVID != groups[6].bvid {
		t.Fatalf("newest remaining = %s, want %s", got[0].BVID, groups[6].bvid)
	}
}

func TestEnforceLimit_KeepZero_RemovesAllMatchedGroups_KeepsJunk(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a__BV1axx.txt", "a")
	p2 := writeFile(t, dir, "b__BV1bxx.p2.md", "b")
	notes := writeFile(t, dir, "notes.txt", "n")

	removed, err := history.EnforceLimit(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("EnforceLimit(0): %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d groups, want 2", len(removed))
	}
	mustNotExist(t, p1)
	mustNotExist(t, p2)
	mustExist(t, notes)
}

func TestEnforceLimit_NonExistentDir_Noop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	removed, err := history.EnforceLimit(context.Background(), dir, 5)
	if err != nil {
		t.Fatalf("EnforceLimit missing dir: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %d, want 0", len(removed))
	}
}

// TestEnforceLimit_ReadOnlyDir_FailureNoPanic is a real-filesystem
// integration check: a read-only output directory makes every surplus
// unlink fail. EnforceLimit must return a non-nil error, never panic,
// report no removed paths and leave every file in place. (Note: on Unix a
// read-only FILE 0o444 is still unlinkable — directory write permission
// governs unlink — so per-file failure injection lives in the internal
// seam test; chmod on the shared directory cannot let one group fail while
// a sibling still deletes.)
func TestEnforceLimit_ReadOnlyDir_FailureNoPanic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0555 cannot block unlink when running as root")
	}
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a__BV1axx.txt", "a")
	p2 := writeFile(t, dir, "b__BV1bxx.txt", "b")

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var removed []history.RemovedGroup
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("EnforceLimit panicked on delete failure: %v", r)
			}
		}()
		removed, err = history.EnforceLimit(context.Background(), dir, 0)
	}()
	if err == nil {
		t.Fatal("EnforceLimit expected delete errors on read-only dir, got nil")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want filesystem delete errors, not context.Canceled", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %+v, want empty (no successful deletes)", removed)
	}
	mustExist(t, p1)
	mustExist(t, p2)
}

func TestEnforceLimit_CancelledContext_ReturnsCtxErrAndDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a__BV1axx.txt", "a")
	p2 := writeFile(t, dir, "b__BV1bxx.txt", "b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	removed, err := history.EnforceLimit(ctx, dir, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %d, want 0", len(removed))
	}
	mustExist(t, p1)
	mustExist(t, p2)
}

// ---------------------------------------------------------------------------
// RepresentativePath
// ---------------------------------------------------------------------------

func TestRepresentativePath_MatchesListAndContentReadable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "t__BV1xx.md", "md")
	want := writeFile(t, dir, "t__BV1xx.txt", "content-body")
	writeFile(t, dir, "t__BV1xx.p2.txt", "p2")

	listed, err := history.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got, err := history.RepresentativePath(dir, "BV1xx")
	if err != nil {
		t.Fatalf("RepresentativePath: %v", err)
	}
	if got != want {
		t.Fatalf("RepresentativePath = %q, want %q", got, want)
	}
	if got != listed[0].Path {
		t.Fatalf("RepresentativePath %q != List entry Path %q", got, listed[0].Path)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read representative: %v", err)
	}
	if string(body) != "content-body" {
		t.Fatalf("content = %q, want content-body", string(body))
	}
}

func TestRepresentativePath_InvalidBVID(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"",
		" ",
		"\t",
		" BV1xx",
		"BV1 xx",
		"../x",
		"a/b",
		`a\b`,
		"BV1xx\n",
		"BV1\x00xx",
		"..",
		".",
	}
	for _, bvid := range cases {
		if p, err := history.RepresentativePath(dir, bvid); err == nil {
			t.Fatalf("RepresentativePath(%q) = %q, want error", bvid, p)
		}
	}
}

// TestValidateBVID_RuleDriftLock pins the rejection/acceptance set of
// history's unexported validateBVID, reached through RepresentativePath
// (which validates before touching disk). The rule MUST stay in sync with
// naming.validateBvid (internal/naming/naming.go): no empty string, no
// path separators, no parent traversal, no whitespace, no control
// characters. If naming's rules change, update both implementations AND
// this table together.
func TestValidateBVID_RuleDriftLock(t *testing.T) {
	dir := t.TempDir()
	reject := []string{
		"",
		" ",
		"/a",
		`a\b`,
		"a/b",
		"..",
		".",
		"BV1xx\t",
		"BV1xx\n",
		"BV1xx\r",
		"BV1\x00xx",
	}
	for _, bvid := range reject {
		if _, err := history.RepresentativePath(dir, bvid); err == nil {
			t.Errorf("RepresentativePath(%q) accepted, want validation error", bvid)
		}
	}

	// A syntactically valid bvid must PASS validation; with no matching
	// file it then fails with ErrNotFound — not a validation error.
	p, err := history.RepresentativePath(dir, "BV1Dstq6ZEFu")
	if !errors.Is(err, history.ErrNotFound) {
		t.Errorf("valid bvid: err = %v, want history.ErrNotFound (path=%q)", err, p)
	}
}

func TestRepresentativePath_NotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "t__BV1otherx.txt", "other")
	p, err := history.RepresentativePath(dir, "BV1missxx")
	if !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("err = %v, want history.ErrNotFound", err)
	}
	if p != "" {
		t.Fatalf("path = %q on not-found, want empty", p)
	}
}
