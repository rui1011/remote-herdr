// Package server is the HTTP layer of remote-herdr.
//
// The API only exposes: list agents, read screen/transcript/files (read-only),
// list preview folders (read-only, opt-in), submit a prompt, answer an approval dialog, and interrupt. There is no
// endpoint that runs commands, writes files or starts agents.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"remote-herdr/internal/files"
	"remote-herdr/internal/herdr"
	"remote-herdr/internal/transcript"
)

// Herdr is the subset of the herdr client the server needs.
type Herdr interface {
	List(ctx context.Context) ([]herdr.Agent, error)
	Read(ctx context.Context, pane, source string, lines int) (string, error)
	Prompt(ctx context.Context, pane, text string) error
	SendKeys(ctx context.Context, pane string, keys ...string) error
}

// Config configures the server.
type Config struct {
	Herdr       Herdr
	ProjectsDir string // ~/.claude/projects
	SessionsDir string // ~/.claude/sessions; empty disables background session lookup
	// CodexSessionsDir is where Codex keeps its session logs
	// (~/.codex/sessions). Empty disables Codex transcripts.
	CodexSessionsDir string
	AllowedUsers     map[string]bool // Tailscale logins allowed to use the UI
	Origin           string          // public origin, e.g. https://host.tailnet.ts.net:8450
	AllowedSlash     map[string]bool // slash commands allowed in prompts
	// AllowShell lets prompts start with "!", the shell mode of both Claude
	// Code and Codex.
	// Off by default: anyone who can reach the UI as an allowed user would
	// otherwise get a shell on this machine.
	AllowShell   bool
	Static       fs.FS // built web UI
	Logger       *log.Logger
	PollInterval time.Duration
	// AccessLog logs every request (method, path, status, fetch metadata,
	// user agent) for debugging client problems. Prompt bodies are never logged.
	AccessLog bool
	// HostName identifies this machine in API responses. A future hub that
	// aggregates several hosts keys agents by (host, pane).
	HostName string
	// PreviewDir is the folder that holds published previews as
	// <project>/<name>/ (e.g. ~/previews). Empty disables the preview API.
	PreviewDir string
	// PreviewOrigin is where PreviewDir is served (another origin, e.g.
	// https://host.tailnet.ts.net:8460). Required with PreviewDir.
	PreviewOrigin string
	// PreviewAppPorts are ports on PreviewOrigin's host used for running
	// apps; the UI shows whether each has a listener.
	PreviewAppPorts []int
}

// Agent kinds as herdr reports them.
const (
	agentClaude = "claude"
	agentCodex  = "codex"
)

// Server implements http.Handler.
type Server struct {
	cfg        Config
	originHost string
	mux        *http.ServeMux
	mcache     mentionCache
	csp        string
}

const (
	userHeader     = "Tailscale-User-Login"
	csrfHeader     = "X-Remote-Herdr"
	maxBodyBytes   = 64 << 10
	transcriptMax  = 400
	screenMaxLines = 2000
)

// appCSP is the app's policy; frameSrc is "'none'" unless previews are
// enabled, then only the preview origins may be framed.
func appCSP(frameSrc string) string {
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
		"frame-src " + frameSrc + "; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; manifest-src 'self'"
}

// fileCSP applies to files served from an agent's working directory. The
// sandbox keeps scripts in SVG or PDF from running with the app's origin.
const fileCSP = "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; frame-ancestors 'self'"

// New builds the server. It returns an error for unsafe configuration.
func New(cfg Config) (*Server, error) {
	if cfg.Herdr == nil {
		return nil, errors.New("herdr client is required")
	}
	if len(cfg.AllowedUsers) == 0 {
		return nil, errors.New("at least one allowed user is required")
	}
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("origin must look like https://host[:port], got %q", cfg.Origin)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1500 * time.Millisecond
	}
	if cfg.HostName == "" {
		cfg.HostName, _ = os.Hostname()
	}
	if cfg.PreviewDir != "" || cfg.PreviewOrigin != "" {
		if cfg.PreviewDir == "" || cfg.PreviewOrigin == "" {
			return nil, errors.New("preview dir and preview origin must be set together")
		}
		pu, err := url.Parse(cfg.PreviewOrigin)
		if err != nil || pu.Scheme != "https" || pu.Host == "" || (pu.Path != "" && pu.Path != "/") ||
			pu.RawQuery != "" || pu.Fragment != "" || pu.User != nil {
			return nil, fmt.Errorf("preview origin must look like https://host[:port], got %q", cfg.PreviewOrigin)
		}
		if cfg.PreviewOrigin == cfg.Origin || pu.Host == u.Host {
			// Same origin would let preview scripts call this API.
			return nil, errors.New("preview origin must differ from origin")
		}
		cfg.PreviewOrigin = strings.TrimSuffix(cfg.PreviewOrigin, "/")
		for _, p := range cfg.PreviewAppPorts {
			if p <= 0 || p > 65535 {
				return nil, fmt.Errorf("invalid preview app port %d", p)
			}
			if pu.Scheme+"://"+net.JoinHostPort(pu.Hostname(), strconv.Itoa(p)) == cfg.Origin ||
				(pu.Hostname() == u.Hostname() && strconv.Itoa(p) == u.Port()) {
				// A framed app on this origin could call this API.
				return nil, fmt.Errorf("preview app port %d is the app's own origin", p)
			}
		}
	} else if len(cfg.PreviewAppPorts) > 0 {
		return nil, errors.New("preview app ports need preview dir and origin")
	}
	if cfg.AllowedSlash == nil {
		cfg.AllowedSlash = map[string]bool{}
	}
	s := &Server{cfg: cfg, originHost: u.Host, mux: http.NewServeMux(), csp: appCSP("'none'")}
	if s.previewsEnabled() {
		s.csp = appCSP(strings.Join(s.previewFrameOrigins(), " "))
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/me", s.handleMe)
	s.mux.HandleFunc("GET /api/agents", s.handleAgents)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/agents/{pane}/screen", s.withAgent(s.handleScreen))
	s.mux.HandleFunc("GET /api/agents/{pane}/transcript", s.withAgent(s.handleTranscript))
	s.mux.HandleFunc("GET /api/agents/{pane}/files", s.withAgent(s.handleList))
	s.mux.HandleFunc("GET /api/agents/{pane}/file", s.withAgent(s.handleFile))
	s.mux.HandleFunc("POST /api/agents/{pane}/prompt", s.withAgent(s.handlePrompt))
	s.mux.HandleFunc("POST /api/agents/{pane}/decision", s.withAgent(s.handleDecision))
	s.mux.HandleFunc("GET /api/agents/{pane}/dialog", s.withAgent(s.handleDialog))
	s.mux.HandleFunc("POST /api/agents/{pane}/choose", s.withAgent(s.handleChoose))
	s.mux.HandleFunc("POST /api/agents/{pane}/interrupt", s.withAgent(s.handleInterrupt))
	if s.previewsEnabled() {
		s.mux.HandleFunc("GET /api/previews", s.handlePreviews)
		s.mux.HandleFunc("GET /api/agents/{pane}/previews", s.withAgent(s.handleAgentPreviews))
	}
	if s.cfg.Static != nil {
		s.mux.Handle("GET /", http.FileServerFS(s.cfg.Static))
	}
}

// ServeHTTP applies the security checks to every request before routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AccessLog {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			s.cfg.Logger.Printf("access %s %s %d %s host=%q user=%q site=%q mode=%q dest=%q ua=%q",
				r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), r.Host,
				r.Header.Get(userHeader), r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Sec-Fetch-Mode"),
				r.Header.Get("Sec-Fetch-Dest"), r.UserAgent())
		}()
		w = rec
	}
	h := w.Header()
	h.Set("Content-Security-Policy", s.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")

	// Host: the public name, or "localhost" when tailscale serve proxies to a unix socket.
	host := r.Host
	if host != s.originHost && host != "localhost" {
		httpError(w, http.StatusMisdirectedRequest, "unexpected host")
		return
	}
	user := r.Header.Get(userHeader)
	if !s.cfg.AllowedUsers[user] {
		httpError(w, http.StatusForbidden, "not allowed")
		return
	}
	if !fetchSiteAllowed(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("Origin") != s.cfg.Origin || r.Header.Get(csrfHeader) != "1" ||
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			httpError(w, http.StatusForbidden, "csrf check failed")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
	}
	s.mux.ServeHTTP(w, r)
}

// fetchSiteAllowed refuses cross-site subresource requests. Top-level
// navigations are allowed so links and home-screen launches work.
func fetchSiteAllowed(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	switch site {
	case "", "same-origin", "none":
		return true
	}
	return r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		!strings.HasPrefix(r.URL.Path, "/api/")
}

func (s *Server) audit(r *http.Request, action, pane, detail string) {
	s.cfg.Logger.Printf("audit user=%s action=%s pane=%s detail=%q", r.Header.Get(userHeader), action, pane, detail)
}

type agentHandler func(w http.ResponseWriter, r *http.Request, a herdr.Agent)

// withAgent resolves {pane} against the live agent list.
func (s *Server) withAgent(next agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pane := r.PathValue("pane")
		if !ValidPaneID(pane) {
			httpError(w, http.StatusBadRequest, "invalid pane id")
			return
		}
		agents, err := s.listAgents(r.Context())
		if err != nil {
			s.herdrError(w, err)
			return
		}
		for _, a := range agents {
			if a.PaneID == pane {
				next(w, r, a)
				return
			}
		}
		httpError(w, http.StatusNotFound, "agent not found")
	}
}

// listAgents lists local herdr agents stamped with this host's name.
func (s *Server) listAgents(ctx context.Context) ([]herdr.Agent, error) {
	agents, err := s.cfg.Herdr.List(ctx)
	var bg []transcript.Background
	loaded := false
	for i := range agents {
		agents[i].Host = s.cfg.HostName
		a := &agents[i]
		if a.SessionID != "" || a.Agent != agentClaude || s.cfg.SessionsDir == "" {
			continue
		}
		// Forked and background sessions run outside the pane, so herdr
		// reports no session id. Match them by title instead.
		if !loaded {
			bg, loaded = transcript.LiveBackground(s.cfg.SessionsDir), true
		}
		a.SessionID = transcript.MatchBackground(bg, a.Title, a.Cwd)
	}
	return agents, err
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	writeJSON(w, map[string]any{
		"user":       r.Header.Get(userHeader),
		"host":       s.cfg.HostName,
		"home":       home,
		"allowShell": s.cfg.AllowShell,
		"previews":   s.previewsEnabled(),
	})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.listAgents(r.Context())
	if err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"agents": agents})
}

func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	source := r.URL.Query().Get("source")
	switch source {
	case "":
		source = "visible"
	case "visible", "recent", "recent_unwrapped":
	default:
		httpError(w, http.StatusBadRequest, "invalid source")
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines < 0 || lines > screenMaxLines {
		lines = screenMaxLines
	}
	text, err := s.cfg.Herdr.Read(r.Context(), a.PaneID, source, lines)
	if err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"text": text, "status": a.Status})
}

// transcriptOf locates the log of the agent's session and the loader for its
// format: Claude Code logs under ProjectsDir, Codex logs under
// CodexSessionsDir. Panes without a session id, other agents and missing logs
// give transcript.ErrNotFound.
func (s *Server) transcriptOf(a herdr.Agent) (string, transcript.Loader, error) {
	if a.SessionID == "" {
		return "", nil, transcript.ErrNotFound
	}
	switch a.Agent {
	case agentCodex:
		if s.cfg.CodexSessionsDir == "" {
			return "", nil, transcript.ErrNotFound
		}
		p, err := transcript.LocateCodex(s.cfg.CodexSessionsDir, a.SessionID)
		return p, transcript.LoadCodex, err
	case agentClaude, "":
		if s.cfg.ProjectsDir == "" {
			return "", nil, transcript.ErrNotFound
		}
		p, err := transcript.Locate(s.cfg.ProjectsDir, a.SessionID)
		return p, transcript.Load, err
	}
	return "", nil, transcript.ErrNotFound
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	path, load, err := s.transcriptOf(a)
	if err != nil {
		if errors.Is(err, transcript.ErrNotFound) {
			writeJSON(w, map[string]any{"messages": []any{}, "size": -1})
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > transcriptMax {
		limit = transcriptMax
	}
	msgs, size, err := load(path, limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot read transcript")
		return
	}
	writeJSON(w, map[string]any{"messages": msgs, "size": size})
}

func (s *Server) resolvePath(w http.ResponseWriter, a herdr.Agent, rel string) (string, bool) {
	p, err := files.Resolve(a.Cwd, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpError(w, http.StatusNotFound, "not found")
		} else {
			httpError(w, http.StatusForbidden, "path not allowed")
		}
		return "", false
	}
	return p, true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	p, ok := s.resolvePath(w, a, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	entries, err := files.List(p)
	if err != nil {
		httpError(w, http.StatusBadRequest, "not a directory")
		return
	}
	writeJSON(w, map[string]any{"entries": entries})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	p, ok := s.resolvePath(w, a, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	c, err := files.Open(p)
	if err != nil {
		httpError(w, http.StatusUnsupportedMediaType, "file cannot be shown")
		return
	}
	defer c.File.Close()
	h := w.Header()
	h.Set("Content-Type", c.ContentType)
	h.Set("Content-Security-Policy", fileCSP)
	h.Set("Content-Length", strconv.FormatInt(c.Size, 10))
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Disposition", "inline")
	_, _ = io.Copy(w, c.File)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := ValidatePrompt(body.Text, s.cfg.AllowedSlash, s.cfg.AllowShell); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if a.Status == "blocked" {
		httpError(w, http.StatusConflict, "agent is waiting for an approval")
		return
	}
	kind := "prompt"
	if strings.HasPrefix(strings.TrimSpace(body.Text), "!") {
		kind = "shell"
	}
	s.audit(r, kind, a.PaneID, fmt.Sprintf("%d chars", len([]rune(body.Text))))
	if err := s.cfg.Herdr.Prompt(r.Context(), a.PaneID, body.Text); err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	var body struct {
		Decision string `json:"decision"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if a.Status != "blocked" {
		httpError(w, http.StatusConflict, "agent is not waiting for an approval")
		return
	}
	var key string
	switch body.Decision {
	case "approve":
		screen, err := s.cfg.Herdr.Read(r.Context(), a.PaneID, "visible", 0)
		if err != nil {
			s.herdrError(w, err)
			return
		}
		if !looksLikeApprovalFor(a.Agent, screen) {
			httpError(w, http.StatusConflict, "unrecognised dialog; answer it in the terminal")
			return
		}
		key = "enter"
	case "deny":
		key = "esc"
	default:
		httpError(w, http.StatusBadRequest, "decision must be approve or deny")
		return
	}
	s.audit(r, "decision", a.PaneID, body.Decision)
	if err := s.cfg.Herdr.SendKeys(r.Context(), a.PaneID, key); err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleDialog returns the selection dialog visible in a blocked agent.
func (s *Server) handleDialog(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	if a.Status != "blocked" {
		writeJSON(w, map[string]any{"dialog": nil})
		return
	}
	screen, err := s.cfg.Herdr.Read(r.Context(), a.PaneID, "visible", 0)
	if err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"dialog": parseDialogFor(a.Agent, screen)})
}

// handleChoose selects one item of the visible dialog. The client sends the
// index and label it saw; if the screen has changed since, nothing is sent.
func (s *Server) handleChoose(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	var body struct {
		Index int    `json:"index"`
		Label string `json:"label"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if a.Status != "blocked" {
		httpError(w, http.StatusConflict, "agent is not waiting for an answer")
		return
	}
	screen, err := s.cfg.Herdr.Read(r.Context(), a.PaneID, "visible", 0)
	if err != nil {
		s.herdrError(w, err)
		return
	}
	d := parseDialogFor(a.Agent, screen)
	if d == nil || d.Cursor < 0 || body.Index < 0 || body.Index >= len(d.Items) {
		httpError(w, http.StatusConflict, "dialog changed; reload")
		return
	}
	item := d.Items[body.Index]
	if item.Label != body.Label {
		httpError(w, http.StatusConflict, "dialog changed; reload")
		return
	}
	if item.FreeText {
		httpError(w, http.StatusConflict, "free-text answers must be typed in the terminal")
		return
	}
	s.audit(r, "choose", a.PaneID, item.Number+" "+item.Label)
	if err := s.cfg.Herdr.SendKeys(r.Context(), a.PaneID, d.KeysToChoose(body.Index)...); err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request, a herdr.Agent) {
	if a.Status != "working" {
		httpError(w, http.StatusConflict, "agent is not working")
		return
	}
	s.audit(r, "interrupt", a.PaneID, "")
	if err := s.cfg.Herdr.SendKeys(r.Context(), a.PaneID, "esc"); err != nil {
		s.herdrError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleEvents streams agent state and transcript sizes (keyed "host/pane")
// over SSE so the UI knows when to refetch.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")

	paths := map[string]string{}
	last := ""
	send := func() bool {
		agents, err := s.listAgents(r.Context())
		if err != nil {
			_, err = fmt.Fprintf(w, "event: error\ndata: %q\n\n", "herdr unavailable")
			flusher.Flush()
			return err == nil
		}
		sizes := map[string]int64{}
		for _, a := range agents {
			if a.SessionID == "" {
				continue
			}
			key := a.Agent + "/" + a.SessionID
			p, ok := paths[key]
			if !ok {
				// Cache only found paths: a new session's log can appear
				// after herdr reports its id, so misses are retried on the
				// next tick.
				if found, _, err := s.transcriptOf(a); err == nil {
					p = found
					paths[key] = p
				}
			}
			if p != "" {
				sizes[a.Host+"/"+a.PaneID] = transcript.Size(p)
			}
		}
		b, _ := json.Marshal(map[string]any{"agents": agents, "sizes": sizes})
		if string(b) == last {
			return true
		}
		last = string(b)
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) herdrError(w http.ResponseWriter, err error) {
	var he *herdr.Error
	if errors.As(err, &he) {
		status := http.StatusBadGateway
		switch he.Code {
		case "agent_not_found":
			status = http.StatusNotFound
		case "agent_blocked":
			status = http.StatusConflict
		}
		httpError(w, status, he.Code)
		return
	}
	s.cfg.Logger.Printf("herdr error: %v", err)
	httpError(w, http.StatusBadGateway, "herdr unavailable")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// statusRecorder captures the status code for the access log and keeps
// streaming (SSE) working by forwarding Flush.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
