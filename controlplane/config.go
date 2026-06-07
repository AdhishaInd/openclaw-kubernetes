package main

import (
	"os"
	"time"
)

// Config holds all runtime settings, sourced from environment variables so the
// same binary works in-cluster and (with KUBECONFIG) locally.
type Config struct {
	ListenAddr       string        // address the control plane listens on
	PublicOrigin     string        // browser-facing origin, e.g. http://localhost:8080
	UsersNS          string        // namespace that holds per-user resources
	SystemNS         string        // control-plane namespace (for cloudflared discovery)
	OpenclawImage    string        // image used for both gateway + onboarding initContainer
	SharedKeySecret  string        // Secret (in UsersNS) holding the shared Anthropic key
	DefaultModel     string        // default model set during onboarding
	TelegramBase     string        // public HTTPS base URL for Telegram webhooks (e.g. cloudflared)
	CookieKey        []byte        // HMAC key for signing session cookies
	IdleTimeout      time.Duration // scale a user pod to 0 after this much inactivity
	ColdStartTimeout time.Duration // how long to wait for a pod to become ready on wake
	ReaperTick       time.Duration // how often the idle reaper runs
	CronTick         time.Duration // how often the cron scheduler checks for due jobs
	CronWakeLead     time.Duration // wake a sleeping pod this long before a cron slot
	CronRunBuffer    time.Duration // keep the pod up this long past the slot for the run
}

func loadConfig() Config {
	return Config{
		ListenAddr:       env("LISTEN_ADDR", ":8080"),
		PublicOrigin:     env("PUBLIC_ORIGIN", "http://localhost:8080"),
		UsersNS:          env("USERS_NS", "oc-users"),
		SystemNS:         env("SYSTEM_NS", "oc-system"),
		OpenclawImage:    env("OPENCLAW_IMAGE", "ghcr.io/openclaw/openclaw:latest"),
		SharedKeySecret:  env("SHARED_KEY_SECRET", "oc-shared-anthropic"),
		DefaultModel:     env("DEFAULT_MODEL", "anthropic/claude-sonnet-4-6"),
		TelegramBase:     env("TELEGRAM_WEBHOOK_BASE", ""),
		CookieKey:        []byte(env("COOKIE_KEY", "dev-insecure-cookie-key-change-me")),
		IdleTimeout:      envDuration("IDLE_TIMEOUT", 15*time.Minute),
		ColdStartTimeout: envDuration("COLD_START_TIMEOUT", 90*time.Second),
		ReaperTick:       envDuration("REAPER_TICK", 60*time.Second),
		CronTick:         envDuration("CRON_TICK", 30*time.Second),
		CronWakeLead:     envDuration("CRON_WAKE_LEAD", 0),
		CronRunBuffer:    envDuration("CRON_RUN_BUFFER", 90*time.Second),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
