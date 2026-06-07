package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// handleChannelsPage serves the authenticated self-serve "Channels" page where a
// user connects their own Telegram bot.
func (s *Server) handleChannelsPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	connected := false
	if sec, err := s.k8s.getSecret(r.Context(), id); err == nil && sec != nil {
		connected = len(sec.Data["telegram-webhook-secret"]) > 0
	}
	s.renderPage(w, "channels.html", map[string]any{"Connected": connected})
}

var pairingCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{4,32}$`)

// handlePairingsList returns the tenant's pending Telegram DM pairing requests so
// the user can approve them from the /channels page.
func (s *Server) handlePairingsList(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ColdStartTimeout+30*time.Second)
	defer cancel()
	one := "1"
	s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: &one})
	defer s.k8s.setAnnotations(context.Background(), id, map[string]*string{annBusy: nil})
	if !s.wakeAndWait(ctx, id) {
		http.Error(w, "instance starting", http.StatusServiceUnavailable)
		return
	}
	s.touchActivity(ctx, id)
	out, err := s.k8s.execInGateway(ctx, id, "node", "openclaw.mjs", "pairing", "list", "--channel", "telegram", "--json")
	if err != nil {
		http.Error(w, "list failed", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Requests []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
			Meta struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
			} `json:"meta"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(jsonSlice(out)), &parsed); err != nil {
		http.Error(w, "parse failed", http.StatusBadGateway)
		return
	}
	type req struct{ Code, ID, Name string }
	reqs := []req{}
	for _, p := range parsed.Requests {
		name := strings.TrimSpace(p.Meta.FirstName + " " + p.Meta.LastName)
		reqs = append(reqs, req{Code: p.Code, ID: p.ID, Name: name})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"requests": reqs})
}

// handlePairingApprove approves a pending Telegram DM pairing code for the user.
func (s *Server) handlePairingApprove(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	_ = r.ParseForm()
	code := strings.TrimSpace(r.PostFormValue("code"))
	if !pairingCodeRe.MatchString(code) {
		http.Error(w, "invalid code", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ColdStartTimeout+30*time.Second)
	defer cancel()
	one := "1"
	s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: &one})
	defer s.k8s.setAnnotations(context.Background(), id, map[string]*string{annBusy: nil})
	if !s.wakeAndWait(ctx, id) {
		http.Error(w, "instance starting", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.k8s.execInGateway(ctx, id, "node", "openclaw.mjs", "pairing", "approve", "telegram", code, "--notify"); err != nil {
		http.Error(w, "approve failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("telegram pairing approved user=%s code=%s", id, code)
	fmt.Fprint(w, "approved")
}

// handleTelegramWebhook receives Telegram updates at POST /tg/<userId>, verifies
// the per-user secret, wakes the user's pod, and forwards the update to the pod's
// local Telegram webhook listener. This is the wake-on-webhook primitive that lets
// Telegram work with scale-to-zero (Telegram delivers here, not to the asleep pod).
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/tg/")
	if id == "" || strings.ContainsAny(id, "/?") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sec, err := s.k8s.getSecret(r.Context(), id)
	if err != nil || sec == nil {
		http.Error(w, "unknown user", http.StatusNotFound)
		return
	}
	want := sec.Data["telegram-webhook-secret"]
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if len(want) == 0 || subtle.ConstantTimeCompare(want, []byte(got)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	r.Body.Close()

	// Guard so the reaper won't sleep the pod mid-delivery.
	one := "1"
	s.k8s.setAnnotations(r.Context(), id, map[string]*string{annBusy: &one})
	defer s.k8s.setAnnotations(context.Background(), id, map[string]*string{annBusy: nil})

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ColdStartTimeout+90*time.Second)
	defer cancel()
	if !s.wakeAndWait(ctx, id) {
		log.Printf("telegram user=%s: pod not ready", id)
		http.Error(w, "instance starting", http.StatusServiceUnavailable) // Telegram will retry
		return
	}
	s.touchActivity(ctx, id) // keep warm briefly for follow-up messages

	// The gateway's :8787 webhook listener starts a few seconds AFTER /healthz goes
	// ready, so on a cold wake the first forward can hit "connection refused".
	// Retry until the listener is up (within the cold-start budget).
	target := fmt.Sprintf("http://%s.%s.svc:8787/telegram-webhook", serviceName(id), s.cfg.UsersNS)
	var resp *http.Response
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", got)
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			log.Printf("telegram forward user=%s: %v", id, err)
			http.Error(w, "upstream not ready", http.StatusBadGateway)
			return
		case <-time.After(2 * time.Second):
		}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	log.Printf("telegram delivered user=%s upstream=%d", id, resp.StatusCode)
	w.WriteHeader(http.StatusOK) // ack Telegram once forwarded
}

// handleConnectTelegram (authenticated) wires up a user's Telegram bot: it stores
// the bot token + a generated webhook secret, writes the channel config into the
// pod with webhookUrl pointing back at this control plane, and restarts the gateway
// so it registers the webhook with Telegram (setWebhook).
func (s *Server) handleConnectTelegram(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionUser(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	base := s.webhookBase(r.Context())
	if base == "" {
		http.Error(w, "no public webhook URL available yet (cloudflared tunnel not ready)", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("bot_token"))
	if token == "" {
		http.Error(w, "bot_token required", http.StatusBadRequest)
		return
	}
	secret, err := randomToken()
	if err != nil {
		http.Error(w, "secret gen failed", http.StatusInternalServerError)
		return
	}
	if err := s.k8s.patchSecretData(r.Context(), id, map[string]string{
		"telegram-bot-token":      token,
		"telegram-webhook-secret": secret,
	}); err != nil {
		http.Error(w, "store failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*s.cfg.ColdStartTimeout+2*time.Minute)
	defer cancel()
	one := "1"
	s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: &one})
	defer s.k8s.setAnnotations(context.Background(), id, map[string]*string{annBusy: nil})

	if !s.wakeAndWait(ctx, id) {
		http.Error(w, "instance not ready", http.StatusServiceUnavailable)
		return
	}
	webhookURL := strings.TrimRight(base, "/") + "/tg/" + id
	cmds := [][]string{
		{"config", "set", "channels.telegram.botToken", token},
		{"config", "set", "channels.telegram.webhookSecret", secret},
		{"config", "set", "channels.telegram.webhookUrl", webhookURL},
		{"config", "set", "channels.telegram.webhookHost", "0.0.0.0"},
		{"config", "set", "channels.telegram.webhookPort", "8787", "--strict-json"},
		{"config", "set", "channels.telegram.enabled", "true", "--strict-json"},
	}
	for _, c := range cmds {
		if _, err := s.k8s.execInGateway(ctx, id, append([]string{"node", "openclaw.mjs"}, c...)...); err != nil {
			http.Error(w, "config failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Restart the gateway so it starts the Telegram channel and registers the
	// webhook with Telegram (setWebhook to webhookURL).
	if err := s.k8s.restart(ctx, id); err != nil {
		http.Error(w, "restart failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	time.Sleep(3 * time.Second) // let the old pod begin terminating before we wait
	if !s.wakeAndWait(ctx, id) {
		http.Error(w, "gateway did not come back", http.StatusServiceUnavailable)
		return
	}
	time.Sleep(5 * time.Second) // give the channel time to register setWebhook
	log.Printf("telegram connected user=%s webhook=%s", id, webhookURL)
	fmt.Fprintf(w, "Telegram connected. Webhook registered at %s\n", webhookURL)
}
