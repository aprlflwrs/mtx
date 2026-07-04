// Package analyze walks a library and aggregates the codec, resolution,
// dynamic-range, and policy-decision facts probe and policy already know
// about each file, so the mix in a library can be seen without eyeballing
// files one at a time.
package analyze

import (
	"mtx/internal/policy"
	"mtx/internal/probe"
	"mtx/internal/scan"
)

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
func Walk(roots []string) (Report, error) {
	report := newReport()
	err := scan.Walk(roots, func(path string) error {
		media, probeErr := probe.Probe(path)
		if probeErr != nil {
			report.Errors = append(report.Errors, probeErr)
			return nil
		}
		report.add(media)
		return nil
	})
	return report, err
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
