package encode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mtx/internal/config"
	"mtx/internal/policy"
	"mtx/internal/probe"
)

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
// exactly where it was. Canceling ctx kills a running ffmpeg.
func ProcessFile(ctx context.Context, path string, grainRequested bool, cfg config.Config) (Result, error) {
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
	if err := runFFmpeg(ctx, args); err != nil {
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

func runFFmpeg(ctx context.Context, args []string) error {
	quietArgs := append([]string{"-v", "warning"}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", quietArgs...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, lastLines(stderr.String(), 10))
	}
	return nil
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
