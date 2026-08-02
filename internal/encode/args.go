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

// videoPipeline is the ffmpeg argument list for one profile, split by where
// each part must sit relative to -i: hardware device setup has to precede
// the input, everything else follows it.
type videoPipeline struct {
	preInput []string // global options that must appear before -i (e.g. hardware device init)
	filter   []string // -vf entries applied to the video stream before encoding
	encoder  []string // -c:v and its encoder-specific flags
}

// Args returns the full ffmpeg argument list (excluding the "ffmpeg" binary
// itself) to transcode src into dst under the given profile.
//
// Every profile keeps all streams (-map 0) and copies audio, subtitles, and
// chapters untouched — only the video stream is re-encoded.
func Args(m probe.MediaInfo, profile policy.Profile, q config.Quality, dst string) ([]string, error) {
	pipeline, err := videoArgs(m, profile, q)
	if err != nil {
		return nil, err
	}

	args := append([]string{}, pipeline.preInput...)
	args = append(args, "-y", "-i", m.Path, "-map", "0", "-c:a", "copy", "-c:s", "copy")
	args = append(args, pipeline.filter...)
	args = append(args, pipeline.encoder...)
	return append(args, dst), nil
}

func videoArgs(m probe.MediaInfo, profile policy.Profile, q config.Quality) (videoPipeline, error) {
	switch profile {
	case policy.HDQuickSync:
		// hevc_vaapi, not hevc_qsv: hevc_qsv itself works fine on current
		// driver versions (media-driver 25.2.3 + libmfx-gen1.2 25.1.4) —
		// an earlier belief that its MFX/oneVPL layer rejected every
		// parameter combination is stale, fixed by a driver update. But at
		// matching -global_quality numbers, QSV measured meaningfully worse
		// VMAF than vaapi on real content (quality-scale numbers aren't
		// portable across encoder wrappers), and QSV's lookahead-related
		// flags (-extbrc/-mbbrc/-look_ahead_depth) measured zero effect
		// under ICQ rate control — so vaapi stays until QSV gets its own
		// calibrated quality target and a real head-to-head validates it's
		// actually better, not just numerically smaller at the same digit.
		//
		// -bf 4 -b_depth 2 (vaapi defaults to bf=2, the lowest of any HEVC
		// backend here): validated via direct ffmpeg+VMAF comparison on two
		// content types (old grainy film, modern WEB-DL) — smaller output
		// AND better VMAF AND no speed cost on both, no downside found.
		//
		// This driver rejects explicit ICQ (confirmed: "Driver does not
		// support ICQ RC mode"); leaving rc_mode on auto with only
		// -global_quality set makes it choose QVBR instead, which is
		// quality-targeted the same way ICQ is, just with an added soft
		// bitrate ceiling. Explicitly setting -rc_mode ICQ with a large
		// -bufsize measured zero difference from the auto/QVBR fallback on
		// real content, so it's not worth the extra flags.
		return videoPipeline{
			preInput: []string{"-init_hw_device", "vaapi=hw"},
			filter:   []string{"-vf", "format=nv12,hwupload"},
			encoder:  []string{"-c:v", "hevc_vaapi", "-global_quality", strconv.Itoa(q.HDGlobalQuality), "-bf", "4", "-b_depth", "2"},
		}, nil

	case policy.UHDSDRx265:
		return videoPipeline{encoder: []string{
			"-c:v", "libx265",
			"-preset", "slow",
			"-crf", strconv.Itoa(q.UHDSDRCRF),
			"-pix_fmt", "yuv420p10le",
		}}, nil

	case policy.UHDHDRx265:
		return videoPipeline{encoder: append([]string{
			"-c:v", "libx265",
			"-preset", "slow",
			"-crf", strconv.Itoa(q.UHDHDRCRF),
			"-pix_fmt", "yuv420p10le",
		}, hdrPreservationArgs(m)...)}, nil

	case policy.AV1FilmGrain:
		// film-grain-denoise defaults to on, which discards fine detail its
		// denoiser mistakes for grain; grain synthesis works fine without it.
		// preset 4 (not the slower 2): grain-retention guidance favors 2, but
		// it's nearly 3x the cost of 4 with CPU-only encode on this hardware,
		// and this profile is already opt-in/per-file rather than bulk.
		// tune=0 (VQ) is mainline SVT-AV1's perceptual mode, matching intent
		// to preserve how grain looks rather than optimize PSNR.
		return videoPipeline{encoder: []string{
			"-c:v", "libsvtav1",
			"-preset", "4",
			"-crf", strconv.Itoa(q.AV1CRF),
			"-svtav1-params", fmt.Sprintf("film-grain=%d:film-grain-denoise=0:tune=0", q.AV1FilmGrainLevel),
		}}, nil
	}
	return videoPipeline{}, fmt.Errorf("no ffmpeg arguments defined for profile %q", profile)
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
