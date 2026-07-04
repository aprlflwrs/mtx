// Package config holds the tunable knobs. Quality values live here, not in
// code, so they can be validated on real files and adjusted without edits.
// File loading (TOML) arrives with the daemon; the defaults below are the
// researched starting points from docs/optimize.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	QuarantineRoot string // originals move here after a verified re-encode; never deleted
	Quality        Quality
}

type Quality struct {
	HDGlobalQuality   int // hevc_qsv ICQ quality (lower = better, bigger)
	UHDHDRCRF         int // x265 CRF for HDR 4K — conservative: artifacts show easily in HDR
	UHDSDRCRF         int // x265 CRF for SDR 4K
	AV1CRF            int // SVT-AV1 CRF for the grain-synthesis path
	AV1FilmGrainLevel int // SVT-AV1 film-grain strength (0-50)
}

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		QuarantineRoot: filepath.Join(home, "media-quarantine"),
		Quality: Quality{
			HDGlobalQuality:   22,
			UHDHDRCRF:         18,
			UHDSDRCRF:         19,
			AV1CRF:            30,
			AV1FilmGrainLevel: 8,
		},
	}
}
