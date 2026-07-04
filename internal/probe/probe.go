// Package probe inspects a media file with ffprobe and distills the result
// into the few facts the transcode policy cares about.
package probe

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

type MediaInfo struct {
	Path           string
	VideoCodec     string // e.g. "h264", "hevc", "mpeg2video"
	Height         int
	DolbyVision    bool
	HDR            *HDRMetadata // nil for SDR content
	Duration       time.Duration
	SizeBytes      int64
	ColorPrimaries string // e.g. "bt2020"; empty when the source doesn't say
	ColorTransfer  string // e.g. "smpte2084" (PQ), "arib-std-b67" (HLG)
	ColorMatrix    string // e.g. "bt2020nc"
}

// HDRMetadata carries the static HDR10 metadata that must be re-attached
// explicitly when re-encoding, because ffmpeg does not carry it through on
// its own. Either field may be empty when the source lacks that record.
type HDRMetadata struct {
	MasterDisplay string // x265 master-display syntax: "G(x,y)B(x,y)R(x,y)WP(x,y)L(max,min)"
	ContentLight  string // x265 max-cll syntax: "maxCLL,maxFALL"
}

func (m MediaInfo) IsUHD() bool { return m.Height >= 1600 }
func (m MediaInfo) IsHDR() bool { return m.HDR != nil }
func (m MediaInfo) AlreadyEfficient() bool {
	return m.VideoCodec == "hevc" || m.VideoCodec == "av1"
}

func Probe(path string) (MediaInfo, error) {
	out, err := exec.Command(
		"ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path,
	).Output()
	if err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	return Parse(path, out)
}

// Parse turns raw ffprobe JSON into a MediaInfo. Split from Probe so tests
// can feed captured ffprobe output without running ffprobe.
func Parse(path string, ffprobeJSON []byte) (MediaInfo, error) {
	var report ffprobeReport
	if err := json.Unmarshal(ffprobeJSON, &report); err != nil {
		return MediaInfo{}, fmt.Errorf("parsing ffprobe output for %s: %w", path, err)
	}

	video := report.firstVideoStream()
	if video == nil {
		return MediaInfo{}, fmt.Errorf("%s: no video stream", path)
	}

	info := MediaInfo{
		Path:           path,
		VideoCodec:     video.CodecName,
		Height:         video.Height,
		DolbyVision:    video.hasDolbyVision(),
		HDR:            video.hdrMetadata(),
		ColorPrimaries: video.ColorPrimaries,
		ColorTransfer:  video.ColorTransfer,
		ColorMatrix:    video.ColorSpace,
	}
	if seconds, err := strconv.ParseFloat(report.Format.Duration, 64); err == nil {
		info.Duration = time.Duration(seconds * float64(time.Second))
	}
	if size, err := strconv.ParseInt(report.Format.Size, 10, 64); err == nil {
		info.SizeBytes = size
	}
	return info, nil
}

// --- ffprobe JSON shapes ---

type ffprobeReport struct {
	Streams []stream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
	} `json:"format"`
}

func (r *ffprobeReport) firstVideoStream() *stream {
	for i := range r.Streams {
		if r.Streams[i].CodecType == "video" {
			return &r.Streams[i]
		}
	}
	return nil
}

type stream struct {
	CodecType      string     `json:"codec_type"`
	CodecName      string     `json:"codec_name"`
	CodecTagString string     `json:"codec_tag_string"`
	Height         int        `json:"height"`
	ColorPrimaries string     `json:"color_primaries"`
	ColorTransfer  string     `json:"color_transfer"`
	ColorSpace     string     `json:"color_space"`
	SideData       []sideData `json:"side_data_list"`
}

type sideData struct {
	Type string `json:"side_data_type"`

	// Mastering display metadata: chromaticities and luminance as rationals,
	// e.g. "35400/50000".
	RedX         rational `json:"red_x"`
	RedY         rational `json:"red_y"`
	GreenX       rational `json:"green_x"`
	GreenY       rational `json:"green_y"`
	BlueX        rational `json:"blue_x"`
	BlueY        rational `json:"blue_y"`
	WhiteX       rational `json:"white_point_x"`
	WhiteY       rational `json:"white_point_y"`
	MaxLuminance rational `json:"max_luminance"`
	MinLuminance rational `json:"min_luminance"`

	// Content light level metadata.
	MaxContent int `json:"max_content"`
	MaxAverage int `json:"max_average"`
}
