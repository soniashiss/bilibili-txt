// Package history scans the transcript output directory and groups the
// result files by BVID.
//
// The on-disk naming is owned by internal/naming:
//
//	<slug>__<bvid>[.p<n>].<ext>          ext ∈ txt | md | srt
//
// A single-page video has no page segment (treated as page 1); multi-P
// videos carry `.p2`, `.p3`, … (and, when produced by naming.BuildPath
// with totalPages>1, an explicit `.p1`). Files that do not match the
// grammar — notes.txt, *.log, stray extensions, directories — are
// invisible to this package and are never deleted.
//
// List returns EVERY matching group, newest first; callers that only
// want the recent five take a slice themselves. [EnforceLimit] owns the
// "keep N" policy and removes whole surplus groups.
package history

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// namePattern mirrors the grammar rendered by naming.BuildPath.
//
// Deviation from the BV{10} shape seen on bilibili: naming deliberately
// does NOT enforce a BVID length (see naming.validateBvid's doc — short
// placeholders are legitimate), and the multi-P suffix is optional, so
// the tail is `BV[0-9A-Za-z]+` rather than a fixed-length class.
var namePattern = regexp.MustCompile(
	`^(?P<stem>.+)__(?P<bv>BV[0-9A-Za-z]+)(?:\.p(?P<p>\d+))?\.(?P<ext>txt|md|srt)$`)

// ErrNotFound is returned by [RepresentativePath] when a syntactically
// valid BVID has no matching file in the directory. Callers map it to
// HTTP 404.
var ErrNotFound = errors.New("history: no transcript for bvid")

// removeFile is the unlink primitive used by EnforceLimit, indirected as a
// package variable so tests can force per-file removal outcomes
// (ErrNotExist races, permission failures) deterministically. Production
// behaviour is exactly os.Remove.
var removeFile = os.Remove

// Entry is one BVID group as shown in the Web UI's recent list.
type Entry struct {
	BVID  string    `json:"bvid"`
	Name  string    `json:"name"`
	MTime time.Time `json:"mtime"`
	Path  string    `json:"-"`
}

// RemovedGroup describes one BVID group deleted by [EnforceLimit]: the
// group key and every file path actually removed from disk.
type RemovedGroup struct {
	BVID  string
	Paths []string
}

// parsedFile is one regexp-matched directory entry.
type parsedFile struct {
	bvid    string
	base    string
	ext     string
	page    int
	hasPage bool
	mtime   time.Time
	absPath string
}

// group aggregates every matched file sharing a BVID.
type group struct {
	bvid     string
	files    []parsedFile
	maxMtime time.Time
}

// List scans dir and returns every BVID group, ordered by the newest
// mtime in the group (descending); equal mtimes break ties by BVID
// ascending so the order is deterministic. A missing or empty directory
// yields an empty slice and no error.
func List(ctx context.Context, dir string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	groups, err := scanGroups(dir)
	if err != nil {
		return nil, err
	}
	sortGroups(groups)

	out := make([]Entry, 0, len(groups))
	for _, g := range groups {
		rep := representative(g.files)
		out = append(out, Entry{
			BVID:  g.bvid,
			Name:  strings.TrimSuffix(rep.base, "."+rep.ext),
			MTime: g.maxMtime,
			Path:  rep.absPath,
		})
	}
	return out, nil
}

// EnforceLimit keeps the `keep` newest groups and deletes every file of
// the remaining (older) groups, including all pages and all formats.
// Non-matching files are left untouched.
//
// Each group from which THIS call successfully unlinks at least one file
// becomes one [RemovedGroup] listing only the paths that were actually
// removed: a file that vanished between scan and remove (ErrNotExist) was
// not removed by this call and is never listed, so a fully-vanished group
// is not reported at all. Other per-file deletion failures do not stop
// the sweep: remaining files are still attempted and all failures are
// joined into the returned error. ctx is checked before each group; if it
// is already cancelled the function returns ctx.Err() without touching
// disk, and cancellation arriving mid-sweep stops after the current group
// (earlier deletions stay committed and are reported in removed).
func EnforceLimit(ctx context.Context, dir string, keep int) ([]RemovedGroup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	groups, err := scanGroups(dir)
	if err != nil {
		return nil, err
	}
	sortGroups(groups)

	if keep < 0 {
		keep = 0
	}
	var removed []RemovedGroup
	var errs []error
	for i := keep; i < len(groups); i++ {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			return removed, errors.Join(errs...)
		}
		g := groups[i]
		rg := RemovedGroup{BVID: g.bvid}
		for _, f := range g.files {
			err := removeFile(f.absPath)
			switch {
			case err == nil:
				rg.Paths = append(rg.Paths, f.absPath)
			case errors.Is(err, fs.ErrNotExist):
				// The file vanished between scan and remove (a concurrent
				// sweep). It was not removed by THIS call, so it is not
				// "actually removed": skip it silently and leave it out of
				// Paths.
			default:
				errs = append(errs, err)
			}
		}
		if len(rg.Paths) > 0 {
			removed = append(removed, rg)
		}
	}
	return removed, errors.Join(errs...)
}

// RepresentativePath returns the absolute path of the representative
// file for one BVID using the same total order as [List]/[EnforceLimit].
//
// bvid is strictly validated (no empty string, path separators,
// traversal, whitespace or control characters) and is only ever matched
// against parsed group keys — it is never joined into a path, so path
// traversal is impossible. A valid BVID with no file returns
// ErrNotFound.
func RepresentativePath(dir, bvid string) (string, error) {
	if err := validateBVID(bvid); err != nil {
		return "", err
	}
	groups, err := scanGroups(dir)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.bvid == bvid {
			return representative(g.files).absPath, nil
		}
	}
	return "", &notFoundError{bvid: bvid}
}

type notFoundError struct{ bvid string }

func (e *notFoundError) Error() string {
	return "history: no transcript for bvid " + e.bvid
}

func (e *notFoundError) Is(target error) bool { return target == ErrNotFound }

// validateBVID enforces the same minimal invariants as naming's
// unexported validateBvid.
func validateBVID(bvid string) error {
	if bvid == "" {
		return errors.New("history: empty bvid")
	}
	if strings.ContainsAny(bvid, `/\`) {
		return errors.New("history: bvid contains path separator")
	}
	if bvid == "." || strings.Contains(bvid, "..") {
		return errors.New("history: bvid contains parent traversal")
	}
	for _, r := range bvid {
		if unicode.IsSpace(r) {
			return errors.New("history: bvid contains whitespace")
		}
		if unicode.IsControl(r) {
			return errors.New("history: bvid contains control character")
		}
	}
	return nil
}

// scanGroups reads dir once and buckets matched files by BVID. A missing
// directory is treated as empty.
func scanGroups(dir string) ([]*group, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []*group{}, nil
		}
		return nil, err
	}

	byBVID := map[string]*group{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := namePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		f := parsedFile{
			bvid:    m[nameIndex("bv")],
			base:    e.Name(),
			ext:     m[nameIndex("ext")],
			page:    1,
			hasPage: m[nameIndex("p")] != "",
			mtime:   info.ModTime(),
		}
		if f.hasPage {
			n := parsePage(m[nameIndex("p")])
			f.page = n
		}
		f.absPath = filepath.Join(dir, f.base)
		if abs, aerr := filepath.Abs(f.absPath); aerr == nil {
			f.absPath = abs
		}

		g := byBVID[f.bvid]
		if g == nil {
			g = &group{bvid: f.bvid, maxMtime: f.mtime}
			byBVID[f.bvid] = g
		}
		if f.mtime.After(g.maxMtime) {
			g.maxMtime = f.mtime
		}
		g.files = append(g.files, f)
	}

	groups := make([]*group, 0, len(byBVID))
	for _, g := range byBVID {
		groups = append(groups, g)
	}
	return groups, nil
}

// sortGroups orders by newest group mtime desc, BVID asc.
func sortGroups(groups []*group) {
	sort.Slice(groups, func(i, j int) bool {
		if !groups[i].maxMtime.Equal(groups[j].maxMtime) {
			return groups[i].maxMtime.After(groups[j].maxMtime)
		}
		return groups[i].bvid < groups[j].bvid
	})
}

// representative picks the group's display file using one total order
// shared by [List] and [RepresentativePath], so the two can never point
// at different files. ([EnforceLimit] deletes whole groups and never
// selects a representative.)
//
//  1. page ascending (no `.pN` segment == page 1, and for an equal page
//     the suffix-less file sorts before an explicit `.p1`);
//  2. format priority txt > md > srt;
//  3. basename ascending (final deterministic tie-break).
func representative(files []parsedFile) parsedFile {
	best := files[0]
	for _, f := range files[1:] {
		if lessFile(f, best) {
			best = f
		}
	}
	return best
}

func lessFile(a, b parsedFile) bool {
	if a.page != b.page {
		return a.page < b.page
	}
	if a.hasPage != b.hasPage {
		return !a.hasPage // suffix-less P1 first
	}
	if ra, rb := formatRank(a.ext), formatRank(b.ext); ra != rb {
		return ra < rb
	}
	return a.base < b.base
}

func formatRank(ext string) int {
	switch ext {
	case "txt":
		return 0
	case "md":
		return 1
	default: // srt
		return 2
	}
}

func nameIndex(name string) int {
	for i, n := range namePattern.SubexpNames() {
		if n == name {
			return i
		}
	}
	return -1
}

// parsePage converts the `\d+` page segment, clamping values that do not
// fit an int; all real inputs are tiny.
func parsePage(s string) int {
	n := 0
	for _, r := range s {
		d := int(r - '0')
		if n > (int(^uint(0)>>1)-d)/10 {
			return int(^uint(0) >> 1)
		}
		n = n*10 + d
	}
	if n == 0 {
		return 1
	}
	return n
}
