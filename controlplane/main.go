package main

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"os/signal"
	"syscall"
)

//go:embed web/*.html
var webFS embed.FS

// Server is the control plane: auth + provisioning + activating proxy + reaper.
type Server struct {
	cfg   Config
	k8s   *K8s
	tmpl  *template.Template
	ps    *proxyState
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[controlplane] ")

	cfg := loadConfig()
	k8s, err := newK8s(cfg)
	if err != nil {
		log.Fatalf("kubernetes init: %v", err)
	}
	tmpl, err := template.ParseFS(webFS, "web/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	s := &Server{cfg: cfg, k8s: k8s, tmpl: tmpl, ps: newProxyState()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.runReaper(ctx)
	go s.runCronScheduler(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/signup", s.handleSignup)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc(seedPath, s.handleSeedJS)      // same-origin token seed (CSP-safe)
	mux.HandleFunc("/__oc-ready", s.handleReadyCheck) // wake interstitial readiness poll
	mux.HandleFunc("/sw.js", s.handleNoopSW)      // neuter OpenClaw's caching service worker
	mux.HandleFunc("/", s.handleApp)         // catch-all: authenticated reverse proxy

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("listening on %s (public origin %s, users ns %s)", cfg.ListenAddr, cfg.PublicOrigin, cfg.UsersNS)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

// renderPage renders an embedded template with the given data.
func (s *Server) renderPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}
