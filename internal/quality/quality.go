// Package quality measures how much an encode drifted from its source using
// VMAF, so "does this look right" is a number you can threshold and log
// instead of something only caught by watching every file.
//
// VMAF isn't packaged for Debian and this system's ffmpeg wasn't built with
// libvmaf, so this shells out to the standalone `vmaf` CLI (built from
// Netflix's libvmaf source — see README "Quality scoring setup") rather than
// an ffmpeg filter. Both files are decoded to raw y4m via the system ffmpeg
// and streamed into `vmaf` through named pipes, so nothing large is ever
// written to disk.
//
// Caveat: HDR (10-bit PQ) sources are decoded to plain 8-bit yuv420p for
// scoring, the same as SDR. VMAF's models are trained on SDR viewing
// conditions, and this isn't a real tone-map — it's a like-for-like
// quantization applied to both the source and encoded clip, so it's valid
// for relative A/B comparison (did this encode drift from its source) but
// the absolute score shouldn't be read as "how this looks on an HDR panel."
package quality

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"mtx/internal/probe"
)

type Score struct {
	Mean float64 // overall VMAF score across all frames
	Min  float64 // worst single frame — a bad mean can hide one ruined scene
}

// Compare decodes source and encoded to y4m and scores encoded against
// source with VMAF. The model is chosen by the source's resolution, mirroring
// the same HD/UHD split the encode policy uses.
func Compare(ctx context.Context, sourcePath, encodedPath string) (Score, error) {
	source, err := probe.Probe(sourcePath)
	if err != nil {
		return Score{}, fmt.Errorf("probing source for model selection: %w", err)
	}
	model := "vmaf_v0.6.1"
	if source.IsUHD() {
		model = "vmaf_4k_v0.6.1"
	}

	workDir, err := os.MkdirTemp("", "mtx-vmaf-*")
	if err != nil {
		return Score{}, err
	}
	defer os.RemoveAll(workDir)

	refPipe := filepath.Join(workDir, "ref.y4m")
	distPipe := filepath.Join(workDir, "dist.y4m")
	resultFile := filepath.Join(workDir, "result.json")
	for _, pipe := range []string{refPipe, distPipe} {
		if err := mkfifo(pipe); err != nil {
			return Score{}, fmt.Errorf("creating pipe %s: %w", pipe, err)
		}
	}

	decodeSource := decodeToY4M(ctx, sourcePath, refPipe)
	decodeEncoded := decodeToY4M(ctx, encodedPath, distPipe)
	if err := decodeSource.Start(); err != nil {
		return Score{}, fmt.Errorf("decoding source: %w", err)
	}
	if err := decodeEncoded.Start(); err != nil {
		return Score{}, fmt.Errorf("decoding encoded: %w", err)
	}

	vmafCmd, err := vmafCommand(ctx,
		"-r", refPipe, "-d", distPipe,
		"-m", "version="+model,
		"--threads", strconv.Itoa(runtime.NumCPU()),
		"--subsample", "5", // every 5th frame is standard practice and plenty for a sanity check, not a research paper
		"--json", "-o", resultFile, "-q",
	)
	if err != nil {
		return Score{}, err
	}
	vmafErr := vmafCmd.Run()

	sourceErr := decodeSource.Wait()
	encodedErr := decodeEncoded.Wait()
	if vmafErr != nil {
		return Score{}, fmt.Errorf("vmaf: %w (source decode: %v, encoded decode: %v)", vmafErr, sourceErr, encodedErr)
	}

	return parseResult(resultFile)
}

func decodeToY4M(ctx context.Context, input, outputPipe string) *exec.Cmd {
	return exec.CommandContext(ctx, "ffmpeg",
		"-v", "error", "-y",
		"-i", input,
		"-pix_fmt", "yuv420p",
		// Real (non-synthetic) sources often carry chroma_location metadata
		// that makes ffmpeg tag the y4m header C420paldv instead of the
		// standard C420mpeg2/C420jpeg. This libvmaf build's y4m reader
		// segfaults on that tag — confirmed by hand against real 1080p
		// content. Forcing standard left-sited chroma sidesteps it entirely.
		"-vf", "setparams=chroma_location=left",
		"-f", "yuv4mpegpipe", outputPipe,
	)
}

// vmafCommand locates the `vmaf` binary — checking PATH first, then the
// standard user-local build location — and arranges for it to find its
// shared library without requiring the caller's shell to have that set up.
func vmafCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	binary, err := exec.LookPath("vmaf")
	if err != nil {
		home, _ := os.UserHomeDir()
		fallback := filepath.Join(home, ".local", "bin", "vmaf")
		if _, statErr := os.Stat(fallback); statErr != nil {
			return nil, fmt.Errorf(
				"vmaf binary not found on PATH or at %s — see README \"Quality scoring setup\"", fallback)
		}
		binary = fallback
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+libvmafLibDir()+":"+os.Getenv("LD_LIBRARY_PATH"))
	return cmd, nil
}

func libvmafLibDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "lib", "x86_64-linux-gnu")
}

func mkfifo(path string) error {
	return exec.Command("mkfifo", path).Run()
}

func parseResult(path string) (Score, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Score{}, err
	}
	var result struct {
		PooledMetrics struct {
			VMAF struct {
				Mean float64 `json:"mean"`
				Min  float64 `json:"min"`
			} `json:"vmaf"`
		} `json:"pooled_metrics"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Score{}, fmt.Errorf("parsing vmaf output: %w", err)
	}
	return Score{Mean: result.PooledMetrics.VMAF.Mean, Min: result.PooledMetrics.VMAF.Min}, nil
}
