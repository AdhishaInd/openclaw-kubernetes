package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName    = "oc_session"
	sessionMaxAge = 7 * 24 * time.Hour
)

// --- stateless signed-cookie sessions ---

func (s *Server) signSession(id string) string {
	exp := time.Now().Add(sessionMaxAge).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%d", id, exp)))
	mac := hmac.New(sha256.New, s.cfg.CookieKey)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

func (s *Server) verifySession(value string) (string, bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	mac := hmac.New(sha256.New, s.cfg.CookieKey)
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	fields := strings.SplitN(string(raw), "|", 2)
	if len(fields) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return fields[0], true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.signSession(id), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionMaxAge.Seconds()),
	})
}

// sessionUser returns the authenticated user id from the request, if any.
func (s *Server) sessionUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	return s.verifySession(c.Value)
}

// --- handlers ---

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderPage(w, "signup.html", nil)
		return
	}
	email, pass, err := credsFromForm(r)
	if err != nil {
		s.renderPage(w, "signup.html", map[string]string{"Error": err.Error()})
		return
	}
	id := userID(email)
	existing, err := s.k8s.getSecret(r.Context(), id)
	if err != nil {
		http.Error(w, "lookup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if existing != nil {
		s.renderPage(w, "signup.html", map[string]string{"Error": "an account with that email already exists"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hash failed", http.StatusInternalServerError)
		return
	}
	if err := s.ensureUser(r.Context(), id, email, string(hash)); err != nil {
		http.Error(w, "provisioning failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, id)
	s.redirectToApp(w, r, id)
}

// redirectToApp sends the user to their instance with the gateway token in the
// URL fragment (#token=…), OpenClaw's official mechanism: the Control UI applies
// it directly to settings, bypassing localStorage timing and the service worker.
func (s *Server) redirectToApp(w http.ResponseWriter, r *http.Request, id string) {
	token, err := s.gatewayToken(r.Context(), id)
	if err != nil || token == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/#token="+url.QueryEscape(token), http.StatusSeeOther)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderPage(w, "login.html", nil)
		return
	}
	email, pass, err := credsFromForm(r)
	if err != nil {
		s.renderPage(w, "login.html", map[string]string{"Error": err.Error()})
		return
	}
	id := userID(email)
	sec, err := s.k8s.getSecret(r.Context(), id)
	if err != nil {
		http.Error(w, "lookup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if sec == nil || bcrypt.CompareHashAndPassword(sec.Data["password-hash"], []byte(pass)) != nil {
		s.renderPage(w, "login.html", map[string]string{"Error": "invalid email or password"})
		return
	}
	s.setSessionCookie(w, id)
	s.redirectToApp(w, r, id)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func credsFromForm(r *http.Request) (email, password string, err error) {
	if err = r.ParseForm(); err != nil {
		return "", "", fmt.Errorf("bad form")
	}
	email = strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password = r.PostFormValue("password")
	if _, e := mail.ParseAddress(email); e != nil {
		return "", "", fmt.Errorf("enter a valid email address")
	}
	if len(password) < 8 {
		return "", "", fmt.Errorf("password must be at least 8 characters")
	}
	return email, password, nil
}

// gatewayToken fetches the per-user gateway token used to authenticate to the pod.
func (s *Server) gatewayToken(ctx context.Context, id string) (string, error) {
	sec, err := s.k8s.getSecret(ctx, id)
	if err != nil || sec == nil {
		return "", fmt.Errorf("no secret for user")
	}
	return string(sec.Data["gateway-token"]), nil
}
