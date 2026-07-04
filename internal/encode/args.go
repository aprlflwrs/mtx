// Package encode builds and runs ffmpeg commands for the policy's profiles,
// verifies the results, and performs the quarantine swap.
package encode

import (
	"fmt"
	"strconv"
	"strings"

	"mtx/internal/config"
	"mtx/internal/policy"
	"mtx/internal/probe"
)

// Args returns the full ffmpeg argument list (excluding the "ffmpeg" binary
// itself) to transcode src into dst under the given profile.
//
// Every profile keeps all streams (-map 0) and copies audio, subtitles, and
// chapters untouched — only the video stream is re-encoded.
func Args(m probe.MediaInfo, profile policy.Profile, q config.Quality, dst string) ([]string, error) {
	keepEverythingButVideo := []string{
		"-y", "-i", m.Path,
		"-map", "0",
		"-c:a", "copy",
		"-c:s", "copy",
	}

	video, err := videoArgs(m, profile, q)
	if err != nil {
		return nil, err
	}
	return append(append(keepEverythingButVideo, video...), dst), nil
}

func videoArgs(m probe.MediaInfo, profile policy.Profile, q config.Quality) ([]string, error) {
	switch profile {
	case policy.HDQuickSync:
		// -look_ahead 1 upgrades ICQ to lookahead-ICQ: better quality for
		// nearly the same speed.
		return []string{
			"-c:v", "hevc_qsv",
			"-preset", "slow",
			"-global_quality", strconv.Itoa(q.HDGlobalQuality),
			"-look_ahead", "1",
		}, nil

	case policy.UHDSDRx265:
		return []string{
			"-c:v", "libx265",
			"-preset", "slow",
			"-crf", strconv.Itoa(q.UHDSDRCRF),
			"-pix_fmt", "yuv420p10le",
		}, nil

	case policy.UHDHDRx265:
		return append([]string{
			"-c:v", "libx265",
			"-preset", "slow",
			"-crf", strconv.Itoa(q.UHDHDRCRF),
			"-pix_fmt", "yuv420p10le",
		}, hdrPreservationArgs(m)...), nil

	case policy.AV1FilmGrain:
		// film-grain-denoise defaults to on, which discards fine detail its
		// denoiser mistakes for grain; grain synthesis works fine without it.
		return []string{
			"-c:v", "libsvtav1",
			"-preset", "6",
			"-crf", strconv.Itoa(q.AV1CRF),
			"-svtav1-params", fmt.Sprintf("film-grain=%d:film-grain-denoise=0", q.AV1FilmGrainLevel),
		}, nil
	}
	return nil, fmt.Errorf("no ffmpeg arguments defined for profile %q", profile)
}

// hdrPreservationArgs re-attaches the source's HDR signaling explicitly,
// because ffmpeg/x265 do not carry it through a re-encode on their own.
// Dropping it would produce washed-out, SDR-looking playback.
func hdrPreservationArgs(m probe.MediaInfo) []string {
	args := []string{
		"-color_primaries", m.ColorPrimaries,
		"-color_trc", m.ColorTransfer,
		"-colorspace", m.ColorMatrix,
	}

	// ffprobe and x265 conveniently spell these values the same way.
	x265Params := []string{
		"colorprim=" + m.ColorPrimaries,
		"transfer=" + m.ColorTransfer,
		"colormatrix=" + m.ColorMatrix,
	}
	// hdr10 signaling and its static metadata apply to PQ content only;
	// HLG carries everything it needs in the transfer function itself.
	if m.ColorTransfer == "smpte2084" {
		x265Params = append(x265Params, "hdr10=1", "hdr10-opt=1")
		if m.HDR.MasterDisplay != "" {
			x265Params = append(x265Params, "master-display="+m.HDR.MasterDisplay)
		}
		if m.HDR.ContentLight != "" {
			x265Params = append(x265Params, "max-cll="+m.HDR.ContentLight)
		}
	}
	return append(args, "-x265-params", strings.Join(x265Params, ":"))
}
