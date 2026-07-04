package encode

import (
	"strings"
	"testing"

	"mtx/internal/config"
	"mtx/internal/policy"
	"mtx/internal/probe"
)

func buildArgs(t *testing.T, m probe.MediaInfo, p policy.Profile) string {
	t.Helper()
	args, err := Args(m, p, config.Defaults().Quality, "out.mkv")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(args, " ")
}

func TestEveryProfileKeepsAudioAndSubtitlesUntouched(t *testing.T) {
	m := probe.MediaInfo{Path: "in.mkv", ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorMatrix: "bt709", HDR: &probe.HDRMetadata{}}
	for _, p := range []policy.Profile{policy.HDQuickSync, policy.UHDSDRx265, policy.UHDHDRx265, policy.AV1FilmGrain} {
		cmd := buildArgs(t, m, p)
		for _, want := range []string{"-map 0", "-c:a copy", "-c:s copy"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("%s: missing %q in: %s", p, want, cmd)
			}
		}
	}
}

func TestQuickSyncUsesLookaheadICQ(t *testing.T) {
	cmd := buildArgs(t, probe.MediaInfo{Path: "ep.mkv"}, policy.HDQuickSync)
	for _, want := range []string{"-c:v hevc_qsv", "-global_quality 22", "-look_ahead 1"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in: %s", want, cmd)
		}
	}
}

func TestHDR10MetadataIsReattachedExplicitly(t *testing.T) {
	m := probe.MediaInfo{
		Path:           "movie.mkv",
		ColorPrimaries: "bt2020",
		ColorTransfer:  "smpte2084",
		ColorMatrix:    "bt2020nc",
		HDR: &probe.HDRMetadata{
			MasterDisplay: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)",
			ContentLight:  "1000,400",
		},
	}
	cmd := buildArgs(t, m, policy.UHDHDRx265)
	for _, want := range []string{
		"-color_primaries bt2020", "-color_trc smpte2084", "-colorspace bt2020nc",
		"hdr10=1", "hdr10-opt=1",
		"master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)",
		"max-cll=1000,400",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in: %s", want, cmd)
		}
	}
}

func TestHLGDoesNotGetHDR10Signaling(t *testing.T) {
	m := probe.MediaInfo{
		Path:           "show.mkv",
		ColorPrimaries: "bt2020",
		ColorTransfer:  "arib-std-b67",
		ColorMatrix:    "bt2020nc",
		HDR:            &probe.HDRMetadata{},
	}
	cmd := buildArgs(t, m, policy.UHDHDRx265)
	if strings.Contains(cmd, "hdr10") {
		t.Errorf("HLG content must not carry hdr10 params: %s", cmd)
	}
	if !strings.Contains(cmd, "transfer=arib-std-b67") {
		t.Errorf("HLG transfer not preserved: %s", cmd)
	}
}

func TestFilmGrainSynthesisDisablesDenoising(t *testing.T) {
	cmd := buildArgs(t, probe.MediaInfo{Path: "film.mkv"}, policy.AV1FilmGrain)
	if !strings.Contains(cmd, "film-grain=8:film-grain-denoise=0") {
		t.Errorf("grain synthesis args wrong: %s", cmd)
	}
}
