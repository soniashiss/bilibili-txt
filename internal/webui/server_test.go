package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bilibili-txt/internal/biliurl"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/history"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/pipeline"
	"bilibili-txt/internal/preflight"
)

type serverHarness struct {
	t       *testing.T
	srv     *Server
	cfg     *config.Config
	runDone chan error
	client  *http.Client

	runCtx    context.Context
	runCancel context.CancelFunc
}

func newServerHarness(t *testing.T, runner Runner, deps func(*config.Config) pipeline.Deps) *serverHarness {
	t.Helper()
	restoreTestLogger(t)
	cfg := testConfig(t)

	srv, err := New(Config{
		HTTPPort: 0,
		Cfg:      cfg,
		Runner:   runner,
		Deps:     deps,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &serverHarness{
		t:         t,
		srv:       srv,
		cfg:       cfg,
		runDone:   make(chan error),
		client:    &http.Client{},
		runCtx:    ctx,
		runCancel: cancel,
	}
	go func() {
		err := srv.Run(ctx)
		h.runDone <- err
		close(h.runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.runDone:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("Run did not return within 5s of context cancellation")
		}
	})
	return h
}

// waitRun waits for Run to exit and returns its error.
func (h *serverHarness) waitRun() error {
	h.t.Helper()
	select {
	case err := <-h.runDone:
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not return within 5s")
		return nil
	}
}

func (h *serverHarness) base() string {
	return "http://" + h.srv.Addr()
}

func (h *serverHarness) token() string {
	return h.srv.token
}

// do issues a request. auth controls the Authorization header:
// "" sends no header, "BEARER" sends the real token, anything else is
// used verbatim as the raw header value.
func (h *serverHarness) do(method, path, body, auth string) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.base()+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch auth {
	case "":
	case "BEARER":
		req.Header.Set("Authorization", "Bearer "+h.token())
	default:
		req.Header.Set("Authorization", auth)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func postJSON(t *testing.T, h *serverHarness, path string, v any, auth string) *http.Response {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return h.do(http.MethodPost, path, string(b), auth)
}

type sseClient struct {
	resp *http.Response
	sc   *bufio.Scanner
	ch   chan string
	last string
}

func (h *serverHarness) openSSE(tokenStyle string) *sseClient {
	h.t.Helper()
	path := "/api/events"
	if tokenStyle != "" {
		path += "?token=" + tokenStyle
	}
	resp, err := h.client.Get(h.base() + path)
	if err != nil {
		h.t.Fatalf("GET /api/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body := readBody(h.t, resp)
		h.t.Fatalf("SSE connect status = %d body=%q, want 200", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		h.t.Fatalf("SSE Content-Type = %q, want text/event-stream", ct)
	}
	c := &sseClient{
		resp: resp,
		sc:   bufio.NewScanner(resp.Body),
		ch:   make(chan string, 64),
	}
	go func() {
		for c.sc.Scan() {
			line := c.sc.Text()
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				c.last = data
				c.ch <- data
			}
		}
		close(c.ch)
	}()
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return c
}

func (c *sseClient) nextEvent(t *testing.T) (Event, bool) {
	t.Helper()
	select {
	case line, ok := <-c.ch:
		if !ok {
			return Event{}, false
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid SSE JSON %q: %v", line, err)
		}
		return ev, true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
		return Event{}, false
	}
}

// waitFor blocks until an event satisfying pred arrives; startup history
// snapshots are ignored by the caller's predicate.
func (c *sseClient) waitFor(t *testing.T, pred func(Event) bool) Event {
	t.Helper()
	for {
		ev, ok := c.nextEvent(t)
		if !ok {
			t.Fatal("SSE stream closed before the expected event arrived")
		}
		if pred(ev) {
			return ev
		}
	}
}

// closed drains any events already buffered in the channel (e.g. ones a
// predicate-based wait skipped) and then asserts the stream itself is
// closed, without ever flushing a zero-value frame. The last data frame
// received before the close must not be a zero-value Event.
func (c *sseClient) closed(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.ch:
			if !ok {
				if c.last == "{}" {
					t.Fatal("last SSE frame before close was a zero-value data frame")
				}
				return
			}
		case <-deadline:
			t.Fatal("SSE stream was not closed within 5s")
		}
	}
}

var tokenMetaRe = regexp.MustCompile(`name="token" content="([^"]+)"`)

// TestServer_IndexAndHostGuard covers plan test 1: the index page renders
// the token, while every Host header outside the exact loopback
// whitelist is rejected with 403.
func TestServer_IndexAndHostGuard(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	m := tokenMetaRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("index HTML is missing <meta name=\"token\">")
	}
	if m[1] != h.token() || len(m[1]) != 64 {
		t.Fatalf("meta token = %q (len %d), want the server's 64-char hex token", m[1], len(m[1]))
	}
	if !strings.Contains(body, `/static/app.css`) || !strings.Contains(body, `/static/app.js`) {
		t.Fatal("index HTML must reference /static/app.css and /static/app.js")
	}

	hostCases := []struct {
		name string
		host string
		want int
	}{
		{"foreign host", "evil.example", http.StatusForbidden},
		{"wrong port", "127.0.0.1:1", http.StatusForbidden},
		{"empty host", "", http.StatusForbidden},
		{"trailing dot", "localhost.", http.StatusForbidden},
		{"trailing dot with port", "localhost.:" + portOf(h.srv.Addr()), http.StatusForbidden},
		{"foreign host with our port", "evil.example:" + portOf(h.srv.Addr()), http.StatusForbidden},
		{"loopback ip", h.srv.Addr(), http.StatusOK},
		{"localhost with port", "localhost:" + portOf(h.srv.Addr()), http.StatusOK},
		{"uppercase localhost", "LOCALHOST:" + portOf(h.srv.Addr()), http.StatusOK},
		{"other loopback ip", "127.0.0.2:" + portOf(h.srv.Addr()), http.StatusForbidden},
		{"leading zero port", "127.0.0.1:0" + portOf(h.srv.Addr()), http.StatusForbidden},
		{"ipv6 loopback", "[::1]:" + portOf(h.srv.Addr()), http.StatusOK},
		{"ipv6 loopback long form", "[0:0:0:0:0:0:0:1]:" + portOf(h.srv.Addr()), http.StatusForbidden},
		{"ipv4-mapped ipv6 loopback", "[::ffff:127.0.0.1]:" + portOf(h.srv.Addr()), http.StatusForbidden},
	}
	for _, tc := range hostCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.host == "" {
				// http.Client refills an empty Host from the URL, so send
				// a raw request with literally no Host header over TCP.
				if code := rawRequestStatus(h.srv.Addr(), "GET / HTTP/1.0\r\n\r\n"); code != tc.want {
					t.Fatalf("no Host header status = %d, want %d", code, tc.want)
				}
				return
			}
			req, err := http.NewRequest(http.MethodGet, h.base()+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("Host %q status = %d, want %d", tc.host, resp.StatusCode, tc.want)
			}
		})
	}
}

// rawRequestStatus opens a TCP connection to addr and sends req, returning
// the parsed HTTP status code. It exists for cases http.Client will not
// faithfully reproduce (a completely absent Host header).
func rawRequestStatus(addr, req string) int {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return -1
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, req); err != nil {
		return -1
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return -1
	}
	line := sc.Text()
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return -1
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return code
}

func portOf(addr string) string {
	i := strings.LastIndex(addr, ":")
	return addr[i+1:]
}

// TestServer_HistoryAuth covers plan test 2: every /api endpoint needs
// the token (Bearer or ?token= for events), and the payload is a JSON
// array.
func TestServer_HistoryAuth(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	if resp := h.do(http.MethodGet, "/api/history", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	} else {
		body := readBody(t, resp)
		if !strings.Contains(body, "unauthorized") {
			t.Fatalf("401 body = %q, want {\"error\":\"unauthorized\"}", body)
		}
	}
	if resp := h.do(http.MethodGet, "/api/history", "", "Bearer wrong-token"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer = %d, want 401", resp.StatusCode)
	}

	resp := h.do(http.MethodGet, "/api/history", "", "BEARER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid bearer = %d, want 200", resp.StatusCode)
	}
	var entries []history.Entry
	if err := json.Unmarshal([]byte(readBody(t, resp)), &entries); err != nil {
		t.Fatalf("history payload is not a JSON array: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty dir history = %d entries, want 0", len(entries))
	}

	if resp := h.do(http.MethodGet, "/api/history?token=wrong", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad query token = %d, want 401", resp.StatusCode)
	}
	// ?token= is honoured for the SSE endpoint; the generic API surface
	// keeps to the Bearer scheme, which is verified on /api/events in the
	// SSE tests.
	if c := h.openSSE(h.token()); c == nil {
		t.Fatal("SSE connect with ?token=<good> failed")
	}
}

// TestServer_JobsBusyCancelResubmit covers plan test 3: 202 on submit,
// 409 while busy, and a new submission accepted only AFTER the canceled
// terminal + history have been observed (event-driven, no sleeps).
func TestServer_JobsBusyCancelResubmit(t *testing.T) {
	r := newFakeRunner(nil, context.Canceled)
	r.block = true
	h := newServerHarness(t, r, nil)
	sse := h.openSSE(h.token())

	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST /api/jobs = %d %s, want 202", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	<-r.started

	resp = postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second POST while busy = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/cancel", "", "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/cancel = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	sse.waitFor(t, func(e Event) bool { return e.Type == "canceled" })
	sse.waitFor(t, func(e Event) bool { return e.Type == "history" })

	// The first fake is finished (canceled); give the service a healthy
	// runner for the immediate resubmission. The idle window must already
	// be closed by the time the terminal event was delivered.
	h.srv.svc.runner = newFakeRunner(successResult(h.cfg.OutputDir), nil)
	h.srv.svc.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.srv.svc.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	resp = postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resubmit after canceled terminal = %d %s, want 202 (idle window must not exist)",
			resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	sse.waitFor(t, func(e Event) bool { return e.Type == "done" })
}

// TestServer_SSEReplayAndClose covers plan test 4: live events parse as
// JSON, a late connection replays the done snapshot, and broker shutdown
// ends the stream without flushing a zero-value frame.
func TestServer_SSEReplayAndClose(t *testing.T) {
	r := newFakeRunner(successResult(t.TempDir()), nil)
	r.emitPhase = true
	h := newServerHarness(t, r, nil)

	live := h.openSSE(h.token())
	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/jobs = %d", resp.StatusCode)
	}
	resp.Body.Close()

	var sawPhase, sawDone, sawHistory bool
	for !(sawPhase && sawDone && sawHistory) {
		ev := live.waitFor(t, func(e Event) bool {
			return e.Type == "phase" || e.Type == "done" || e.Type == "history"
		})
		switch ev.Type {
		case "phase":
			if ev.Step != "metadata" || ev.Status != "start" {
				t.Fatalf("phase = %q/%q", ev.Step, ev.Status)
			}
			sawPhase = true
		case "done":
			sawDone = true
		case "history":
			sawHistory = true
		}
	}

	// Late connection: the done terminal snapshot and the buffered log
	// events must both be replayed. Replay delivers buffered logs before
	// the terminal snapshot, so record them while waiting for done.
	late := h.openSSE(h.token())
	var lateSawLog bool
	for {
		ev, ok := late.nextEvent(t)
		if !ok {
			t.Fatal("late SSE stream closed before done was replayed")
		}
		if ev.Type == "log" {
			lateSawLog = true
		}
		if ev.Type == "done" {
			break
		}
	}
	if !lateSawLog {
		t.Fatal("late connection did not replay buffered log events")
	}

	// Graceful shutdown closes every subscription; the handler must exit
	// without writing a zero-value "data: {}" frame.
	h.runCancel()
	if err := h.waitRun(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	live.closed(t)
	late.closed(t)
}

// TestServer_Transcript covers plan test 5.
func TestServer_Transcript(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	p := filepath.Join(h.cfg.OutputDir, "我的视频__"+testBVID+".txt")
	content := "第一行\n第二行 transcript 内容\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodGet, "/api/transcript?bvid="+testBVID, "", "BEARER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript = %d %s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if got := readBody(t, resp); got != content {
		t.Fatalf("transcript body = %q, want %q", got, content)
	}

	for _, bad := range []string{"..", "a/b", " ", `a\b`, "a\tb", ""} {
		resp := h.do(http.MethodGet, "/api/transcript?bvid="+url.QueryEscape(bad), "", "BEARER")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bvid %q status = %d, want 400", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp = h.do(http.MethodGet, "/api/transcript?bvid=BVnonexist00", "", "BEARER")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing bvid = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestServer_Health covers plan test 6: cached preflight results,
// directly deserializable into []preflight.CheckResult.
func TestServer_Health(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/api/health", "", "BEARER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	var results []preflight.CheckResult
	if err := json.Unmarshal([]byte(readBody(t, resp)), &results); err != nil {
		t.Fatalf("health is not []preflight.CheckResult: %v", err)
	}
	if len(results) < 6 {
		t.Fatalf("preflight results = %d, want >= 6", len(results))
	}
	var sawOutputDir bool
	for _, r := range results {
		if r.Name == "output dir" {
			sawOutputDir = true
			if !r.OK {
				t.Fatalf("output dir check = %+v, want OK", r)
			}
		}
	}
	if !sawOutputDir {
		t.Fatalf("results %+v must include the output dir check", results)
	}
}

// TestServer_BadJobRequest covers plan test 7.
func TestServer_BadJobRequest(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": "这根本不是链接"}, "BEARER")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad url = %d, want 400", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.Unmarshal([]byte(readBody(t, resp)), &errBody); err != nil {
		t.Fatalf("bad url body not JSON: %v", err)
	}
	if errBody["error"] == "" {
		t.Fatal("400 body must carry an error message")
	}

	resp = h.do(http.MethodPost, "/api/jobs", "{not json", "BEARER")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/jobs", `{"url":"`+testURL()+`","extra":1}`, "BEARER")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestServer_Static covers plan test 8.
func TestServer_Static(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/static/app.css", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app.css = %d", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "grid-template-columns") {
		t.Fatal("app.css body looks wrong")
	}

	resp = h.do(http.MethodGet, "/static/app.js", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app.js = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodGet, "/static/does-not-exist", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown static file = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestStatic_AssetsAreComplete pins the Task 13 frontend contract: the
// embedded JS drives SSE/job submission, the CSS lays out the three
// columns, and the rendered index carries every panel id with the token
// placeholder already replaced by the real token.
func TestStatic_AssetsAreComplete(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/static/app.js", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app.js = %d, want 200", resp.StatusCode)
	}
	js := readBody(t, resp)
	for _, want := range []string{"EventSource", "/api/jobs", "addEventListener", "textContent"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}

	resp = h.do(http.MethodGet, "/static/app.css", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app.css = %d, want 200", resp.StatusCode)
	}
	css := readBody(t, resp)
	for _, want := range []string{"grid-template-columns", "white-space: pre-wrap", "-webkit-line-clamp"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css missing %q", want)
		}
	}

	resp = h.do(http.MethodGet, "/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	html := readBody(t, resp)
	for _, want := range []string{`id="url"`, `id="go"`, `id="urlerr"`, `id="healthbar"`,
		`id="logs"`, `id="reader"`, `id="reader-status"`, `id="reader-content"`,
		`id="history"`, `id="history-list"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered index missing %s", want)
		}
	}
	if strings.Contains(html, "{{.Token}}") {
		t.Fatal("rendered index must not contain the unresolved {{.Token}} placeholder")
	}
	if !strings.Contains(html, h.token()) {
		t.Fatal("rendered index must contain the real session token")
	}
}

// TestServer_UnknownAPIRoute verifies unknown API paths stay behind the
// token guard and return a JSON 404 afterwards (no ServeMux plain text).
func TestServer_UnknownAPIRoute(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	if resp := h.do(http.MethodGet, "/api/nope", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown route without token = %d, want 401", resp.StatusCode)
	}
	resp := h.do(http.MethodGet, "/api/nope", "", "BEARER")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route with token = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(readBody(t, resp)), &body); err != nil {
		t.Fatalf("404 body not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("404 body must carry an error field")
	}

	// Wrong method on a known path is also an unknown route (404, not
	// ServeMux's 405 plain text) behind a valid token.
	if resp := h.do(http.MethodDelete, "/api/history", "", "BEARER"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE /api/history = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestServer_StartupEviction covers plan test 9: EnforceLimit runs during
// New, the two oldest of seven groups are deleted wholesale and the
// cleanup warnings + history snapshot reach a client connecting AFTER
// startup via the broker replay buffer.
func TestServer_StartupEviction(t *testing.T) {
	restoreTestLogger(t)
	dir := filepath.Join(t.TempDir(), "output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-7 * 24 * time.Hour)
	for i := 1; i <= 7; i++ {
		for _, ext := range []string{"txt", "srt"} {
			p := filepath.Join(dir, fmt.Sprintf("title%d__BV100000%d.%s", i, i, ext))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			m := base.Add(time.Duration(i) * time.Hour)
			if err := os.Chtimes(p, m, m); err != nil {
				t.Fatal(err)
			}
		}
	}

	cfg := testConfig(t)
	cfg.OutputDir = dir
	srv, err := New(Config{HTTPPort: 0, Cfg: cfg, Runner: newFakeRunner(nil, nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()
	defer func() {
		cancel()
		<-runErr
	}()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("after New, %d files remain, want 10 (5 groups x 2 files)", len(entries))
	}
	for i := 1; i <= 2; i++ {
		for _, ext := range []string{"txt", "srt"} {
			p := filepath.Join(dir, fmt.Sprintf("title%d__BV100000%d.%s", i, i, ext))
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("evicted file %s still present: %v", filepath.Base(p), err)
			}
		}
	}

	h := &serverHarness{t: t, srv: srv, cfg: cfg, client: &http.Client{}}
	sse := h.openSSE(srv.token)

	var warns int
	var deletedText strings.Builder
	for {
		ev := sse.waitFor(t, func(e Event) bool {
			return (e.Type == "log" && strings.Contains(e.Text, "已自动清理旧视频 ")) || e.Type == "history"
		})
		if ev.Type == "log" {
			warns++
			deletedText.WriteString(ev.Text)
			deletedText.WriteByte('|')
			continue
		}
		if warns < 2 {
			t.Fatalf("history arrived after only %d cleanup warnings, want >= 2", warns)
		}
		list, ok := ev.Data.([]any)
		if !ok || len(list) != 5 {
			t.Fatalf("startup history payload = %#v, want 5 entries", ev.Data)
		}
		break
	}
	joined := deletedText.String()
	if !strings.Contains(joined, "BV1000001") || !strings.Contains(joined, "BV1000002") {
		t.Fatalf("cleanup warnings %q must name the two oldest groups", joined)
	}
}

// TestServer_CreatesMissingOutputDir pins New startup step 2: a
// nonexistent output directory is created before preflight runs.
func TestServer_CreatesMissingOutputDir(t *testing.T) {
	restoreTestLogger(t)
	dir := filepath.Join(t.TempDir(), "nested", "transcripts")
	cfg := testConfig(t)
	cfg.OutputDir = dir
	srv, err := New(Config{HTTPPort: 0, Cfg: cfg})
	if err != nil {
		t.Fatalf("New with missing dir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("output dir was not created: %v", err)
	}
	_ = srv
}

// TestServer_DepsBuildTiming covers plan test 10, route (a): with no fake
// Runner, the injected Deps builder runs inside the production
// jobRunner.Run AFTER logger.Init, so a probe line written via
// logger.StderrSink reaches the SSE stream. Zero-value Deps then fail
// fast; the terminal state is irrelevant to the timing assertion.
func TestServer_DepsBuildTiming(t *testing.T) {
	probeCalled := make(chan struct{}, 1)
	deps := func(*config.Config) pipeline.Deps {
		if _, err := logger.StderrSink("yt-dlp").Write([]byte("[probe]\n")); err != nil {
			t.Errorf("probe write: %v", err)
		}
		select {
		case probeCalled <- struct{}{}:
		default:
		}
		return pipeline.Deps{}
	}
	h := newServerHarness(t, nil, deps)
	sse := h.openSSE(h.token())

	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/jobs = %d", resp.StatusCode)
	}
	resp.Body.Close()

	sse.waitFor(t, func(e Event) bool {
		return e.Type == "log" && strings.Contains(e.Text, "[probe]")
	})
}

// ctxAwareRunner signals both start and ctx cancellation.
type ctxAwareRunner struct {
	started  chan struct{}
	canceled chan struct{}
}

func (r *ctxAwareRunner) Run(ctx context.Context, _ string) (*pipeline.Result, error) {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	return nil, ctx.Err()
}

// TestServer_GracefulShutdown covers extra tests 11: cancelling Run's
// context stops the running job and returns nil promptly.
func TestServer_GracefulShutdown(t *testing.T) {
	r := &ctxAwareRunner{started: make(chan struct{}), canceled: make(chan struct{})}
	h := newServerHarness(t, r, nil)

	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/jobs = %d", resp.StatusCode)
	}
	resp.Body.Close()
	<-r.started

	h.runCancel()
	if err := h.waitRun(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	select {
	case <-r.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("running job never observed context cancellation")
	}
}

// TestServer_TokenNotLeaked covers extra test 12: a rejected response
// must never echo the real token.
func TestServer_TokenNotLeaked(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	resp := h.do(http.MethodGet, "/api/history", "", "Bearer nope")
	body := readBody(t, resp)
	if strings.Contains(body, h.token()) {
		t.Fatal("401 response leaks the real token")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestServer_MetaTokenWorksAsBearer covers extra test 13: the token from
// the index page meta is exactly the value the API accepts.
func TestServer_MetaTokenWorksAsBearer(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	body := readBody(t, h.do(http.MethodGet, "/", "", ""))
	m := tokenMetaRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("missing token meta")
	}
	req, err := http.NewRequest(http.MethodGet, h.base()+"/api/history", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+m[1])
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta token as bearer = %d, want 200", resp.StatusCode)
	}
}

// TestServer_ClosedServiceRejectsJobs pins the 503 mapping: once the
// service has shut down (the HTTP server itself is still serving, as
// during the shutdown ordering window), a job submission gets 503.
func TestServer_ClosedServiceRejectsJobs(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	shCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.srv.svc.Shutdown(shCtx)

	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": testURL()}, "BEARER")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("submit after service shutdown = %d, want 503", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "error") {
		t.Fatalf("503 body = %q, want JSON error", body)
	}
}

// TestServer_BiliurlErrorShape ensures the 400 body carries the biliurl
// message verbatim (regression guard for thin-handler mapping).
func TestServer_BiliurlErrorShape(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	bad := "这根本不是一个链接"
	_, wantErr := biliurl.Extract(bad)
	if wantErr == nil {
		t.Fatal("test setup: biliurl unexpectedly accepted the bad URL")
	}
	resp := postJSON(t, h, "/api/jobs", map[string]string{"url": bad}, "BEARER")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.Unmarshal([]byte(readBody(t, resp)), &errBody); err != nil {
		t.Fatalf("400 body is not JSON: %v", err)
	}
	if errBody["error"] != wantErr.Error() {
		t.Fatalf("400 error = %q, want %q", errBody["error"], wantErr.Error())
	}
}

// assertSecurityHeaders verifies the four clickjacking/sniffing headers on
// a response, regardless of its status code.
func assertSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for name, value := range want {
		if got := resp.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

// TestServer_SecurityHeaders pins the global security-header middleware on
// representative 200/401 responses, the index no-store directive and the
// static-asset no-cache directive.
func TestServer_SecurityHeaders(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("index Cache-Control = %q, want no-store", cc)
	}
	resp.Body.Close()

	resp = h.do(http.MethodGet, "/api/health", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("health without token = %d, want 401", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
	resp.Body.Close()

	resp = h.do(http.MethodGet, "/api/health", "", "BEARER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health with token = %d, want 200", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
	resp.Body.Close()

	resp = h.do(http.MethodGet, "/static/app.css", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app.css = %d, want 200", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("static Cache-Control = %q, want no-cache", cc)
	}
	resp.Body.Close()

	// A hostGuard 403 must carry the headers too, since the middleware
	// wraps hostGuard.
	req, err := http.NewRequest(http.MethodGet, h.base()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example:" + portOf(h.srv.Addr())
	resp, err = h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forbidden host = %d, want 403", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
}

// TestAuth_BearerCaseInsensitive pins RFC 7235 scheme comparison: a
// lowercase "bearer <token>" must authenticate.
func TestAuth_BearerCaseInsensitive(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	resp := h.do(http.MethodGet, "/api/history", "", "bearer "+h.token())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lowercase bearer = %d %s, want 200", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	// Exactly one separator space stays mandatory.
	if resp := h.do(http.MethodGet, "/api/history", "", "Bearer  "+h.token()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("double-space bearer = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestJobs_BodyTooLarge pins the 1 MiB MaxBytesReader cap on job
// submissions.
func TestJobs_BodyTooLarge(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	body := `{"url":"` + strings.Repeat("a", 1_200_000) + `"}`
	req, err := http.NewRequest(http.MethodPost, h.base()+"/api/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token())
	resp, err := h.client.Do(req)
	if err != nil {
		// MaxBytesReader may abort the connection before a response is
		// delivered; a transport error is an acceptable rejection too.
		t.Logf("oversized body transport error (acceptable): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized body status = %d, want rejection (400/closed connection)", resp.StatusCode)
	}
	t.Logf("oversized body status = %d", resp.StatusCode)
}

// TestStatic_PathTraversal ensures dot-segment tricks cannot make the
// embedded file server serve files outside assets/.
func TestStatic_PathTraversal(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)

	for _, p := range []string{"/static/../server.go", "/static/%2e%2e/server.go"} {
		resp := h.do(http.MethodGet, p, "", "")
		body := readBody(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("GET %s = 200, want 301/400/404", p)
		}
		if strings.Contains(body, "securityHeaders applies clickjacking") {
			t.Fatalf("GET %s leaked server.go source", p)
		}
	}
}

// TestTranscript_MultiBVIDParam pins first-value semantics: a second
// traversal-shaped bvid must never influence the resolved path.
func TestTranscript_MultiBVIDParam(t *testing.T) {
	h := newServerHarness(t, newFakeRunner(nil, nil), nil)
	p := filepath.Join(h.cfg.OutputDir, "我的视频__"+testBVID+".txt")
	content := "multi-bvid transcript\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodGet,
		"/api/transcript?bvid="+testBVID+"&bvid="+url.QueryEscape(".."), "", "BEARER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid bvid first = %d %s, want 200 (second value must be ignored)",
			resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); got != content {
		t.Fatalf("body = %q, want %q", got, content)
	}

	resp = h.do(http.MethodGet,
		"/api/transcript?bvid="+url.QueryEscape("..")+"&bvid="+testBVID, "", "BEARER")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("traversal bvid first = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// runServerWithOpener builds a server with the injected Opener and runs
// it, mirroring newServerHarness for the browser-open tests.
func runServerWithOpener(t *testing.T, openBrowser bool, opener func(string) error) *serverHarness {
	t.Helper()
	restoreTestLogger(t)
	cfg := testConfig(t)
	srv, err := New(Config{
		HTTPPort:    0,
		Cfg:         cfg,
		Runner:      newFakeRunner(nil, nil),
		Opener:      opener,
		OpenBrowser: openBrowser,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &serverHarness{
		t:         t,
		srv:       srv,
		cfg:       cfg,
		runDone:   make(chan error),
		client:    &http.Client{},
		runCtx:    ctx,
		runCancel: cancel,
	}
	go func() {
		err := srv.Run(ctx)
		h.runDone <- err
		close(h.runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.runDone:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("Run did not return within 5s of context cancellation")
		}
	})
	return h
}

// TestRun_OpensBrowserWhenConfigured pins Task 12: with OpenBrowser the
// injected Opener is called exactly once after Serve starts accepting,
// with the root URL (no token in the URL).
func TestRun_OpensBrowserWhenConfigured(t *testing.T) {
	var mu sync.Mutex
	openCount := 0
	opened := make(chan string, 1)
	h := runServerWithOpener(t, true, func(u string) error {
		mu.Lock()
		openCount++
		if openCount > 1 {
			t.Errorf("opener called %d times, want exactly 1", openCount)
		}
		mu.Unlock()
		select {
		case opened <- u:
		default:
		}
		return nil
	})

	var got string
	select {
	case got = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("opener was not called within 5s of Run")
	}
	want := "http://" + h.srv.Addr() + "/"
	if got != want {
		t.Fatalf("opener url = %q, want %q", got, want)
	}
	if strings.Contains(got, h.token()) {
		t.Fatal("opened URL must not contain the session token")
	}

	h.runCancel()
	if err := h.waitRun(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if openCount != 1 {
		t.Fatalf("opener called %d times, want exactly 1", openCount)
	}
}

// TestRun_NoBrowserByDefault pins Task 12: with OpenBrowser false the
// Opener is never invoked.
func TestRun_NoBrowserByDefault(t *testing.T) {
	opened := make(chan struct{}, 1)
	h := runServerWithOpener(t, false, func(string) error {
		select {
		case opened <- struct{}{}:
		default:
		}
		return nil
	})

	if resp := h.do(http.MethodGet, "/api/health", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("health = %d, want 401 (server should still be serving)", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	select {
	case <-opened:
		t.Fatal("opener must not be called when OpenBrowser is false")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRun_OpenerFailureNonFatal pins Task 12: an opener error only warns
// to stderr; Run keeps serving and still exits cleanly on cancel.
func TestRun_OpenerFailureNonFatal(t *testing.T) {
	h := runServerWithOpener(t, true, func(string) error {
		return errors.New("no browser")
	})

	if resp := h.do(http.MethodGet, "/api/health", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("health after opener failure = %d, want 401 (server must still serve)", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	h.runCancel()
	if err := h.waitRun(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
}
