package probe

import (
	"fmt"
	"math"
	"strings"
)

// Dolby Vision shows up either as a DV codec tag (dvh1/dvhe) or as a
// "DOVI configuration record" side-data entry.
func (s *stream) hasDolbyVision() bool {
	if s.CodecTagString == "dvh1" || s.CodecTagString == "dvhe" {
		return true
	}
	for _, sd := range s.SideData {
		if strings.Contains(strings.ToLower(sd.Type), "dovi") ||
			strings.Contains(strings.ToLower(sd.Type), "dolby vision") {
			return true
		}
	}
	return false
}

// hdrMetadata returns nil for SDR content. Content is HDR when its transfer
// function is PQ (HDR10) or HLG; the static metadata records are attached
// when the source carries them.
func (s *stream) hdrMetadata() *HDRMetadata {
	if s.ColorTransfer != "smpte2084" && s.ColorTransfer != "arib-std-b67" {
		return nil
	}
	hdr := &HDRMetadata{}
	for _, sd := range s.SideData {
		switch {
		case strings.Contains(strings.ToLower(sd.Type), "mastering display"):
			hdr.MasterDisplay = sd.x265MasterDisplay()
		case strings.Contains(strings.ToLower(sd.Type), "content light"):
			hdr.ContentLight = fmt.Sprintf("%d,%d", sd.MaxContent, sd.MaxAverage)
		}
	}
	return hdr
}

// x265MasterDisplay renders mastering-display metadata in the syntax x265
// expects: chromaticities in 0.00002 units, luminance in 0.0001 cd/m² units.
func (sd sideData) x265MasterDisplay() string {
	if sd.MaxLuminance.zero() {
		return ""
	}
	chroma := func(r rational) int { return int(math.Round(r.value() * 50000)) }
	lum := func(r rational) int { return int(math.Round(r.value() * 10000)) }
	return fmt.Sprintf("G(%d,%d)B(%d,%d)R(%d,%d)WP(%d,%d)L(%d,%d)",
		chroma(sd.GreenX), chroma(sd.GreenY),
		chroma(sd.BlueX), chroma(sd.BlueY),
		chroma(sd.RedX), chroma(sd.RedY),
		chroma(sd.WhiteX), chroma(sd.WhiteY),
		lum(sd.MaxLuminance), lum(sd.MinLuminance))
}

// rational parses ffprobe's "35400/50000" fraction strings.
type rational struct {
	Num, Den float64
}

func (r *rational) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if _, err := fmt.Sscanf(s, "%f/%f", &r.Num, &r.Den); err != nil {
		return fmt.Errorf("unexpected rational %q: %w", s, err)
	}
	return nil
}

func (r rational) zero() bool { return r.Num == 0 }

func (r rational) value() float64 {
	if r.Den == 0 {
		return 0
	}
	return r.Num / r.Den
}
