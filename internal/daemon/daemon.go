// Package daemon runs the long-lived service: encode workers draining the
// queue, plus a periodic library scan feeding it.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mtx/internal/config"
	"mtx/internal/encode"
	"mtx/internal/queue"
	"mtx/internal/scan"
)

// How long an idle worker waits before checking the queue again.
const idlePollInterval = 5 * time.Second

type Daemon struct {
	Config config.Config
	Store  *queue.Store
	Log    *slog.Logger
}

// Run blocks until ctx is canceled, then returns once workers have stopped.
// A worker interrupted mid-encode leaves its job "running"; queue.Open
// resets those to pending on the next start, so nothing is lost.
func (d *Daemon) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range d.Config.Workers {
		wg.Add(1)
		go func() { defer wg.Done(); d.drainQueue(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.scanPeriodically(ctx) }()
	wg.Wait()
}

func (d *Daemon) drainQueue(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := d.Store.ClaimNext()
		if err != nil {
			d.Log.Error("claiming next job", "error", err)
			sleep(ctx, idlePollInterval)
			continue
		}
		if job == nil {
			sleep(ctx, idlePollInterval)
			continue
		}

		d.Log.Info("job started", "path", job.Path)
		result, err := encode.ProcessFile(ctx, job.Path, job.Grain, d.Config, nil)
		if ctx.Err() != nil {
			// Interrupted by shutdown, not a real failure: leave the job
			// "running" so the restart reset re-queues it.
			d.Log.Info("job interrupted by shutdown", "path", job.Path)
			return
		}
		if recordErr := d.Store.RecordResult(job.ID, result, err); recordErr != nil {
			d.Log.Error("recording result", "path", job.Path, "error", recordErr)
		}

		switch {
		case err != nil:
			d.Log.Error("job failed", "path", job.Path, "error", err)
		case result.Skipped != "":
			d.Log.Info("job skipped", "path", job.Path, "reason", result.Skipped)
		default:
			d.Log.Info("job done", "path", result.FinalPath,
				"profile", result.Profile,
				"mb_before", result.SizeBefore/1e6, "mb_after", result.SizeAfter/1e6,
				"percent_smaller", int(result.PercentSmaller()),
				"original", result.Quarantined)
		}
	}
}

func (d *Daemon) scanPeriodically(ctx context.Context) {
	for {
		d.scanOnce()
		if !sleep(ctx, time.Duration(d.Config.ScanInterval)) {
			return
		}
	}
}

func (d *Daemon) scanOnce() {
	if len(d.Config.LibraryRoots) == 0 {
		return
	}
	found := 0
	err := scan.Walk(d.Config.LibraryRoots, func(path string) error {
		inserted, err := d.Store.Enqueue(path, false)
		if inserted {
			found++
		}
		return err
	})
	if err != nil {
		d.Log.Warn("library scan had problems", "error", err)
	}
	d.Log.Info("library scan complete", "new_jobs", found)
}

// sleep waits for the duration or until ctx is canceled; reports whether the
// full duration elapsed.
func sleep(ctx context.Context, duration time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(duration):
		return true
	}
}
