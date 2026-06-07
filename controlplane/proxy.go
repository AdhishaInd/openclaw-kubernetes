package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// proxyState caches per-user reverse proxies, readiness, and activity throttling
// so the hot path doesn't hammer the API server.
type proxyState struct {
	mu        sync.Mutex
	proxies   map[string]*httputil.ReverseProxy
	readyAt   map[string]time.Time // last time the pod was observed ready
	activedAt map[string]time.Time // last time we stamped the activity annotation
}

func newProxyState() *proxyState {
	return &proxyState{
		proxies:   map[string]*httputil.ReverseProxy{},
		readyAt:   map[string]time.Time{},
		activedAt: map[string]time.Time{},
	}
}

// handleApp is the catch-all authenticated handler: it wakes the user's pod if
// needed and reverse-proxies (HTTP + WebSocket) with the gateway token injected.
func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	ready := s.cachedReady(r.Context(), id)
	if !ready {
		// Trigger wake; scaleTo is cheap and idempotent.
		if err := s.k8s.scaleTo(r.Context(), id, 1); err != nil {
			http.Error(w, "could not wake instance: "+err.Error(), http.StatusBadGateway)
			return
		}
		if isHTMLNav(r) {
			// Show an interstitial that polls readiness and reloads when ready.
			w.Header().Set("Retry-After", "3")
			w.Header().Set("Cache-Control", "no-store")
			s.renderPage(w, "spinup.html", nil)
			return
		}
		// Non-navigation (XHR/WS): hold until ready or time out.
		if !s.waitReady(r.Context(), id) {
			http.Error(w, "instance still starting", http.StatusServiceUnavailable)
			return
		}
	}

	s.touchActivity(r.Context(), id)
	s.proxyFor(r.Context(), id).ServeHTTP(w, r)
}

// proxyFor returns a cached reverse proxy for the user, building one (with the
// gateway token captured in the Director) on first use.
func (s *Server) proxyFor(ctx context.Context, id string) *httputil.ReverseProxy {
	s.ps.mu.Lock()
	if p := s.ps.proxies[id]; p != nil {
		s.ps.mu.Unlock()
		return p
	}
	s.ps.mu.Unlock()

	token, _ := s.gatewayToken(ctx, id)
	target := &url.URL{Scheme: "http", Host: s.k8s.serviceHost(id)}
	p := httputil.NewSingleHostReverseProxy(target)
	orig := p.Director
	p.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Ask upstream for uncompressed HTML so ModifyResponse can rewrite it.
		req.Header.Set("Accept-Encoding", "identity")
	}
	// The gateway authenticates the control WebSocket via an in-band challenge
	// that only the client can answer (using the gateway token). Since the proxy
	// can't transparently answer it, we seed the user's own token into the page's
	// localStorage so the client auto-connects without the user pasting anything.
	p.ModifyResponse = func(resp *http.Response) error {
		if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		body = injectSeedTag(body)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		// Never let the browser cache the seeded HTML, or a stale copy without the
		// token seed could be served on reload.
		resp.Header.Set("Cache-Control", "no-store, must-revalidate")
		resp.Header.Del("ETag")
		resp.Header.Del("Last-Modified")
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		log.Printf("proxy error user=%s: %v", id, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}

	s.ps.mu.Lock()
	s.ps.proxies[id] = p
	s.ps.mu.Unlock()
	return p
}

// cachedReady returns readiness with a short TTL cache to limit API calls.
func (s *Server) cachedReady(ctx context.Context, id string) bool {
	s.ps.mu.Lock()
	if t, ok := s.ps.readyAt[id]; ok && time.Since(t) < 2*time.Second {
		s.ps.mu.Unlock()
		return true
	}
	s.ps.mu.Unlock()

	ready, err := s.k8s.ready(ctx, id)
	if err != nil || !ready {
		return false
	}
	s.ps.mu.Lock()
	s.ps.readyAt[id] = time.Now()
	s.ps.mu.Unlock()
	return true
}

// wakeAndWait scales a user's pod to 1 and waits for it to become ready.
// Shared by the cron scheduler and the Telegram webhook receiver.
func (s *Server) wakeAndWait(ctx context.Context, id string) bool {
	if err := s.k8s.scaleTo(ctx, id, 1); err != nil {
		log.Printf("wake scale user=%s: %v", id, err)
		return false
	}
	return s.waitReady(ctx, id)
}

// waitReady polls until the pod is ready or the cold-start timeout elapses.
func (s *Server) waitReady(ctx context.Context, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ColdStartTimeout)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.cachedReady(ctx, id) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// touchActivity stamps the activity annotation, throttled to once per 30s/user.
func (s *Server) touchActivity(ctx context.Context, id string) {
	s.ps.mu.Lock()
	if t, ok := s.ps.activedAt[id]; ok && time.Since(t) < 30*time.Second {
		s.ps.mu.Unlock()
		return
	}
	s.ps.activedAt[id] = time.Now()
	s.ps.mu.Unlock()

	if err := s.k8s.touchActivity(ctx, id); err != nil {
		log.Printf("touch activity user=%s: %v", id, err)
	}
}

// seedPath is a same-origin script the control plane serves; the Control UI's
// CSP allows 'self' scripts, so this runs where an inline script would be blocked.
const seedPath = "/__oc-seed.js"

// injectSeedTag inserts a classic (non-deferred) external <script> reference so it
// runs before the app's deferred module bundle. The script itself (served at
// seedPath) writes the user's gateway token into localStorage.
func injectSeedTag(body []byte) []byte {
	script := []byte(`<script src="` + seedPath + `"></script>`)

	lower := bytes.ToLower(body)
	// Insert at the very start of <head> so it runs before any of the app's own
	// head scripts that might read settings.
	if i := bytes.Index(lower, []byte("<head")); i >= 0 {
		if j := bytes.IndexByte(body[i:], '>'); j >= 0 {
			pos := i + j + 1
			return append(body[:pos:pos], append(script, body[pos:]...)...)
		}
	}
	if i := bytes.Index(lower, []byte("<body")); i >= 0 {
		if j := bytes.IndexByte(body[i:], '>'); j >= 0 {
			pos := i + j + 1
			return append(body[:pos:pos], append(script, body[pos:]...)...)
		}
	}
	return append(script, body...)
}

// handleSeedJS serves a same-origin script that writes the authenticated user's
// gateway token into the Control UI's localStorage settings, so the client can
// answer the gateway's connect challenge. Served from the control-plane origin so
// it satisfies the gateway's `script-src 'self'` CSP (an inline script would be
// blocked). It patches ':default' plus every existing settings key so a stale
// entry (from a prior manual connect attempt) can't shadow the token.
func (s *Server) handleSeedJS(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Error(w, "// unauthenticated", http.StatusUnauthorized)
		return
	}
	token, err := s.gatewayToken(r.Context(), id)
	if err != nil {
		http.Error(w, "// no token", http.StatusInternalServerError)
		return
	}
	tok, _ := json.Marshal(token)
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Seed the token into: the exact per-gateway-URL key the UI reads first
	// (computed from location, matching the app's own derivation), ':default',
	// and every existing settings key.
	io.WriteString(w, `try{var P='openclaw.control.settings.v1';var T=`+string(tok)+
		`;function U(k){var s={};try{s=JSON.parse(localStorage.getItem(k))||{}}catch(e){}s.token=T;localStorage.setItem(k,JSON.stringify(s));}`+
		`for(var i=0;i<localStorage.length;i++){var k=localStorage.key(i);if(k&&k.indexOf(P)===0)U(k);}`+
		`U(P+':default');`+
		`try{U(P+':'+((location.protocol==='https:'?'wss':'ws')+'://'+location.host));}catch(e){}`+
		`}catch(e){}`)
}

// handleNoopSW replaces OpenClaw's caching service worker with a pass-through one.
// The real SW serves a cached index.html that lacks our injected seed script (and
// mangles proxied requests), which breaks token seeding. A no-op SW (no fetch
// handler) lets every request hit the network/proxy so the seed always runs. On
// activation it claims and reloads open windows once so the swap takes effect.
func (s *Server) handleNoopSW(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, `self.addEventListener('install',function(){self.skipWaiting()});`+
		`self.addEventListener('activate',function(e){e.waitUntil((async function(){`+
		`try{var ks=await caches.keys();await Promise.all(ks.map(function(k){return caches.delete(k)}));}catch(e){}`+
		`await self.clients.claim();`+
		`try{var cs=await self.clients.matchAll({type:'window'});cs.forEach(function(c){c.navigate(c.url)});}catch(e){}`+
		`})())});`)
}

// handleReadyCheck lets the wake interstitial poll whether the user's pod is
// ready, so it can reload exactly when the gateway is up (no fixed-timer guessing).
func (s *Server) handleReadyCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.cachedReady(r.Context(), id) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
		return
	}
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("waking"))
}

func isHTMLNav(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return false // websocket
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
