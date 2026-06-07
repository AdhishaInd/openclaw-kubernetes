package main

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"
)

// --- cron schedule mirror ---
//
// OpenClaw's cron scheduler runs inside the gateway, and missed slots are NOT
// caught up on restart (verified). So for scale-to-zero we mirror each user's
// earliest next-fire time onto their Deployment while the pod is awake, then a
// scheduler wakes the pod shortly before that slot so the in-pod scheduler fires
// the job naturally.

type cronJobInfo struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	State   struct {
		NextRunAtMs int64 `json:"nextRunAtMs"`
	} `json:"state"`
}
type cronListResp struct {
	Jobs []cronJobInfo `json:"jobs"`
}

// listJobs reads the user's cron jobs from the live gateway.
func (s *Server) listJobs(ctx context.Context, id string) ([]cronJobInfo, error) {
	out, err := s.k8s.execInGateway(ctx, id, "node", "openclaw.mjs", "cron", "list", "--json")
	if err != nil {
		return nil, err
	}
	var resp cronListResp
	if err := json.Unmarshal([]byte(jsonSlice(out)), &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// refreshMirror reads cron jobs from the live gateway and stamps the earliest
// enabled next-fire time onto annCronNext (removing it when there are no jobs).
func (s *Server) refreshMirror(ctx context.Context, id string) {
	jobs, err := s.listJobs(ctx, id)
	if err != nil {
		log.Printf("cron mirror user=%s: %v", id, err)
		return
	}
	var next int64
	for _, j := range jobs {
		if !j.Enabled || j.State.NextRunAtMs == 0 {
			continue
		}
		if next == 0 || j.State.NextRunAtMs < next {
			next = j.State.NextRunAtMs
		}
	}
	var val *string
	if next > 0 {
		v := strconv.FormatInt(next, 10)
		val = &v
	}
	if err := s.k8s.setAnnotations(ctx, id, map[string]*string{annCronNext: val}); err != nil {
		log.Printf("cron mirror stamp user=%s: %v", id, err)
		return
	}
	if next > 0 {
		log.Printf("cron mirror user=%s next=%s", id, time.UnixMilli(next).UTC().Format(time.RFC3339))
	}
}

// jsonSlice returns the substring from the first '{' so a stray leading log line
// from the CLI doesn't break JSON parsing.
func jsonSlice(s string) string {
	if i := strings.IndexByte(s, '{'); i >= 0 {
		return s[i:]
	}
	return s
}

// --- cron wake scheduler ---

func (s *Server) runCronScheduler(ctx context.Context) {
	tick := time.NewTicker(s.cfg.CronTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.cronTick(ctx)
		}
	}
}

func (s *Server) cronTick(ctx context.Context) {
	deps, err := s.k8s.listManagedDeployments(ctx)
	if err != nil {
		log.Printf("cron tick: %v", err)
		return
	}
	now := time.Now()
	for _, d := range deps {
		id := d.Labels[labelUser]
		if d.Annotations[annBusy] == "1" {
			continue // busy with cron/webhook work already
		}
		// Warm pods run their own cron via the in-pod scheduler; nothing to do here.
		if d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
			continue
		}
		// Sleeping pod: wake it shortly BEFORE its next slot so the in-pod scheduler
		// is running to fire the job (missed slots are not caught up). The mirrored
		// next-fire time was stamped when the pod was last awake.
		nextStr := d.Annotations[annCronNext]
		if nextStr == "" {
			continue
		}
		nextMs, perr := strconv.ParseInt(nextStr, 10, 64)
		if perr != nil {
			continue
		}
		if time.UnixMilli(nextMs).Add(-s.cfg.CronWakeLead).Before(now) {
			go s.wakeForCron(id, nextMs)
		}
	}
}

// wakeForCron wakes a sleeping pod ahead of a cron slot and holds it up through the
// slot so the in-pod scheduler fires the job, then refreshes the mirror to the next
// occurrence and releases the pod for the reaper. No `cron run` (which needs CLI
// device-scope approval) — the gateway's own scheduler does the firing.
func (s *Server) wakeForCron(id string, nextMs int64) {
	ctx, cancel := context.WithTimeout(context.Background(),
		s.cfg.ColdStartTimeout+s.cfg.CronWakeLead+s.cfg.CronRunBuffer+2*time.Minute)
	defer cancel()

	one := "1"
	if err := s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: &one}); err != nil {
		log.Printf("cron wake guard user=%s: %v", id, err)
		return
	}
	defer s.k8s.setAnnotations(context.Background(), id, map[string]*string{annBusy: nil})

	log.Printf("cron wake user=%s for slot %s", id, time.UnixMilli(nextMs).UTC().Format(time.RFC3339))
	if !s.wakeAndWait(ctx, id) {
		log.Printf("cron wake user=%s: pod not ready in time", id)
		return
	}
	// Hold until the slot has passed plus a buffer so the in-pod scheduler fires it.
	fireBy := time.UnixMilli(nextMs).Add(s.cfg.CronRunBuffer)
	if d := time.Until(fireBy); d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
	s.refreshMirror(ctx, id) // advance cron-next to the next occurrence
	log.Printf("cron wake user=%s done; released for reaper", id)
}
