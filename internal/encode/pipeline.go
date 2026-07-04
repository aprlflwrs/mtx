package encode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mtx/internal/config"
	"mtx/internal/policy"
	"mtx/internal/probe"
)

// Progress reports how far a single file's encode has gotten, for callers
// that want to render a progress bar. Speed is ffmpeg's own "Nx" realtime
// multiplier (e.g. "17.3x"), printed as ffmpeg reports it.
type Progress struct {
	FractionDone float64
	Speed        string
}

type Result struct {
	Path        string
	Profile     policy.Profile // empty when the file was left untouched
	Skipped     string         // why the file was left untouched; empty when transcoded
	SizeBefore  int64
	SizeAfter   int64
	FinalPath   string // the new file that replaced the original
	Quarantined string // where the original was moved
}

func (r Result) PercentSmaller() float64 {
	if r.SizeBefore == 0 {
		return 0
	}
	return 100 * (1 - float64(r.SizeAfter)/float64(r.SizeBefore))
}

// ProcessFile runs one file through the full pipeline:
// probe → policy decision → encode to a temp file → verify → swap the
// original into quarantine and the new file into its place.
//
// The original is never deleted; a failed or unprofitable encode leaves it
// exactly where it was. Canceling ctx kills a running ffmpeg. onProgress, if
// non-nil, is called periodically while the encode runs; it may be called
// from a different goroutine than the caller.
func ProcessFile(ctx context.Context, path string, grainRequested bool, cfg config.Config, onProgress func(Progress)) (Result, error) {
	media, err := probe.Probe(path)
	if err != nil {
		return Result{Path: path}, err
	}

	decision := policy.Decide(media, grainRequested)
	if !decision.ShouldTranscode() {
		return Result{Path: path, Skipped: string(decision.SkipReason)}, nil
	}

	tempOutput := strings.TrimSuffix(path, filepath.Ext(path)) + ".mtx-tmp.mkv"
	defer os.Remove(tempOutput) // no-op after a successful swap; cleanup otherwise

	args, err := Args(media, decision.Profile, cfg.Quality, tempOutput)
	if err != nil {
		return Result{Path: path}, err
	}
	if err := runFFmpeg(ctx, args, media.Duration, onProgress); err != nil {
		return Result{Path: path}, err
	}

	encoded, err := verify(media, tempOutput)
	if err != nil {
		return Result{Path: path}, fmt.Errorf("verification failed, original kept: %w", err)
	}
	if encoded.SizeBytes >= media.SizeBytes {
		return Result{Path: path, Skipped: "not-smaller"}, nil
	}

	quarantined, err := quarantineOriginal(path, cfg.QuarantineRoot)
	if err != nil {
		return Result{Path: path}, err
	}
	finalPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".mkv"
	if err := os.Rename(tempOutput, finalPath); err != nil {
		return Result{Path: path}, fmt.Errorf(
			"encoded file could not replace original (original is at %s): %w", quarantined, err)
	}

	return Result{
		Path:        path,
		Profile:     decision.Profile,
		SizeBefore:  media.SizeBytes,
		SizeAfter:   encoded.SizeBytes,
		FinalPath:   finalPath,
		Quarantined: quarantined,
	}, nil
}

func runFFmpeg(ctx context.Context, args []string, duration time.Duration, onProgress func(Progress)) error {
	// -progress pipe:1 makes ffmpeg emit periodic key=value lines (out_time_us,
	// speed, progress=continue|end) instead of its usual human-readable
	// stats, so we can parse real progress without scraping the stderr banner.
	progressArgs := append([]string{"-v", "warning", "-progress", "pipe:1"}, args...)
	cmd := exec.CommandContext(ctx, "ffmpeg", progressArgs...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	watchProgress(stdout, duration, onProgress)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, lastLines(stderr.String(), 10))
	}
	return nil
}

// watchProgress reads ffmpeg's -progress output until it closes and reports
// each update through onProgress (a no-op when onProgress is nil).
func watchProgress(stdout io.Reader, duration time.Duration, onProgress func(Progress)) {
	scanner := bufio.NewScanner(stdout)
	var outTime time.Duration
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		switch key {
		case "out_time_us":
			if microseconds, err := strconv.ParseInt(value, 10, 64); err == nil {
				outTime = time.Duration(microseconds) * time.Microsecond
			}
		case "speed":
			if onProgress != nil {
				onProgress(Progress{FractionDone: fractionDone(outTime, duration), Speed: strings.TrimSpace(value)})
			}
		}
	}
}

func fractionDone(elapsed, total time.Duration) float64 {
	if total <= 0 {
		return 0
	}
	fraction := elapsed.Seconds() / total.Seconds()
	return min(fraction, 1)
}

// verify probes the freshly encoded file and confirms it is a plausible
// replacement: it parses, and its duration matches the source within a second.
func verify(source probe.MediaInfo, encodedPath string) (probe.MediaInfo, error) {
	encoded, err := probe.Probe(encodedPath)
	if err != nil {
		return probe.MediaInfo{}, err
	}
	durationDrift := (source.Duration - encoded.Duration).Abs()
	if durationDrift > time.Second {
		return probe.MediaInfo{}, fmt.Errorf(
			"duration drifted %v (source %v, encoded %v)", durationDrift, source.Duration, encoded.Duration)
	}
	return encoded, nil
}

// quarantineOriginal moves the original under the quarantine root, mirroring
// its full path so nothing can collide and restores are unambiguous.
func quarantineOriginal(path, quarantineRoot string) (string, error) {
	destination := filepath.Join(quarantineRoot, strings.TrimPrefix(path, string(filepath.Separator)))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := moveFile(path, destination); err != nil {
		return "", fmt.Errorf("quarantining original: %w", err)
	}
	return destination, nil
}

// moveFile renames when possible and falls back to copy+remove when the
// quarantine root is on a different filesystem.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
