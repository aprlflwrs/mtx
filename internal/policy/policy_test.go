package policy

import (
	"testing"

	"mtx/internal/probe"
)

func TestDecide(t *testing.T) {
	hdr := &probe.HDRMetadata{MasterDisplay: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"}

	tests := []struct {
		name  string
		media probe.MediaInfo
		grain bool
		want  Decision
	}{
		{
			name:  "1080p H.264 sitcom goes to Quick Sync",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 1080},
			want:  Decision{Profile: HDQuickSync},
		},
		{
			name:  "old MPEG-2 rip goes to Quick Sync",
			media: probe.MediaInfo{VideoCodec: "mpeg2video", Height: 480},
			want:  Decision{Profile: HDQuickSync},
		},
		{
			name:  "already HEVC is left alone",
			media: probe.MediaInfo{VideoCodec: "hevc", Height: 1080},
			want:  Decision{SkipReason: AlreadyEfficient},
		},
		{
			name:  "already AV1 is left alone",
			media: probe.MediaInfo{VideoCodec: "av1", Height: 2160},
			want:  Decision{SkipReason: AlreadyEfficient},
		},
		{
			name:  "Dolby Vision is never re-encoded",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160, DolbyVision: true},
			want:  Decision{SkipReason: DolbyVision},
		},
		{
			name:  "4K HDR goes to software x265",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160, HDR: hdr},
			want:  Decision{Profile: UHDHDRx265},
		},
		{
			name:  "4K SDR goes to software x265",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160},
			want:  Decision{Profile: UHDSDRx265},
		},
		{
			name:  "grain flag routes SDR to AV1 film grain synthesis",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160},
			grain: true,
			want:  Decision{Profile: AV1FilmGrain},
		},
		{
			name:  "grain flag on HDR content defers to HDR-safe x265 path",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160, HDR: hdr},
			grain: true,
			want:  Decision{Profile: UHDHDRx265},
		},
		{
			name:  "grain flag on Dolby Vision still skips",
			media: probe.MediaInfo{VideoCodec: "h264", Height: 2160, DolbyVision: true},
			grain: true,
			want:  Decision{SkipReason: DolbyVision},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.media, tt.grain)
			if got != tt.want {
				t.Errorf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
