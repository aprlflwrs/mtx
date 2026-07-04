package quality

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// generateClip writes a short synthetic clip so tests don't depend on real
// media files. brightnessShift=0 makes an identical clip (VMAF ~100);
// nonzero produces a detectable difference.
func generateClip(t *testing.T, path string, brightnessShift float64) {
	t.Helper()
	args := []string{"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10"}
	if brightnessShift != 0 {
		args = append(args, "-vf", fmt.Sprintf("eq=brightness=%f", brightnessShift))
	}
	args = append(args, "-c:v", "libx264", "-y", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg unavailable or failed generating test clip: %v\n%s", err, out)
	}
}

func requireVMAF(t *testing.T) {
	t.Helper()
	if _, err := vmafCommand(context.Background()); err != nil {
		t.Skipf("vmaf binary not available: %v", err)
	}
}

func TestCompareIdenticalClipsScoresNearMax(t *testing.T) {
	requireVMAF(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mkv")
	generateClip(t, clip, 0)

	score, err := Compare(context.Background(), clip, clip)
	if err != nil {
		t.Fatal(err)
	}
	if score.Mean < 95 {
		t.Errorf("identical clip scored %f, expected ~100", score.Mean)
	}
}

func TestCompareDetectsDegradation(t *testing.T) {
	requireVMAF(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	degraded := filepath.Join(dir, "degraded.mkv")
	generateClip(t, source, 0)
	generateClip(t, degraded, 0.3) // a large, obvious shift

	score, err := Compare(context.Background(), source, degraded)
	if err != nil {
		t.Fatal(err)
	}
	if score.Mean > 90 {
		t.Errorf("obviously degraded clip scored %f, expected a clear drop", score.Mean)
	}
	if score.Min > score.Mean {
		t.Errorf("worst-frame score (%f) should never exceed the mean (%f)", score.Min, score.Mean)
	}
}
