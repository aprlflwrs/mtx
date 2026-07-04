// Package analyze walks a library and aggregates the codec, resolution,
// dynamic-range, and policy-decision facts probe and policy already know
// about each file, so the mix in a library can be seen without eyeballing
// files one at a time.
package analyze

import (
	"runtime"
	"sync"

	"mtx/internal/policy"
	"mtx/internal/probe"
	"mtx/internal/scan"
)

// probeConcurrency bounds how many ffprobe processes run at once. Probing
// only reads container headers, not the whole file, so it's cheap enough to
// run well beyond the CPU count without saturating anything.
func probeConcurrency() int {
	return min(runtime.NumCPU()*4, 32)
}

// Bucket tallies the files and bytes that share some attribute.
type Bucket struct {
	Files int
	Bytes int64
}

// Report is a set of breakdowns over the same library sweep, each keyed by
// a different attribute of the probed files.
type Report struct {
	Files          int
	Bytes          int64
	ByCodec        map[string]Bucket
	ByResolution   map[string]Bucket
	ByDynamicRange map[string]Bucket
	ByDecision     map[string]Bucket // policy.Decide's profile, or "skip: <reason>"
	Errors         []error           // files that couldn't be probed
}

func newReport() Report {
	return Report{
		ByCodec:        make(map[string]Bucket),
		ByResolution:   make(map[string]Bucket),
		ByDynamicRange: make(map[string]Bucket),
		ByDecision:     make(map[string]Bucket),
	}
}

// Walk probes every video file under roots and returns the aggregated
// report. A file that fails to probe is recorded in Report.Errors rather
// than aborting the sweep; the returned error carries only filesystem
// problems (unreadable directories and the like) from scan.Walk.
//
// Discovery stays single-threaded (scan.Walk), but probing fans out across
// a worker pool: each ffprobe invocation pays for a process spawn, and a
// library sweep is otherwise bottlenecked on that one file at a time.
func Walk(roots []string) (Report, error) {
	paths := make(chan string)
	results := make(chan probeOutcome)

	var workers sync.WaitGroup
	for range probeConcurrency() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for path := range paths {
				media, err := probe.Probe(path)
				results <- probeOutcome{media, err}
			}
		}()
	}

	var walkErr error
	go func() {
		walkErr = scan.Walk(roots, func(path string) error {
			paths <- path
			return nil
		})
		close(paths)
		workers.Wait()
		close(results)
	}()

	report := newReport()
	for outcome := range results {
		if outcome.err != nil {
			report.Errors = append(report.Errors, outcome.err)
			continue
		}
		report.add(outcome.media)
	}
	return report, walkErr
}

type probeOutcome struct {
	media probe.MediaInfo
	err   error
}

func (r *Report) add(m probe.MediaInfo) {
	r.Files++
	r.Bytes += m.SizeBytes
	bump(r.ByCodec, m.VideoCodec, m.SizeBytes)
	bump(r.ByResolution, resolutionTier(m.Height), m.SizeBytes)
	bump(r.ByDynamicRange, dynamicRange(m), m.SizeBytes)
	bump(r.ByDecision, decisionLabel(m), m.SizeBytes)
}

func bump(buckets map[string]Bucket, key string, size int64) {
	b := buckets[key]
	b.Files++
	b.Bytes += size
	buckets[key] = b
}

// resolutionTier mirrors the height cutoff policy.MediaInfo.IsUHD uses, plus
// the common tiers below it.
func resolutionTier(height int) string {
	switch {
	case height >= 1600:
		return "4K/UHD"
	case height >= 1080:
		return "1080p"
	case height >= 720:
		return "720p"
	default:
		return "SD"
	}
}

func dynamicRange(m probe.MediaInfo) string {
	switch {
	case m.DolbyVision:
		return "Dolby Vision"
	case m.HDR == nil:
		return "SDR"
	case m.ColorTransfer == "smpte2084":
		return "HDR10"
	case m.ColorTransfer == "arib-std-b67":
		return "HLG"
	default:
		return "HDR (other)"
	}
}

// decisionLabel reports what a bulk `mtx enqueue` (no --grain) would do with
// this file, since that's the decision an unattended library sweep makes.
func decisionLabel(m probe.MediaInfo) string {
	decision := policy.Decide(m, false)
	if decision.ShouldTranscode() {
		return string(decision.Profile)
	}
	return "skip: " + string(decision.SkipReason)
}
