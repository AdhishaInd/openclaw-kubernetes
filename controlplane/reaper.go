package main

import (
	"context"
	"log"
	"time"
)

// runReaper periodically scales idle user Deployments back to zero. A pod counts
// as active while its last-activity annotation is within the idle timeout; the
// proxy refreshes that annotation on every request (including live WebSockets).
func (s *Server) runReaper(ctx context.Context) {
	tick := time.NewTicker(s.cfg.ReaperTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.reapOnce(ctx)
		}
	}
}

func (s *Server) reapOnce(ctx context.Context) {
	deps, err := s.k8s.listManagedDeployments(ctx)
	if err != nil {
		log.Printf("reaper list: %v", err)
		return
	}
	now := time.Now()
	for _, d := range deps {
		if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
			continue // already asleep
		}
		if d.Annotations[annCronRunning] == "1" {
			continue // held up for a cron slot — don't reap mid-run
		}
		last, err := time.Parse(time.RFC3339, d.Annotations[annLastActive])
		if err != nil {
			last = d.CreationTimestamp.Time // missing/invalid annotation: fall back to creation
		}
		if now.Sub(last) < s.cfg.IdleTimeout {
			continue
		}
		id := d.Labels[labelUser]
		// Capture the cron schedule while the pod is still up, so the scheduler
		// can wake it for due jobs while it sleeps.
		s.refreshMirror(ctx, id)
		if err := s.k8s.scaleTo(ctx, id, 0); err != nil {
			log.Printf("reaper scale-down user=%s: %v", id, err)
			continue
		}
		log.Printf("reaper: scaled user=%s to 0 (idle %s)", id, now.Sub(last).Round(time.Second))
	}
}
