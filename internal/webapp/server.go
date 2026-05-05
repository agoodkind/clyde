// Package webapp implements the optional remote dashboard mounted by
// the clyde daemon. The dashboard renders a single HTML page for
// provider-neutral live sessions. Authentication is a static bearer
// token so the listener can sit behind a Cloudflare tunnel without
// exposing a public surface unguarded.
package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
)

// DefaultPort is the loopback port the dashboard listens on when no
// port is configured. The choice of 11435 avoids the OpenAI adapter
// default at 11434.
const DefaultPort = 11435

// DefaultHost is the loopback bind. The dashboard never binds a
// public interface unless the user explicitly sets WebAppConfig.Host.
const DefaultHost = "::1"

// LiveSession is the daemon side view of one provider-neutral live
// session that can be displayed and driven by the browser chat harness.
type LiveSession struct {
	Provider       string    `json:"provider"`
	SessionName    string    `json:"session_name"`
	SessionID      string    `json:"session_id"`
	Status         string    `json:"status"`
	Basedir        string    `json:"basedir,omitempty"`
	URL            string    `json:"url,omitempty"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	SupportsSend   bool      `json:"supports_send"`
	SupportsStream bool      `json:"supports_stream"`
	SupportsStop   bool      `json:"supports_stop"`
}

// LiveSessionEvent is one provider-neutral stream event rendered by
// the browser harness and the TUI sidecar.
type LiveSessionEvent struct {
	SessionID string    `json:"session_id"`
	Kind      string    `json:"kind"`
	Role      string    `json:"role,omitempty"`
	Text      string    `json:"text,omitempty"`
	Timestamp time.Time `json:"timestamp,omitzero"`
}

// StartLiveSessionRequest describes a provider-neutral live-session
// launch. Provider may be empty to ask the daemon to use its default.
type StartLiveSessionRequest struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Basedir   string `json:"basedir"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Incognito bool   `json:"incognito"`
}

// SendLiveSessionRequest is the browser chat input payload.
type SendLiveSessionRequest struct {
	Text string `json:"text"`
}

// Deps wires the daemon helpers the webapp needs.
type Deps struct {
	ListLiveSessions  func(ctx context.Context) ([]LiveSession, error)
	StartLiveSession  func(ctx context.Context, req StartLiveSessionRequest) (LiveSession, error)
	SendLiveSession   func(ctx context.Context, sessionID, text string) error
	StreamLiveSession func(ctx context.Context, sessionID string) (<-chan LiveSessionEvent, error)
	StopLiveSession   func(ctx context.Context, sessionID string) error
}

// Server is the HTTP facade for the dashboard.
type Server struct {
	cfg   config.WebAppConfig
	deps  Deps
	log   *slog.Logger
	token string
	mux   *http.ServeMux
	srv   *http.Server

	mu       sync.Mutex
	starting []startedSession
	connMu   sync.Mutex
	conns    map[net.Conn]http.ConnState
}

type startedSession struct {
	Name      string
	StartedAt time.Time
	Cmd       string
	Note      string
}

// New builds a Server from the given config plus deps.
func New(cfg config.WebAppConfig, deps Deps, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	token := cfg.RequireToken
	if v := os.Getenv("CLYDE_WEBAPP_TOKEN"); v != "" {
		token = v
	}
	s := &Server{
		cfg:      cfg,
		deps:     deps,
		log:      log.With("component", "webapp"),
		token:    token,
		mux:      nil,
		srv:      nil,
		mu:       sync.Mutex{},
		starting: nil,
		connMu:   sync.Mutex{},
		conns:    nil,
	}
	s.mux = s.routes()
	return s
}

// Addr returns the host:port pair the server will bind.
func (s *Server) Addr() string {
	host := s.cfg.Host
	if host == "" {
		host = DefaultHost
	}
	port := s.cfg.Port
	if port <= 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(normalizeListenHost(host), strconv.Itoa(port))
}

func normalizeListenHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.Contains(inner, ":") {
			return inner
		}
	}
	return trimmed
}

// StartOnListener serves the dashboard on an already-bound listener.
// Daemon reload uses this to inherit the existing dashboard socket
// without creating a bind gap.
func (s *Server) StartOnListener(ctx context.Context, lis net.Listener) error {
	s.connMu.Lock()
	s.conns = make(map[net.Conn]http.ConnState)
	s.connMu.Unlock()
	srv := &http.Server{
		Addr:              lis.Addr().String(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         s.trackConnState,
	}
	s.srv = srv
	s.log.InfoContext(ctx, "webapp listening", "addr", lis.Addr().String())
	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.ErrorContext(ctx, "webapp.serve_panic",
					"addr", lis.Addr().String(),
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		errCh <- srv.Serve(lis)
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// Shutdown stops accepting new dashboard requests, closes idle
// keepalive connections, and lets active handlers finish until ctx
// expires.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	s.srv.SetKeepAlivesEnabled(false)
	s.closeTrackedConns(http.StateIdle)
	if err := s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown webapp server: %w", err)
	}
	return nil
}

// Close force-closes all dashboard HTTP connections. Reload uses this
// after a bounded drain so held keepalive connections cannot keep the
// old daemon generation reachable indefinitely.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	s.closeTrackedConns(http.StateNew, http.StateActive, http.StateIdle, http.StateHijacked)
	if err := s.srv.Close(); err != nil {
		return fmt.Errorf("close webapp server: %w", err)
	}
	return nil
}

func (s *Server) trackConnState(conn net.Conn, state http.ConnState) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conns == nil {
		s.conns = make(map[net.Conn]http.ConnState)
	}
	if state == http.StateClosed {
		delete(s.conns, conn)
		return
	}
	s.conns[conn] = state
}

func (s *Server) closeTrackedConns(states ...http.ConnState) {
	if len(states) == 0 {
		return
	}
	wanted := make(map[http.ConnState]bool, len(states))
	for _, state := range states {
		wanted[state] = true
	}
	var toClose []net.Conn
	s.connMu.Lock()
	for conn, state := range s.conns {
		if wanted[state] {
			toClose = append(toClose, conn)
		}
	}
	s.connMu.Unlock()
	for _, conn := range toClose {
		_ = conn.Close()
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.handleIndex))
	mux.HandleFunc("/api/live-sessions", s.auth(s.handleLiveSessions))
	mux.HandleFunc("/api/live-sessions/", s.auth(s.handleLiveSessionByID))
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

func (*Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (*Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListLiveSessions(w, r)
	case http.MethodPost:
		s.handleStartLiveSession(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListLiveSessions(w http.ResponseWriter, r *http.Request) {
	if s.deps.ListLiveSessions == nil {
		writeJSON(w, http.StatusOK, liveSessionsResponse{Sessions: []LiveSession{}})
		return
	}
	sessions, err := s.deps.ListLiveSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, liveSessionsResponse{Sessions: sessions})
}

func (s *Server) handleStartLiveSession(w http.ResponseWriter, r *http.Request) {
	var body StartLiveSessionRequest
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.start_decode_failed",
			"component", "webapp",
			"err", err,
		)
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.StartLiveSession == nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.start_unavailable",
			"component", "webapp",
			"err", "live session launch unavailable",
		)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "live session launch unavailable"})
		return
	}
	live, err := s.deps.StartLiveSession(r.Context(), body)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.start_failed",
			"component", "webapp",
			"provider", body.Provider,
			"model", body.Model,
			"effort", body.Effort,
			"err", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	s.rememberStarting(live.SessionName, "daemon StartLiveSession", "spawned, waiting for stream")
	writeJSON(w, http.StatusAccepted, startLiveSessionResponse{
		Session: live,
		Note:    "session is starting; open the stream endpoint for events",
	})
}

func (s *Server) handleLiveSessionByID(w http.ResponseWriter, r *http.Request) {
	sessionID, action, ok := liveSessionPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "send":
		s.handleSendLiveSession(w, r, sessionID)
	case "stream":
		s.handleStreamLiveSession(w, r, sessionID)
	case "stop":
		s.handleStopLiveSession(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleSendLiveSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body SendLiveSessionRequest
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.send_decode_failed",
			"component", "webapp",
			"session_id", sessionID,
			"err", err,
		)
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.SendLiveSession == nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.send_unavailable",
			"component", "webapp",
			"session_id", sessionID,
			"err", "live session send unavailable",
		)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "live session send unavailable"})
		return
	}
	err = s.deps.SendLiveSession(r.Context(), sessionID, body.Text)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.send_failed",
			"component", "webapp",
			"session_id", sessionID,
			"err", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, sendLiveSessionResponse{Accepted: true})
}

func (s *Server) handleStreamLiveSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.StreamLiveSession == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "live session stream unavailable"})
		return
	}
	events, err := s.deps.StreamLiveSession(r.Context(), sessionID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.stream_failed",
			"component", "webapp",
			"session_id", sessionID,
			"err", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	encoder := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("event: live-session\n"))
			_, _ = w.Write([]byte("data: "))
			if err := encoder.Encode(ev); err != nil {
				s.log.ErrorContext(r.Context(), "webapp.live_session.stream_encode_failed",
					"component", "webapp",
					"session_id", sessionID,
					"err", err,
				)
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleStopLiveSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.StopLiveSession == nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.stop_unavailable",
			"component", "webapp",
			"session_id", sessionID,
			"err", "live session stop unavailable",
		)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "live session stop unavailable"})
		return
	}
	err := s.deps.StopLiveSession(r.Context(), sessionID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "webapp.live_session.stop_failed",
			"component", "webapp",
			"session_id", sessionID,
			"err", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, stopLiveSessionResponse{Stopped: true})
}

func (s *Server) rememberStarting(name, cmd, note string) {
	s.mu.Lock()
	s.starting = append(s.starting, startedSession{
		Name:      name,
		StartedAt: currentTime(),
		Cmd:       cmd,
		Note:      note,
	})
	if len(s.starting) > 50 {
		s.starting = s.starting[len(s.starting)-50:]
	}
	s.mu.Unlock()
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}
		want := "Bearer " + s.token
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func liveSessionPath(path string) (sessionID string, action string, ok bool) {
	const prefix = "/api/live-sessions/"
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := path[len(prefix):]
	slash := -1
	for i, r := range rest {
		if r == '/' {
			slash = i
			break
		}
	}
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

type healthResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type liveSessionsResponse struct {
	Sessions []LiveSession `json:"sessions"`
}

type startLiveSessionResponse struct {
	Session LiveSession `json:"session"`
	Note    string      `json:"note"`
}

type sendLiveSessionResponse struct {
	Accepted bool `json:"accepted"`
}

type stopLiveSessionResponse struct {
	Stopped bool `json:"stopped"`
}

func writeJSON[T healthResponse | errorResponse | liveSessionsResponse | startLiveSessionResponse | sendLiveSessionResponse | stopLiveSessionResponse](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// indexHTML is the dashboard's single page. It renders a list of
// live sessions and a form for spawning a provider-neutral chat.
const indexHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>clyde remote</title>
<style>
body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#0f1115;color:#d8dde6;margin:0;padding:24px;}
h1,h2{font-weight:600;color:#e9eef7;}
a{color:#7cb7ff;}
form,table,.chat{background:#161a22;border:1px solid #232936;border-radius:8px;padding:16px;margin:0 0 24px;}
table{width:100%;border-collapse:collapse;}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid #232936;font-size:13px;}
th{color:#9aa6b8;font-weight:500;}
input,select,button{background:#0f1115;color:#e9eef7;border:1px solid #2c3344;border-radius:6px;padding:8px 10px;margin:4px 6px 4px 0;}
button{cursor:pointer;background:#3b82f6;border-color:#3b82f6;}
button:hover{background:#5fa0ff;}
.chatlog{height:300px;overflow:auto;background:#0f1115;border:1px solid #232936;border-radius:6px;padding:10px;margin-bottom:8px;white-space:pre-wrap;}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap;}
.pill{color:#9aa6b8;border:1px solid #2c3344;border-radius:999px;padding:2px 8px;font-size:12px;}
.empty{color:#9aa6b8;font-style:italic;}
.note{color:#9aa6b8;font-size:12px;margin-top:6px;}
</style></head><body>
<h1>clyde remote</h1>
<h2>Start a live session</h2>
<form id="newform">
  <input name="provider" placeholder="provider (optional)">
  <input name="name" placeholder="session name (optional)">
  <input name="basedir" placeholder="basedir (optional, defaults to home)">
  <select name="model">
    <option value="">model: default</option>
    <option value="claude-4-7-high">claude 4 7 high</option>
    <option value="sonnet">sonnet</option>
    <option value="haiku">haiku</option>
  </select>
  <select name="effort">
    <option value="">effort: default</option>
    <option value="low">low</option>
    <option value="medium">medium</option>
    <option value="high">high</option>
  </select>
  <button type="submit">Start</button>
  <div class="note">Starts a daemon-owned live session. Provider, model, and effort are daemon policy inputs, not browser-side behavior switches.</div>
</form>

<h2 id="pending-h2" style="display:none">Spawning</h2>
<div id="pending"></div>

<h2>Live sessions</h2>
<table id="live"><thead>
<tr><th>session</th><th>provider</th><th>status</th><th>actions</th></tr>
</thead><tbody><tr><td colspan="4" class="empty">loading...</td></tr></tbody></table>

<div class="chat" id="chat" style="display:none">
  <div class="row"><strong id="chat-title">session</strong><span class="pill" id="chat-status"></span></div>
  <div class="chatlog" id="chatlog"></div>
  <form id="sendform" class="row" style="border:0;padding:0;margin:0">
    <input name="text" style="flex:1;min-width:260px" placeholder="message">
    <button type="submit">Send</button>
    <button type="button" id="stopbtn">Stop</button>
  </form>
</div>

<script>
const pending = []; // {name, startedAt, attempts, cmd}
let lastLive = [];
let activeSession = '';
let source = null;

async function refresh(){
  await refreshLive();
  renderPending();
}

async function refreshLive(){
  try{
    const r = await fetch('/api/live-sessions');
    if(!r.ok){throw new Error(r.status);}
    const j = await r.json();
    lastLive = j.sessions || [];
    const tb = document.querySelector('#live tbody');
    if(!lastLive.length){tb.innerHTML='<tr><td colspan="4" class="empty">no live sessions</td></tr>';}
    else{
      tb.innerHTML = lastLive.map(s=>` + "`" + `<tr><td>${escape(s.session_name||s.session_id)}</td><td>${escape(s.provider)}</td><td>${escape(s.status)}</td><td><button data-open="${escape(s.session_id)}">Open</button></td></tr>` + "`" + `).join('');
      tb.querySelectorAll('button[data-open]').forEach(btn=>btn.addEventListener('click',()=>openChat(btn.dataset.open)));
    }
    settlePending();
  }catch(e){
    document.querySelector('#live tbody').innerHTML = '<tr><td colspan="4" class="empty">error: '+e+'</td></tr>';
  }
}

function settlePending(){
  for(let i=pending.length-1;i>=0;i--){
    const p = pending[i];
    p.attempts++;
    const matchedLive = lastLive.find(s=>s.session_name===p.name || s.session_id===p.sessionID);
    if(matchedLive){pending.splice(i,1);}
  }
}

function renderPending(){
  const h2 = document.getElementById('pending-h2');
  const div = document.getElementById('pending');
  if(!pending.length){h2.style.display='none'; div.innerHTML=''; return;}
  h2.style.display='';
  div.innerHTML = pending.map(p=>{
    const age = Math.round((Date.now()-p.startedAt)/1000);
    return ` + "`" + `<div class="note" style="background:#161a22;border:1px solid #232936;border-radius:6px;padding:8px 12px;margin:6px 0">⏳ ${escape(p.name||'(auto-named)')}: probe ${p.attempts}, ${age}s elapsed<br><code style="font-size:11px">${escape(p.cmd||'')}</code></div>` + "`" + `;
  }).join('');
}

function escape(s){return (s||'').replace(/[&<>"']/g, c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));}

document.getElementById('newform').addEventListener('submit', async ev=>{
  ev.preventDefault();
  const fd = new FormData(ev.target);
  const body = Object.fromEntries(fd.entries());
  const r = await fetch('/api/live-sessions', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)});
  const j = await r.json().catch(()=>({}));
  if(!r.ok){alert('Failed: '+JSON.stringify(j)); return;}
  pending.push({name: (j.session&&j.session.session_name)||body.name||'', sessionID: (j.session&&j.session.session_id)||'', startedAt: Date.now(), attempts: 0, cmd: 'daemon StartLiveSession'});
  renderPending();
  if(j.session&&j.session.session_id){openChat(j.session.session_id);}
  setTimeout(refresh, 1500);
});

function openChat(id){
  activeSession = id;
  const s = lastLive.find(x=>x.session_id===id) || {session_name:id,status:'opening'};
  document.getElementById('chat').style.display='';
  document.getElementById('chat-title').textContent = s.session_name || id;
  document.getElementById('chat-status').textContent = s.status || '';
  document.getElementById('chatlog').textContent = '';
  if(source){source.close();}
  source = new EventSource('/api/live-sessions/'+encodeURIComponent(id)+'/stream');
  source.addEventListener('live-session', ev=>{
    const msg = JSON.parse(ev.data);
    const prefix = msg.role ? msg.role+': ' : '';
    document.getElementById('chatlog').textContent += prefix + (msg.text||msg.kind||'') + '\n';
    document.getElementById('chatlog').scrollTop = document.getElementById('chatlog').scrollHeight;
  });
  source.onerror = ()=>{document.getElementById('chat-status').textContent='stream unavailable';};
}

document.getElementById('sendform').addEventListener('submit', async ev=>{
  ev.preventDefault();
  if(!activeSession){return;}
  const input = ev.target.elements.text;
  const text = input.value.trim();
  if(!text){return;}
  input.value = '';
  const r = await fetch('/api/live-sessions/'+encodeURIComponent(activeSession)+'/send', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({text})});
  if(!r.ok){alert('Send failed: '+await r.text());}
});

document.getElementById('stopbtn').addEventListener('click', async ()=>{
  if(!activeSession){return;}
  const r = await fetch('/api/live-sessions/'+encodeURIComponent(activeSession)+'/stop', {method:'POST'});
  if(!r.ok){alert('Stop failed: '+await r.text());}
});

// Poll faster while sessions are pending so daemon live-session state
// appears quickly after spawn. Drop to a slower cadence once
// everything has settled to keep idle CPU low.
function tickerInterval(){return pending.length? 1500 : 4000;}
function loop(){refresh().then(()=>setTimeout(loop, tickerInterval()));}
loop();
</script>
</body></html>`
