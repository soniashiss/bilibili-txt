package webui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

var phaseSteps = map[string]struct{}{
	"metadata":          {},
	"download-subtitle": {},
	"parse-subtitle":    {},
	"download-audio":    {},
	"transcode":         {},
	"transcribe":        {},
	"parse-asr":         {},
	"format":            {},
}

type boundAttr struct {
	groups []string
	attr   slog.Attr
}

type uiHandler struct {
	mu     sync.Mutex
	b      *broker
	attrs  []boundAttr
	groups []string
}

// newUIHandler 创建向 b 推送事件的 handler。b 为 nil 时 handler 处于丢弃模式：
// Handle 不产生任何事件并直接返回 nil（defensive）。
func newUIHandler(b *broker) *uiHandler {
	return &uiHandler{b: b}
}

func (h *uiHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.LevelDebug
}

func (h *uiHandler) Handle(_ context.Context, r slog.Record) error {
	if h.b == nil {
		return nil
	}

	h.mu.Lock()
	bound := make([]boundAttr, len(h.attrs))
	copy(bound, h.attrs)
	groups := make([]string, len(h.groups))
	copy(groups, h.groups)
	h.mu.Unlock()

	// 时间戳由 broker 统一盖在 Event.Time 上（前端据此渲染 [HH:MM:SS]），
	// 这里不再把 r.Time 拼进文本，避免双时间戳。
	var sb strings.Builder
	sb.WriteString(r.Level.String())
	sb.WriteByte(' ')
	sb.WriteString(r.Message)

	step := ""
	for _, ba := range bound {
		appendAttr(&sb, ba.groups, ba.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		if s, ok := topLevelStep(a); ok {
			step = s
		}
		appendAttr(&sb, groups, a)
		return true
	})

	h.b.Log(eventLevel(r.Level), sb.String())
	if status, ok := phaseStatus(r.Message, step); ok {
		h.b.Phase(step, status)
	}
	return nil
}

func (h *uiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	h2 := &uiHandler{b: h.b}
	h2.groups = append([]string(nil), h.groups...)
	h2.attrs = make([]boundAttr, 0, len(h.attrs)+len(attrs))
	h2.attrs = append(h2.attrs, h.attrs...)
	for _, a := range attrs {
		h2.attrs = append(h2.attrs, boundAttr{
			groups: append([]string(nil), h2.groups...),
			attr:   a,
		})
	}
	return h2
}

func (h *uiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	h2 := &uiHandler{b: h.b}
	h2.attrs = append([]boundAttr(nil), h.attrs...)
	h2.groups = make([]string, 0, len(h.groups)+1)
	h2.groups = append(h2.groups, h.groups...)
	h2.groups = append(h2.groups, name)
	return h2
}

func appendAttr(sb *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			nested := make([]string, 0, len(groups)+1)
			nested = append(nested, groups...)
			nested = append(nested, a.Key)
			groups = nested
		}
		for _, ga := range a.Value.Group() {
			appendAttr(sb, groups, ga)
		}
		return
	}
	sb.WriteByte(' ')
	for _, g := range groups {
		sb.WriteString(g)
		sb.WriteByte('.')
	}
	sb.WriteString(a.Key)
	sb.WriteByte('=')
	sb.WriteString(fmt.Sprint(a.Value.Any()))
}

// topLevelStep 只认 record 顶层、非 Group 的字符串属性 step。
// 预绑定属性（WithAttrs）与 Group 嵌套属性都不参与 phase 提取：
// 设计契约要求 pipeline 的 StepStart/Done/Fail 把 step 作为 record 顶层属性发送。
func topLevelStep(a slog.Attr) (string, bool) {
	if a.Key != "step" {
		return "", false
	}
	v := a.Value.Resolve()
	if v.Kind() != slog.KindString {
		return "", false
	}
	return v.String(), true
}

func phaseStatus(msg, step string) (string, bool) {
	var status string
	switch msg {
	case "step start":
		status = "start"
	case "step done":
		status = "done"
	case "step fail":
		status = "fail"
	default:
		return "", false
	}
	if _, ok := phaseSteps[step]; !ok {
		return "", false
	}
	return status, true
}

func eventLevel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

type lineWriter struct {
	b *broker
}

// newLineWriter 创建按行向 b 投递 info 日志的 writer。b 为 nil 时 writer
// 处于丢弃模式：Write 静默吞掉输入并返回 (len(p), nil)（defensive）。
func newLineWriter(b *broker) *lineWriter {
	return &lineWriter{b: b}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if w.b == nil {
		return n, nil
	}
	s := strings.TrimRight(string(p), "\r\n")
	if s != "" {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line != "" {
				w.b.Log("info", line)
			}
		}
	}
	return n, nil
}
