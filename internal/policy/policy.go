// Package policy decides what to do with a media file. It is a pure
// function of the probed facts — no I/O — so the whole decision table is
// unit-testable and readable in one place.
package policy

import "mtx/internal/probe"

type Profile string

const (
	HDQuickSync  Profile = "hd-qsv"         // hardware HEVC via Quick Sync
	UHDHDRx265   Profile = "uhd-hdr-x265"   // software x265, explicit HDR metadata
	UHDSDRx265   Profile = "uhd-sdr-x265"   // software x265, conservative CRF
	AV1FilmGrain Profile = "av1-film-grain" // software SVT-AV1 with grain synthesis
)

type SkipReason string

const (
	AlreadyEfficient SkipReason = "already-efficient" // video is already HEVC/AV1
	DolbyVision      SkipReason = "dolby-vision"      // cannot be re-encoded safely without the original RPU
)

type Decision struct {
	Profile    Profile    // set when the file should be transcoded
	SkipReason SkipReason // set when it should be left alone
}

func (d Decision) ShouldTranscode() bool { return d.Profile != "" }

// Decide maps a file's probed facts to an action. Order matters and reads
// top-down like the design's decision table: safety rules first, then the
// opt-in grain path, then resolution tiers.
//
// Grain synthesis is only honored for SDR sources: for HDR content, correct
// HDR metadata handling (the x265 path) outranks grain preservation.
func Decide(m probe.MediaInfo, grainRequested bool) Decision {
	switch {
	case m.AlreadyEfficient():
		return skip(AlreadyEfficient)
	case m.DolbyVision:
		return skip(DolbyVision)
	case grainRequested && !m.IsHDR():
		return transcode(AV1FilmGrain)
	case !m.IsUHD():
		return transcode(HDQuickSync)
	case m.IsHDR():
		return transcode(UHDHDRx265)
	default:
		return transcode(UHDSDRx265)
	}
}

func skip(r SkipReason) Decision   { return Decision{SkipReason: r} }
func transcode(p Profile) Decision { return Decision{Profile: p} }
