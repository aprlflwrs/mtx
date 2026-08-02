package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mtx/internal/config"
	"mtx/internal/encode"
	"mtx/internal/policy"
	"mtx/internal/probe"
	"mtx/internal/quality"
)

// qualityKnob names the config.Quality field a profile's bench sweep varies,
// so bench can report the same label the config file uses.
type qualityKnob struct {
	name string
	get  func(config.Quality) int
	set  func(*config.Quality, int)
}

var knobByProfile = map[policy.Profile]qualityKnob{
	policy.HDQuickSync: {
		name: "hd_global_quality",
		get:  func(q config.Quality) int { return q.HDGlobalQuality },
		set:  func(q *config.Quality, v int) { q.HDGlobalQuality = v },
	},
	policy.UHDSDRx265: {
		name: "uhd_sdr_crf",
		get:  func(q config.Quality) int { return q.UHDSDRCRF },
		set:  func(q *config.Quality, v int) { q.UHDSDRCRF = v },
	},
	policy.UHDHDRx265: {
		name: "uhd_hdr_crf",
		get:  func(q config.Quality) int { return q.UHDHDRCRF },
		set:  func(q *config.Quality, v int) { q.UHDHDRCRF = v },
	},
	policy.AV1FilmGrain: {
		name: "av1_crf",
		get:  func(q config.Quality) int { return q.AV1CRF },
		set:  func(q *config.Quality, v int) { q.AV1CRF = v },
	},
}

type benchRow struct {
	value        int
	encodedBytes int64
	pctSmaller   float64 // bitrate-based, so it's meaningful with --clip too
	elapsed      time.Duration
	speed        string // ffmpeg's own "Nx" realtime readout
	vmaf         quality.Score
	vmafScored   bool
	verdict      string
}

func benchCommand(args []string) error {
	flags := flag.NewFlagSet("bench", flag.ExitOnError)
	values := flags.String("values", "", "comma-separated quality values to sweep, e.g. \"18,20,22,24\" (required)")
	clip := flags.Duration("clip", 0, "encode/score only this much of each file, e.g. 3m (default: whole file)")
	grain := flags.Bool("grain", false, "opt into AV1 film-grain synthesis (SDR only)")
	skipVMAF := flags.Bool("skip-vmaf", false, "skip VMAF scoring (faster iteration on size/time alone)")
	keepOutputs := flags.String("keep-outputs", "", "directory to save encoded outputs into, instead of discarding them")
	csvPath := flags.String("csv", "", "append results as CSV rows to this file")
	configPath := flags.String("config", "", "base config; only the swept knob is overridden per run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 || *values == "" {
		return fmt.Errorf("usage: mtx bench --values <csv> [flags] <file...>")
	}

	ints, err := parseInts(*values)
	if err != nil {
		return fmt.Errorf("--values: %w", err)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *keepOutputs != "" {
		if err := os.MkdirAll(*keepOutputs, 0o755); err != nil {
			return err
		}
	}

	var csvFile *os.File
	if *csvPath != "" {
		csvFile, err = openCSV(*csvPath)
		if err != nil {
			return err
		}
		defer csvFile.Close()
	}

	ctx := interruptibleContext()
	for _, file := range flags.Args() {
		if ctx.Err() != nil {
			break
		}
		if err := benchFile(ctx, file, ints, *clip, *grain, !*skipVMAF, *keepOutputs, cfg, csvFile); err != nil {
			fmt.Fprintf(os.Stderr, "mtx bench: %s: %v\n", file, err)
		}
	}
	return nil
}

func benchFile(ctx context.Context, file string, values []int, clip time.Duration, grain, scoreVMAF bool, keepOutputs string, cfg config.Config, csvFile *os.File) error {
	media, err := probe.Probe(file)
	if err != nil {
		return err
	}
	decision := policy.Decide(media, grain)
	if !decision.ShouldTranscode() {
		fmt.Printf("%s: skip (%s)\n", file, decision.SkipReason)
		return nil
	}
	knob, ok := knobByProfile[decision.Profile]
	if !ok {
		return fmt.Errorf("no bench knob defined for profile %s", decision.Profile)
	}

	effectiveDuration := media.Duration
	if clip > 0 && clip < media.Duration {
		effectiveDuration = clip
	}

	// A clipped encode must be scored against a same-length reference, not
	// the full source, or VMAF sees mismatched frame counts. One cheap
	// stream-copy up front (no re-encode) covers every value in the sweep.
	referencePath := file
	if scoreVMAF && effectiveDuration < media.Duration {
		clipped, cleanup, err := clipSource(ctx, file, effectiveDuration)
		if err != nil {
			return fmt.Errorf("clipping reference for VMAF: %w", err)
		}
		defer cleanup()
		referencePath = clipped
	}

	fmt.Printf("\n%s — profile %s, %dp, %.1f GB, %s", file, decision.Profile, media.Height,
		float64(media.SizeBytes)/1e9, media.Duration.Round(time.Second))
	if clip > 0 {
		fmt.Printf(" [clip %s]", effectiveDuration.Round(time.Second))
	}
	fmt.Println()

	rows := make([]benchRow, 0, len(values))
	for _, v := range values {
		if ctx.Err() != nil {
			break
		}
		row, err := benchOne(ctx, media, decision.Profile, knob, v, effectiveDuration, referencePath, scoreVMAF, keepOutputs, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s=%d: %v\n", knob.name, v, err)
			continue
		}
		rows = append(rows, row)
	}

	printBenchTable(knob.name, rows, scoreVMAF)
	if effectiveDuration < media.Duration {
		fmt.Println("  (--clip active: size is the clip's own output; % smaller is projected from bitrate)")
	}
	if csvFile != nil {
		writeCSVRows(csvFile, file, decision.Profile, knob.name, effectiveDuration, rows)
	}
	return nil
}

func benchOne(ctx context.Context, media probe.MediaInfo, profile policy.Profile, knob qualityKnob, value int, effectiveDuration time.Duration, referencePath string, scoreVMAF bool, keepOutputs string, cfg config.Config) (benchRow, error) {
	q := cfg.Quality
	knob.set(&q, value)

	workDir, err := os.MkdirTemp("", "mtx-bench-*")
	if err != nil {
		return benchRow{}, err
	}
	defer os.RemoveAll(workDir)
	tempOutput := filepath.Join(workDir, "out.mkv")

	ffmpegArgs, err := encode.Args(media, profile, q, tempOutput)
	if err != nil {
		return benchRow{}, err
	}
	if effectiveDuration < media.Duration {
		ffmpegArgs = insertClipFlag(ffmpegArgs, effectiveDuration)
	}

	var lastSpeed string
	start := time.Now()
	if err := encode.RunFFmpeg(ctx, ffmpegArgs, effectiveDuration, func(p encode.Progress) { lastSpeed = p.Speed }); err != nil {
		return benchRow{}, err
	}
	elapsed := time.Since(start)

	encoded, err := probe.Probe(tempOutput)
	if err != nil {
		return benchRow{}, fmt.Errorf("probing encoded output: %w", err)
	}

	row := benchRow{
		value:        value,
		encodedBytes: encoded.SizeBytes,
		pctSmaller:   pctSmallerByBitrate(media, encoded),
		elapsed:      elapsed,
		speed:        lastSpeed,
	}

	if scoreVMAF {
		score, err := quality.Compare(ctx, referencePath, tempOutput)
		if err != nil {
			return benchRow{}, fmt.Errorf("scoring: %w", err)
		}
		row.vmaf = score
		row.vmafScored = true
		row.verdict = "PASS"
		if score.Mean < transparentMeanVMAF || score.Min < transparentMinVMAF {
			row.verdict = "FAIL"
		}
	}

	if keepOutputs != "" {
		dst := filepath.Join(keepOutputs, fmt.Sprintf("%s-%s%d.mkv",
			strings.TrimSuffix(filepath.Base(media.Path), filepath.Ext(media.Path)), knob.name, value))
		if err := copyFile(tempOutput, dst); err != nil {
			return benchRow{}, fmt.Errorf("saving output: %w", err)
		}
	}

	return row, nil
}

// pctSmallerByBitrate compares average bitrate rather than raw byte counts,
// so results stay meaningful whether or not --clip trimmed the encode.
func pctSmallerByBitrate(source, encoded probe.MediaInfo) float64 {
	if source.SizeBytes == 0 || source.Duration <= 0 || encoded.Duration <= 0 {
		return 0
	}
	sourceBitrate := float64(source.SizeBytes) / source.Duration.Seconds()
	encodedBitrate := float64(encoded.SizeBytes) / encoded.Duration.Seconds()
	return 100 * (1 - encodedBitrate/sourceBitrate)
}

// insertClipFlag limits ffmpeg's output to duration, splicing "-t <seconds>"
// in as an output option right before the destination path (the last
// argument encode.Args returns).
func insertClipFlag(args []string, duration time.Duration) []string {
	dst := args[len(args)-1]
	rest := args[:len(args)-1]
	return append(append(append([]string{}, rest...), "-t", strconv.FormatFloat(duration.Seconds(), 'f', 2, 64)), dst)
}

// clipSource stream-copies (no re-encode) the first duration of src into a
// temp file, for use as a same-length VMAF reference against clipped
// encodes. The returned cleanup func removes it.
func clipSource(ctx context.Context, src string, duration time.Duration) (path string, cleanup func(), err error) {
	workDir, err := os.MkdirTemp("", "mtx-bench-ref-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(workDir) }
	dst := filepath.Join(workDir, "ref.mkv")

	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y",
		"-i", src, "-t", strconv.FormatFloat(duration.Seconds(), 'f', 2, 64),
		"-c", "copy", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("ffmpeg: %w\n%s", err, out)
	}
	return dst, cleanup, nil
}

func printBenchTable(knobName string, rows []benchRow, scoredVMAF bool) {
	if scoredVMAF {
		fmt.Printf("  %-18s %12s %10s %8s %8s %10s %10s %s\n",
			knobName, "size", "vs-source", "time", "speed", "vmaf-mean", "vmaf-min", "verdict")
	} else {
		fmt.Printf("  %-18s %12s %10s %8s %8s\n", knobName, "size", "vs-source", "time", "speed")
	}
	for _, r := range rows {
		if scoredVMAF {
			fmt.Printf("  %-18d %9.1f MB %9.0f%% %8s %8s %10.2f %10.2f %s\n",
				r.value, float64(r.encodedBytes)/1e6, r.pctSmaller, r.elapsed.Round(time.Second), r.speed,
				r.vmaf.Mean, r.vmaf.Min, r.verdict)
		} else {
			fmt.Printf("  %-18d %9.1f MB %9.0f%% %8s %8s\n",
				r.value, float64(r.encodedBytes)/1e6, r.pctSmaller, r.elapsed.Round(time.Second), r.speed)
		}
	}
}

func openCSV(path string) (*os.File, error) {
	existed := true
	if _, err := os.Stat(path); os.IsNotExist(err) {
		existed = false
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if !existed {
		fmt.Fprintln(f, "file,profile,knob,clip_seconds,value,encoded_bytes,pct_smaller,elapsed_seconds,speed,vmaf_mean,vmaf_min,verdict")
	}
	return f, nil
}

func writeCSVRows(f *os.File, file string, profile policy.Profile, knobName string, clip time.Duration, rows []benchRow) {
	for _, r := range rows {
		vmafMean, vmafMin, verdict := "", "", ""
		if r.vmafScored {
			vmafMean = strconv.FormatFloat(r.vmaf.Mean, 'f', 2, 64)
			vmafMin = strconv.FormatFloat(r.vmaf.Min, 'f', 2, 64)
			verdict = r.verdict
		}
		fmt.Fprintf(f, "%s,%s,%s,%.2f,%d,%d,%.2f,%.2f,%s,%s,%s,%s\n",
			csvEscape(file), profile, knobName, clip.Seconds(), r.value, r.encodedBytes, r.pctSmaller,
			r.elapsed.Seconds(), r.speed, vmafMean, vmafMin, verdict)
	}
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func parseInts(csv string) ([]int, error) {
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", p)
		}
		out = append(out, v)
	}
	return out, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = out.ReadFrom(in)
	return err
}
