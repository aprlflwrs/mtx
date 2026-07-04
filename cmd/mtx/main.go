// mtx shrinks a media library by re-encoding video to efficient codecs,
// governed by a safety-first policy (see internal/policy).
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"mtx/internal/config"
	"mtx/internal/encode"
	"mtx/internal/policy"
	"mtx/internal/probe"
)

const usage = `mtx — media transcode helper

Usage:
  mtx probe <file>                 show what would happen to a file, without touching it
  mtx enqueue --now [flags] <path...>  transcode files or directories, synchronously

Flags for enqueue:
  --now                required for now; queue/daemon mode arrives later
  --grain              opt this content into AV1 film-grain synthesis (SDR only)
  --quarantine <dir>   where originals go after a verified re-encode
`

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true, ".m2ts": true, ".wmv": true,
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "probe":
		err = probeCommand(os.Args[2:])
	case "enqueue":
		err = enqueueCommand(os.Args[2:])
	case "serve", "status":
		err = fmt.Errorf("%q is not built yet — coming with the daemon milestone", os.Args[1])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mtx:", err)
		os.Exit(1)
	}
}

func probeCommand(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: mtx probe <file>")
	}
	media, err := probe.Probe(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("file:     %s\n", media.Path)
	fmt.Printf("video:    %s, %dp, %s\n", media.VideoCodec, media.Height, describeDynamicRange(media))
	fmt.Printf("size:     %.1f MB\n", float64(media.SizeBytes)/1e6)
	fmt.Printf("duration: %s\n", media.Duration.Round(1e9))

	decision := policy.Decide(media, false)
	if !decision.ShouldTranscode() {
		fmt.Printf("decision: skip (%s)\n", decision.SkipReason)
		return nil
	}
	fmt.Printf("decision: transcode with profile %s\n", decision.Profile)

	ffmpegArgs, err := encode.Args(media, decision.Profile, config.Defaults().Quality, "<output>.mkv")
	if err != nil {
		return err
	}
	fmt.Printf("ffmpeg:   ffmpeg %s\n", strings.Join(ffmpegArgs, " "))
	return nil
}

func enqueueCommand(args []string) error {
	flags := flag.NewFlagSet("enqueue", flag.ExitOnError)
	now := flags.Bool("now", false, "process synchronously")
	grain := flags.Bool("grain", false, "opt into AV1 film-grain synthesis (SDR only)")
	quarantine := flags.String("quarantine", "", "override the quarantine directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*now {
		return fmt.Errorf("only --now is supported until the daemon milestone lands")
	}
	if flags.NArg() == 0 {
		return fmt.Errorf("usage: mtx enqueue --now [--grain] <path...>")
	}

	cfg := config.Defaults()
	if *quarantine != "" {
		cfg.QuarantineRoot = *quarantine
	}

	for _, path := range flags.Args() {
		if err := processPath(path, *grain, cfg); err != nil {
			return err
		}
	}
	return nil
}

func processPath(path string, grain bool, cfg config.Config) error {
	return filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !videoExtensions[strings.ToLower(filepath.Ext(file))] {
			return nil
		}
		report(encode.ProcessFile(file, grain, cfg))
		return nil
	})
}

func report(result encode.Result, err error) {
	switch {
	case err != nil:
		fmt.Printf("FAILED   %s: %v\n", result.Path, err)
	case result.Skipped != "":
		fmt.Printf("skipped  %s (%s)\n", result.Path, result.Skipped)
	default:
		fmt.Printf("done     %s: %.1f MB -> %.1f MB (%.0f%% smaller), original in %s\n",
			result.FinalPath,
			float64(result.SizeBefore)/1e6, float64(result.SizeAfter)/1e6,
			result.PercentSmaller(), result.Quarantined)
	}
}

func describeDynamicRange(m probe.MediaInfo) string {
	switch {
	case m.DolbyVision:
		return "Dolby Vision"
	case m.IsHDR():
		return "HDR (" + m.ColorTransfer + ")"
	default:
		return "SDR"
	}
}
