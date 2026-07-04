// mtx shrinks a media library by re-encoding video to efficient codecs,
// governed by a safety-first policy (see internal/policy).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"mtx/internal/analyze"
	"mtx/internal/config"
	"mtx/internal/daemon"
	"mtx/internal/encode"
	"mtx/internal/policy"
	"mtx/internal/probe"
	"mtx/internal/queue"
	"mtx/internal/scan"
	"mtx/internal/server"
)

const usage = `mtx — media transcode helper

Usage:
  mtx probe <file>                     show what would happen to a file, without touching it
  mtx analyze [path...]                codec/resolution/HDR/decision breakdown of a library
  mtx enqueue [flags] <path...>        queue files or directories for the daemon
  mtx enqueue --now [flags] <path...>  transcode them right here, synchronously
  mtx serve [--config <file>]          run the daemon: workers + periodic library scan
  mtx status [--config <file>]         queue and savings summary

Flags for enqueue:
  --now                bypass the queue and process synchronously
  --grain              opt this content into AV1 film-grain synthesis (SDR only)
  --quarantine <dir>   override where originals go (with --now)
  --config <file>      config file (default /etc/mtx/config.toml if it exists)

Flags for analyze:
  --config <file>      config file (default /etc/mtx/config.toml if it exists)
                        paths default to the config's library_roots when omitted
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "probe":
		err = probeCommand(os.Args[2:])
	case "analyze":
		err = analyzeCommand(os.Args[2:])
	case "enqueue":
		err = enqueueCommand(os.Args[2:])
	case "serve":
		err = serveCommand(os.Args[2:])
	case "status":
		err = statusCommand(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mtx:", err)
		os.Exit(1)
	}
}

const defaultConfigPath = "/etc/mtx/config.toml"

// loadConfig applies the config file when one is named or the default one
// exists; otherwise built-in defaults apply.
func loadConfig(path string) (config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	if _, err := os.Stat(defaultConfigPath); err == nil {
		return config.Load(defaultConfigPath)
	}
	return config.Defaults(), nil
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

func analyzeCommand(args []string) error {
	flags := flag.NewFlagSet("analyze", flag.ExitOnError)
	configPath := flags.String("config", "", "config file")
	if err := flags.Parse(args); err != nil {
		return err
	}

	roots := flags.Args()
	if len(roots) == 0 {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		if len(cfg.LibraryRoots) == 0 {
			return fmt.Errorf("usage: mtx analyze [path...] (or set library_roots in config)")
		}
		roots = cfg.LibraryRoots
	}

	report, err := analyze.Walk(roots)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mtx: scan warnings:", err)
	}
	printAnalysis(report)
	return nil
}

func printAnalysis(r analyze.Report) {
	fmt.Printf("%d files, %.1f GB\n\n", r.Files, float64(r.Bytes)/1e9)
	printBuckets("codec", r.ByCodec)
	printBuckets("resolution", r.ByResolution)
	printBuckets("dynamic range", r.ByDynamicRange)
	printBuckets("decision", r.ByDecision)
	if len(r.Errors) > 0 {
		fmt.Printf("%d file(s) could not be probed:\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Printf("  %v\n", e)
		}
	}
}

func printBuckets(label string, buckets map[string]analyze.Bucket) {
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return buckets[keys[i]].Bytes > buckets[keys[j]].Bytes })

	fmt.Printf("%s:\n", label)
	for _, k := range keys {
		b := buckets[k]
		fmt.Printf("  %-24s %5d files  %8.1f GB\n", k, b.Files, float64(b.Bytes)/1e9)
	}
	fmt.Println()
}

func enqueueCommand(args []string) error {
	flags := flag.NewFlagSet("enqueue", flag.ExitOnError)
	now := flags.Bool("now", false, "process synchronously instead of queueing")
	grain := flags.Bool("grain", false, "opt into AV1 film-grain synthesis (SDR only)")
	quarantine := flags.String("quarantine", "", "override the quarantine directory")
	configPath := flags.String("config", "", "config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return fmt.Errorf("usage: mtx enqueue [--now] [--grain] <path...>")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *quarantine != "" {
		cfg.QuarantineRoot = *quarantine
	}

	if *now {
		return processNow(flags.Args(), *grain, cfg)
	}
	return addToQueue(flags.Args(), *grain, cfg)
}

func processNow(paths []string, grain bool, cfg config.Config) error {
	ctx := interruptibleContext()
	return eachVideoFile(paths, func(file string) error {
		if ctx.Err() != nil {
			return nil // interrupted: skip the remaining files quietly
		}
		result, err := encode.ProcessFile(ctx, file, grain, cfg)
		report(result, err)
		return nil
	})
}

func addToQueue(paths []string, grain bool, cfg config.Config) error {
	store, err := openQueue(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	queued := 0
	err = eachVideoFile(paths, func(file string) error {
		inserted, err := store.Enqueue(file, grain)
		if inserted {
			queued++
		}
		return err
	})
	fmt.Printf("queued %d file(s)\n", queued)
	return err
}

func serveCommand(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", "", "config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := openQueue(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	log.Info("mtx daemon starting", "workers", cfg.Workers, "roots", cfg.LibraryRoots, "listen", cfg.Listen)

	web := &http.Server{
		Addr:    cfg.Listen,
		Handler: (&server.Server{Config: cfg, Store: store, Log: log}).Handler(),
	}
	go func() {
		if err := web.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "error", err)
		}
	}()

	d := &daemon.Daemon{Config: cfg, Store: store, Log: log}
	d.Run(interruptibleContext())

	web.Shutdown(context.Background())
	log.Info("mtx daemon stopped")
	return nil
}

func statusCommand(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := flags.String("config", "", "config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := openQueue(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	summary, err := store.Summarize()
	if err != nil {
		return err
	}
	for _, status := range []string{"pending", "running", "done", "skipped", "failed"} {
		fmt.Printf("%-8s %d\n", status, summary.CountByStatus[status])
	}
	fmt.Printf("saved    %.1f GB\n", float64(summary.BytesSaved)/1e9)
	return nil
}

func openQueue(cfg config.Config) (*queue.Store, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return nil, err
	}
	return queue.Open(cfg.DBPath)
}

func eachVideoFile(paths []string, visit func(file string) error) error {
	return scan.Walk(paths, visit)
}

func interruptibleContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx
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
