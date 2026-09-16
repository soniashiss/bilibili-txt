package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"bilibili-txt/internal/config"
	"bilibili-txt/internal/history"
	"bilibili-txt/internal/pipeline"
	"bilibili-txt/internal/preflight"
)

const (
	// startupKeep is the number of newest BVID groups retained at startup.
	startupKeep = 5
	// maxJobBody caps the POST /api/jobs request body at 1 MiB.
	maxJobBody = 1 << 20
	// maxTranscriptSize rejects transcript reads above 20 MiB.
	maxTranscriptSize = 20 << 20
	// shutdownGrace bounds each graceful-shutdown stage.
	shutdownGrace = 5 * time.Second
)

// Config configures a Web UI server.
type Config struct {
	// HTTPPort is the port to bind on 127.0.0.1; 0 lets the kernel pick a
	// free port (tests always pass 0).
	HTTPPort int
	// Cfg is the long-lived application config. Required.
	Cfg *config.Config
	// Runner executes jobs. nil makes the server construct the production
	// jobRunner, which builds pipeline.Deps lazily inside Run.
	Runner Runner
	// Deps customises the production jobRunner's dependency builder and is
	// ignored when Runner is set. nil falls back to wire.BuildDeps.
	Deps func(*config.Config) pipeline.Deps
	// Opener opens the server URL in a browser once Run starts serving
	// (Task 12). nil falls back to the platform openBrowser; tests inject
	// a fake here.
	Opener func(url string) error
	// OpenBrowser enables the post-startup browser launch in Run.
	OpenBrowser bool
	// ChromeApp asks macOS to prefer a Chrome --app standalone window;
	// ignored on other platforms and when Opener is injected.
	ChromeApp bool
}

// Server is the local Web UI HTTP server.
type Server struct {
	cfg         *config.Config
	opener      func(url string) error
	openBrowser bool
	chromeApp   bool

	b         *broker
	svc       *service
	token     string
	health    []preflight.CheckResult
	indexTmpl *template.Template
	staticFS  fs.FS

	ln   net.Listener
	http *http.Server
}

// New wires the orchestration layer and binds 127.0.0.1:port. The startup
// sequence is fixed: orchestration -> output directory -> health cache ->
// startup eviction -> token -> listener -> handler assembly.
func New(c Config) (*Server, error) {
	if c.Cfg == nil {
		return nil, errors.New("webui: missing config")
	}
	cfg := c.Cfg

	runner := c.Runner
	if runner == nil {
		jr := newJobRunner(cfg)
		if c.Deps != nil {
			jr.deps = depsBuilder(c.Deps)
		}
		runner = jr
	}
	b := newBroker()
	svc := newService(cfg, b, runner)

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}

	health := preflight.Run(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	removed, err := history.EnforceLimit(ctx, cfg.OutputDir, startupKeep)
	if err != nil {
		b.Log("warn", "启动时清理旧视频失败: "+err.Error())
	}
	for _, g := range removed {
		b.Log("warn", "已自动清理旧视频 "+g.BVID)
	}
	if entries, listErr := history.List(ctx, cfg.OutputDir); listErr != nil {
		b.Log("warn", "读取历史记录失败: "+listErr.Error())
	} else {
		b.History(entries)
	}

	token, err := generateToken()
	if err != nil {
		return nil, err
	}

	indexTmpl, err := template.ParseFS(assetsFS, "assets/index.html")
	if err != nil {
		return nil, err
	}
	staticFS, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:         cfg,
		opener:      c.Opener,
		openBrowser: c.OpenBrowser,
		chromeApp:   c.ChromeApp,
		b:           b,
		svc:         svc,
		token:       token,
		health:      health,
		indexTmpl:   indexTmpl,
		staticFS:    staticFS,
		ln:          ln,
	}
	// ReadTimeout and WriteTimeout are deliberately left unset: the
	// /api/events SSE connection is long-lived, and either deadline would
	// tear it down mid-stream. Slowloris-style header reads are bounded by
	// ReadHeaderTimeout instead, and idle keep-alive connections by
	// IdleTimeout.
	s.http = &http.Server{
		Handler:           s.recoverGuard(s.securityHeaders(s.hostGuard(s.tokenGuard(s.routes())))),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// listenPort extracts the port from a listener address.
func listenPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	i := strings.LastIndex(addr, ":")
	if i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// Addr reports the actual 127.0.0.1:port address of the listener.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Run serves until ctx is canceled, then shuts the service and the HTTP
// server down gracefully (service first so every SSE connection exits
// before http.Server.Shutdown waits on active connections).
func (s *Server) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	if s.openBrowser {
		addr := "http://" + s.Addr() + "/"
		opener := s.opener
		if opener == nil {
			opener = func(u string) error { return openBrowser(u, s.chromeApp) }
		}
		if err := opener(addr); err != nil {
			fmt.Fprintf(os.Stderr, "webui: 打开浏览器失败: %v（请手动访问 %s）\n", err, addr)
		}
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}

	shCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	s.svc.Shutdown(shCtx)
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutCancel()
	if err := s.http.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func generateToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// routes registers every endpoint. The index page and static assets are
// public (behind hostGuard only); every /api route goes through tokenAuth.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded assets carry no version hash, so revalidate on every
		// request instead of letting browsers serve stale copies.
		w.Header().Set("Cache-Control", "no-cache")
		staticHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/cancel", s.handleCancel)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/transcript", s.handleTranscript)

	return mux
}

// knownAPIRoutes is the exact set of registered API routes, used by
// tokenGuard so an unknown /api path answers 401 (no token) or a JSON 404
// instead of ServeMux's plain-text 404.
var knownAPIRoutes = map[string]struct{}{
	"GET /api/health":     {},
	"GET /api/history":    {},
	"POST /api/jobs":      {},
	"POST /api/cancel":    {},
	"GET /api/events":     {},
	"GET /api/transcript": {},
}

// tokenGuard authenticates every /api/ request before routing. The SSE
// endpoint also accepts the token via ?token= because browser
// EventSource cannot set request headers. Comparison is constant-time.
func (s *Server) tokenGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		token := ""
		// RFC 7235: the auth scheme is case-insensitive ("bearer" works),
		// but exactly one space separates scheme from the token.
		if scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok &&
			strings.EqualFold(scheme, "Bearer") && !strings.ContainsRune(rest, ' ') && rest != "" {
			token = rest
		}
		if token == "" && r.URL.Path == "/api/events" {
			token = r.URL.Query().Get("token")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if _, ok := knownAPIRoutes[r.Method+" "+r.URL.Path]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverGuard converts a handler panic into a 500 JSON response and
// prints the stack to the server process's stderr.
func (s *Server) recoverGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Fprintf(os.Stderr, "webui: recovered HTTP panic: %v\n%s\n", rec, debug.Stack())
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务器内部错误"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies clickjacking and content-sniffing defenses to
// every response, including 403/401/404s produced by later guards, so the
// headers must be set before calling the next handler. The SSE handler's
// own Content-Type/Cache-Control coexist with these headers harmlessly.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// hostGuard is a strict loopback whitelist: only the exact
// 127.0.0.1:<port>, localhost:<port> (case-insensitive) and
// [::1]:<port> Host headers pass. An empty, portless, dotted-suffix or
// foreign Host is rejected with 403.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	port := listenPort(s.Addr())
	allowed := map[string]struct{}{
		strings.ToLower(net.JoinHostPort("127.0.0.1", port)): {},
		strings.ToLower(net.JoinHostPort("localhost", port)): {},
		strings.ToLower(net.JoinHostPort("::1", port)):       {},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(strings.TrimSpace(r.Host))
		if host == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "禁止访问"})
			return
		}
		// Validate the syntax (host[:port]) and strip the port before the
		// exact whitelist comparison, so malformed values like
		// "evil.example" cannot reach the comparator in a weird shape.
		h, p, err := net.SplitHostPort(host)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "禁止访问"})
			return
		}
		if p != port {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "禁止访问"})
			return
		}
		if _, ok := allowed[strings.ToLower(net.JoinHostPort(h, p))]; !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "禁止访问"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The HTML embeds the session token in a meta tag; never let caches
	// (including the browser back/forward disk cache) persist a copy.
	w.Header().Set("Cache-Control", "no-store")
	if err := s.indexTmpl.Execute(w, struct {
		Token string
	}{Token: s.token}); err != nil {
		// The response may already be partially written; only the error
		// path matters here and the template itself is compile-time fixed.
		fmt.Fprintf(os.Stderr, "webui: render index: %v\n", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.health)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := history.List(r.Context(), s.cfg.OutputDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取历史记录失败"})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJobBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req struct {
		URL string `json:"url"`
	}
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + safeJSONError(err)})
		return
	}
	switch err := s.svc.Submit(req.URL); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	case errors.Is(err, ErrServiceBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrServiceClosed):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.svc.Cancel()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务器不支持流式响应"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := s.b.subscribe()
	if sub == nil {
		// Shutdown raced ahead: broker already closed.
		return
	}
	defer s.b.unsubscribe(sub)

	for {
		select {
		case ev, open := <-sub.ch:
			if !open {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	bvid := r.URL.Query().Get("bvid")
	path, err := history.RepresentativePath(s.cfg.OutputDir, bvid)
	if err != nil {
		if errors.Is(err, history.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到该视频的文稿"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "视频编号不合法"})
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文稿失败"})
		return
	}
	if info.Size() > maxTranscriptSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "文稿过大，无法在网页查看"})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文稿失败"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// safeJSONError trims the verbose MaxBytesReader tail so the 400 message
// stays user-friendly without leaking server internals.
func safeJSONError(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "请求体超过大小限制"
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err.Error()
	}
	if nerr, ok := err.(*strconv.NumError); ok {
		return nerr.Err.Error()
	}
	return err.Error()
}
