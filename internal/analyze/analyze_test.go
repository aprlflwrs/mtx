package analyze

import (
	"testing"

	"mtx/internal/probe"
)

func TestReportAdd(t *testing.T) {
	hdr := &probe.HDRMetadata{MasterDisplay: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"}

	report := newReport()
	files := []probe.MediaInfo{
		{VideoCodec: "h264", Height: 1080, SizeBytes: 100},
		{VideoCodec: "h264", Height: 1080, SizeBytes: 200},
		{VideoCodec: "hevc", Height: 1080, SizeBytes: 50},
		{VideoCodec: "h264", Height: 2160, HDR: hdr, ColorTransfer: "smpte2084", SizeBytes: 400},
		{VideoCodec: "h264", Height: 2160, DolbyVision: true, SizeBytes: 300},
		{VideoCodec: "mpeg2video", Height: 480, SizeBytes: 20},
	}
	for _, f := range files {
		report.add(f)
	}

	if report.Files != len(files) {
		t.Errorf("Files = %d, want %d", report.Files, len(files))
	}
	if report.Bytes != 1070 {
		t.Errorf("Bytes = %d, want 1070", report.Bytes)
	}

	wantResolutionCodec := map[string]map[string]Bucket{
		"1080p":  {"h264": {Files: 2, Bytes: 300}, "hevc": {Files: 1, Bytes: 50}},
		"4K/UHD": {"h264": {Files: 2, Bytes: 700}},
		"SD":     {"mpeg2video": {Files: 1, Bytes: 20}},
	}
	for tier, codecs := range wantResolutionCodec {
		for codec, want := range codecs {
			if got := report.ByResolutionCodec[tier][codec]; got != want {
				t.Errorf("ByResolutionCodec[%s][%s] = %+v, want %+v", tier, codec, got, want)
			}
		}
	}

	wantResolution := map[string]Bucket{
		"1080p":  {Files: 3, Bytes: 350},
		"4K/UHD": {Files: 2, Bytes: 700},
		"SD":     {Files: 1, Bytes: 20},
	}
	for tier, want := range wantResolution {
		if got := report.ByResolution[tier]; got != want {
			t.Errorf("ByResolution[%s] = %+v, want %+v", tier, got, want)
		}
	}

	wantDynamicRange := map[string]Bucket{
		"SDR":          {Files: 4, Bytes: 370},
		"HDR10":        {Files: 1, Bytes: 400},
		"Dolby Vision": {Files: 1, Bytes: 300},
	}
	for label, want := range wantDynamicRange {
		if got := report.ByDynamicRange[label]; got != want {
			t.Errorf("ByDynamicRange[%s] = %+v, want %+v", label, got, want)
		}
	}

	wantDecision := map[string]Bucket{
		"hd-qsv":                  {Files: 3, Bytes: 320}, // both 1080p h264 files plus the 480p mpeg2video
		"skip: already-efficient": {Files: 1, Bytes: 50},
		"uhd-hdr-x265":            {Files: 1, Bytes: 400},
		"skip: dolby-vision":      {Files: 1, Bytes: 300},
	}
	for label, want := range wantDecision {
		if got := report.ByDecision[label]; got != want {
			t.Errorf("ByDecision[%s] = %+v, want %+v", label, got, want)
		}
	}
}

func TestResolutionTier(t *testing.T) {
	tests := []struct {
		height int
		want   string
	}{
		{480, "SD"},
		{576, "SD"},
		{719, "SD"},
		{720, "720p"},
		{1079, "720p"},
		{1080, "1080p"},
		{1599, "1080p"},
		{1600, "4K/UHD"},
		{2160, "4K/UHD"},
	}
	for _, tt := range tests {
		if got := resolutionTier(tt.height); got != tt.want {
			t.Errorf("resolutionTier(%d) = %q, want %q", tt.height, got, tt.want)
		}
	}
}

func TestDynamicRange(t *testing.T) {
	tests := []struct {
		name  string
		media probe.MediaInfo
		want  string
	}{
		{"SDR", probe.MediaInfo{}, "SDR"},
		{"HDR10", probe.MediaInfo{HDR: &probe.HDRMetadata{}, ColorTransfer: "smpte2084"}, "HDR10"},
		{"HLG", probe.MediaInfo{HDR: &probe.HDRMetadata{}, ColorTransfer: "arib-std-b67"}, "HLG"},
		{"Dolby Vision overrides transfer", probe.MediaInfo{DolbyVision: true, HDR: &probe.HDRMetadata{}, ColorTransfer: "smpte2084"}, "Dolby Vision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dynamicRange(tt.media); got != tt.want {
				t.Errorf("dynamicRange() = %q, want %q", got, tt.want)
			}
		})
	}
}
