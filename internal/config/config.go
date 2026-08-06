// Package config holds the tunable knobs. Quality values live here, not in
// code, so they can be validated on real files and adjusted without edits.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DBPath         string        `toml:"db_path"`
	QuarantineRoot string        `toml:"quarantine_root"` // originals move here after a verified re-encode; never deleted
	LibraryRoots   []string      `toml:"library_roots"`   // directories the scanner sweeps
	ScanInterval   Duration      `toml:"scan_interval"`
	Workers        int           `toml:"workers"` // concurrent encodes; Quick Sync saturates around 2
	Listen         string        `toml:"listen"`  // HTTP address for webhooks and the future UI
	WebhookToken   string        `toml:"webhook_token"`
	PathMappings   []PathMapping `toml:"path_mappings"`
	Quality        Quality       `toml:"quality"`
}

// PathMapping translates paths as the dockerized *arr stack sees them into
// paths on this host, e.g. From="/data/tv" To="/mnt/media/tv".
type PathMapping struct {
	From string `toml:"from"`
	To   string `toml:"to"`
}

type Quality struct {
	HDGlobalQuality   int `toml:"hd_global_quality"`    // hevc_qsv ICQ quality (lower = better, bigger)
	UHDHDRCRF         int `toml:"uhd_hdr_crf"`          // x265 CRF for HDR 4K — lower than UHDSDRCRF: x265's RDO sees flat PQ samples and under-allocates bits otherwise
	UHDSDRCRF         int `toml:"uhd_sdr_crf"`          // x265 CRF for SDR 4K
	AV1CRF            int `toml:"av1_crf"`              // SVT-AV1 CRF for the grain-synthesis path
	AV1FilmGrainLevel int `toml:"av1_film_grain_level"` // SVT-AV1 film-grain strength (0-50)
}

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DBPath:         filepath.Join(home, ".local", "share", "mtx", "mtx.db"),
		QuarantineRoot: filepath.Join(home, "media-quarantine"),
		ScanInterval:   Duration(24 * time.Hour),
		Workers:        1,
		Listen:         "127.0.0.1:8787",
		Quality: Quality{
			// hd_global_quality=18: validated via `mtx bench` against 4
			// representative samples (old high-bitrate BluRay, grainy 1966
			// film, animated WEB-DL, modern live-action WEB-DL) — the
			// lowest-quality (most-compressed) value that keeps VMAF mean
			// >=95 on every sample. 22 measured mean as low as 92-93 on the
			// harder samples — visibly below transparent. See README
			// "Tuning quality settings".
			HDGlobalQuality:   18,
			UHDHDRCRF:         15,
			UHDSDRCRF:         19,
			AV1CRF:            30,
			AV1FilmGrainLevel: 8,
		},
	}
}

// Load reads a TOML config file over the defaults, so the file only needs
// to state what differs.
func Load(path string) (Config, error) {
	cfg := Defaults()
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("loading config %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("config %s has unknown keys (typo?): %v", path, undecoded)
	}
	return cfg, nil
}

// Duration lets the config say scan_interval = "24h" instead of nanoseconds.
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	*d = Duration(parsed)
	return err
}
