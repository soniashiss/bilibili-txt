package webui

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

const testAbsentGrace = 80 * time.Millisecond

// newQuitHarness starts a server in QuitWhenClosed mode with a short absent
// grace. Unlike newServerHarness it performs no cleanup cancellation: Run is
// expected to exit on its own when the client leaves.
func newQuitHarness(t *testing.T) *Server {
	t.Helper()
	restoreTestLogger(t)
	srv, err := New(Config{
		HTTPPort:       0,
		Cfg:            testConfig(t),
		Runner:         newFakeRunner(nil, nil),
		QuitWhenClosed: true,
		AbsentGrace:    testAbsentGrace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-srv.clientQuit:
		default:
		}
	})
	return srv
}

func (h *quitClient) close() {
	if h.body != nil {
		_ = h.body.Close()
	}
}

type quitClient struct {
	body io.ReadCloser
}

func openEventsClient(t *testing.T, addr, token string) *quitClient {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/events?token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Disable keep-alive reuse so closing the body promptly closes the
	// underlying connection and unblocks the server's blocked write.
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("connect SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read SSE hello: %v", err)
	}
	return &quitClient{body: resp.Body}
}

func waitRunDone(t *testing.T, ch <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatalf("Run did not exit within %v", timeout)
		return nil
	}
}

func TestRun_QuitsAfterLastClientCloses(t *testing.T) {
	srv := newQuitHarness(t)
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(context.Background()) }()

	c := openEventsClient(t, srv.Addr(), srv.token)
	c.close()

	if err := waitRunDone(t, runDone, 3*time.Second); err != nil {
		t.Fatalf("Run after close: %v", err)
	}
}

func TestRun_ReconnectWithinGraceKeepsServerAlive(t *testing.T) {
	srv := newQuitHarness(t)
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(context.Background()) }()

	c1 := openEventsClient(t, srv.Addr(), srv.token)
	c1.close()
	// Reconnect well inside the absent grace window.
	time.Sleep(testAbsentGrace / 3)
	c2 := openEventsClient(t, srv.Addr(), srv.token)

	select {
	case err := <-runDone:
		t.Fatalf("Run exited %v while a client reconnected within grace", err)
	case <-time.After(testAbsentGrace * 3):
	}

	c2.close()
	if err := waitRunDone(t, runDone, 3*time.Second); err != nil {
		t.Fatalf("Run after final close: %v", err)
	}
}

func TestRun_OneRemainingClientPreventsQuit(t *testing.T) {
	srv := newQuitHarness(t)
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(context.Background()) }()

	c1 := openEventsClient(t, srv.Addr(), srv.token)
	c2 := openEventsClient(t, srv.Addr(), srv.token)
	c1.close()

	select {
	case err := <-runDone:
		t.Fatalf("Run exited %v with one client still connected", err)
	case <-time.After(testAbsentGrace * 3):
	}

	c2.close()
	if err := waitRunDone(t, runDone, 3*time.Second); err != nil {
		t.Fatalf("Run after final close: %v", err)
	}
}
