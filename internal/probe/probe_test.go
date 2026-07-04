package probe

import "testing"

const sitcomJSON = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "codec_tag_string": "avc1", "height": 1080},
    {"codec_type": "audio", "codec_name": "ac3"}
  ],
  "format": {"duration": "1320.480000", "size": "1500000000"}
}`

const hdr10JSON = `{
  "streams": [
    {
      "codec_type": "video", "codec_name": "hevc", "codec_tag_string": "hvc1",
      "height": 2160,
      "color_primaries": "bt2020", "color_transfer": "smpte2084", "color_space": "bt2020nc",
      "side_data_list": [
        {
          "side_data_type": "Mastering display metadata",
          "red_x": "34000/50000", "red_y": "16000/50000",
          "green_x": "13250/50000", "green_y": "34500/50000",
          "blue_x": "7500/50000", "blue_y": "3000/50000",
          "white_point_x": "15635/50000", "white_point_y": "16450/50000",
          "min_luminance": "1/10000", "max_luminance": "10000000/10000"
        },
        {"side_data_type": "Content light level metadata", "max_content": 1000, "max_average": 400}
      ]
    }
  ],
  "format": {"duration": "7200.000000", "size": "60000000000"}
}`

const dolbyVisionJSON = `{
  "streams": [
    {
      "codec_type": "video", "codec_name": "hevc", "codec_tag_string": "dvhe",
      "height": 2160, "color_transfer": "smpte2084",
      "side_data_list": [{"side_data_type": "DOVI configuration record"}]
    }
  ],
  "format": {"duration": "7200.000000", "size": "60000000000"}
}`

func TestParseSitcom(t *testing.T) {
	m, err := Parse("ep.mkv", []byte(sitcomJSON))
	if err != nil {
		t.Fatal(err)
	}
	if m.VideoCodec != "h264" || m.Height != 1080 {
		t.Errorf("got codec=%s height=%d", m.VideoCodec, m.Height)
	}
	if m.IsUHD() || m.IsHDR() || m.DolbyVision || m.AlreadyEfficient() {
		t.Errorf("plain 1080p h264 misclassified: %+v", m)
	}
	if m.Duration.Seconds() != 1320.48 {
		t.Errorf("duration = %v", m.Duration)
	}
	if m.SizeBytes != 1500000000 {
		t.Errorf("size = %d", m.SizeBytes)
	}
}

func TestParseHDR10(t *testing.T) {
	m, err := Parse("movie.mkv", []byte(hdr10JSON))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsUHD() || !m.IsHDR() || m.DolbyVision {
		t.Fatalf("HDR10 misclassified: %+v", m)
	}
	wantDisplay := "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	if m.HDR.MasterDisplay != wantDisplay {
		t.Errorf("master display:\n got  %s\n want %s", m.HDR.MasterDisplay, wantDisplay)
	}
	if m.HDR.ContentLight != "1000,400" {
		t.Errorf("content light = %s", m.HDR.ContentLight)
	}
}

func TestParseDolbyVision(t *testing.T) {
	m, err := Parse("movie.mkv", []byte(dolbyVisionJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !m.DolbyVision {
		t.Errorf("Dolby Vision not detected: %+v", m)
	}
}

func TestParseNoVideoStream(t *testing.T) {
	_, err := Parse("song.flac", []byte(`{"streams":[{"codec_type":"audio"}],"format":{}}`))
	if err == nil {
		t.Error("expected an error for a file with no video stream")
	}
}
