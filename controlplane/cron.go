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
		nextStr := d.Annotations[annCronNext]
		if nextStr == "" {
			continue
		}
		nextMs, err := strconv.ParseInt(nextStr, 10, 64)
		if err != nil {
			continue
		}
		asleep := d.Spec.Replicas == nil || *d.Spec.Replicas == 0
		running := d.Annotations[annBusy] == "1"
		// Wake once the slot is due (within a small lead). The in-pod scheduler is
		// disabled, so we force-run due jobs ourselves after the pod is ready —
		// cold-start time just delays the run slightly, it can't cause a miss.
		if asleep && !running && time.UnixMilli(nextMs).Before(now.Add(s.cfg.CronWakeLead)) {
			go s.wakeForCron(d.Labels[labelUser])
		}
	}
}

// wakeForCron wakes a sleeping pod, force-runs any due cron jobs, refreshes the
// mirror to their next occurrence, then releases it for the reaper.
func (s *Server) wakeForCron(id string) {
	ctx, cancel := context.WithTimeout(context.Background(),
		s.cfg.ColdStartTimeout+s.cfg.CronRunBuffer+2*time.Minute)
	defer cancel()

	one := "1"
	if err := s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: &one}); err != nil {
		log.Printf("cron wake guard user=%s: %v", id, err)
		return
	}
	defer s.k8s.setAnnotations(ctx, id, map[string]*string{annBusy: nil})

	log.Printf("cron wake user=%s", id)
	if !s.wakeAndWait(ctx, id) {
		log.Printf("cron wake user=%s: pod not ready in time", id)
		return
	}
	s.runDueJobs(ctx, id)
	// Advance the mirror to the next occurrence; reaper scales the pod down since
	// the cron wake never touched last-activity.
	s.refreshMirror(ctx, id)
	log.Printf("cron wake user=%s done; released for reaper", id)
}

// runDueJobs force-runs every enabled job whose next-fire time has arrived.
func (s *Server) runDueJobs(ctx context.Context, id string) {
	jobs, err := s.listJobs(ctx, id)
	if err != nil {
		log.Printf("cron runDue list user=%s: %v", id, err)
		return
	}
	now := time.Now().UnixMilli()
	for _, j := range jobs {
		if !j.Enabled || j.State.NextRunAtMs == 0 || j.State.NextRunAtMs > now {
			continue
		}
		if _, err := s.k8s.execInGateway(ctx, id, "node", "openclaw.mjs", "cron", "run", j.ID,
			"--wait", "--wait-timeout", "60000"); err != nil {
			log.Printf("cron run user=%s job=%s: %v", id, j.ID, err)
			continue
		}
		log.Printf("cron fired user=%s job=%s", id, j.ID)
	}
}
