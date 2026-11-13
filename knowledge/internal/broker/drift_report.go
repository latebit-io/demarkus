package broker

import (
	"context"
	"sync"

	"github.com/latebit-io/demarkus/knowledge/internal/driftreport"
)

// startDriftReports audits the knowledge worlds served in process on a
// schedule and writes each one's drift report, when driftReport.enabled.
func (b *backgroundTasks) startDriftReports(ctx context.Context, wg *sync.WaitGroup) {
	d := b.cfg.DriftReport
	worlds := b.cfg.LocalKnowledgeWorlds()
	if !d.Enabled || b.local == nil || len(worlds) == 0 {
		return
	}
	log := b.log.With("task", "drift-report")
	log.Info("broker: starting drift reports", "interval", d.Interval, "worlds", worlds)
	b.lead(ctx, wg, leaderTask{name: "drift-report", lease: d.LeaseName, run: func(ctx context.Context) {
		driftreport.Run(ctx, driftreport.Config{Worlds: worlds, Store: b.local, Floor: b.floor, Interval: d.Interval, Log: log})
	}})
}
