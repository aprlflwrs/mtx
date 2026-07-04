# mtx

A small, self-contained media transcode daemon. It shrinks a media library by
re-encoding video to efficient codecs — safely, with a policy that knows what
it must never touch. One Go binary, one SQLite file, no other services.

Design research and rationale live in `~/docs/optimize/`.

## What it does

- **1080p-and-below** (the bulk of a TV library): hardware HEVC via Intel
  Quick Sync — fast, cheap, typically 60–85% smaller on old high-bitrate rips.
- **4K SDR**: software x265 at a conservative CRF.
- **4K HDR10/HLG**: software x265 with the source's HDR metadata explicitly
  re-attached (ffmpeg drops it otherwise, producing washed-out output).
- **Dolby Vision**: never re-encoded — without the original RPU there is no
  safe way to regenerate DV metadata. Skipped, loudly.
- **Grainy films** (opt-in per enqueue): SVT-AV1 with film-grain synthesis.
- Audio, subtitles, and chapters are always copied untouched; output is MKV.
- Already-HEVC/AV1 files are skipped.

The full decision table is one readable function: `internal/policy/policy.go`.

## Safety model

Originals are **never deleted**. An encode must verify (parseable output,
duration matches the source, strictly smaller) before the original is moved
to the quarantine directory and the new file takes its place. Anything else —
failed encode, not smaller, verification mismatch — leaves the original
exactly where it was. Prune quarantine yourself once you trust the results.

## Install

Prerequisites on the host:

```sh
sudo apt install ffmpeg intel-media-va-driver-non-free   # QSV needs the iHD driver
```

The service user needs `render` group membership (for `/dev/dri/renderD128`)
and read/write access to the library with the same group your media stack uses.

Build and deploy:

```sh
go build -o mtx ./cmd/mtx
sudo install mtx /usr/local/bin/
sudo mkdir -p /etc/mtx && sudo cp deploy/config.example.toml /etc/mtx/config.toml
# edit /etc/mtx/config.toml: library roots, quarantine, path mappings, token
sudo cp deploy/mtx.service /etc/systemd/system/
sudo systemctl enable --now mtx
```

## Validate quality before a bulk run

Encode one expendable episode and eyeball the dimmest scenes (that's where
compression artifacts appear first):

```sh
mtx probe /mnt/media/tv/Seinfeld/S01E01.mkv        # shows the decision + exact ffmpeg args
mtx enqueue --now --quarantine /tmp/q /mnt/media/tv/Seinfeld/S01E01.mkv
```

Adjust `[quality]` in the config until happy, then feed it the library:

```sh
mtx enqueue /mnt/media/tv/Seinfeld/    # queued; the daemon churns through it
mtx status
```

## Wiring Sonarr/Radarr (dockerized)

The *arrs run in containers, so they call the daemon over HTTP instead of
executing scripts. In each: **Settings → Connect → + → Webhook**, URL
`http://<host>:8787/webhook/sonarr` (or `/webhook/radarr`), method POST,
password = your `webhook_token` (username anything), triggers **On Import**
and **On Upgrade**. The connection Test button should go green.

`path_mappings` in the config translate the container's view of the library
(e.g. `/data/tv`) to the host's (e.g. `/mnt/media/tv`). The periodic library
scan catches anything that arrives outside the *arrs.

## CLI

```
mtx probe <file>                     what would happen, without touching anything
mtx enqueue [--grain] <path...>      queue for the daemon (--grain: AV1 grain synthesis, SDR only)
mtx enqueue --now <path...>          transcode right here, synchronously
mtx serve [--config <file>]          the daemon: workers + scanner + HTTP
mtx status                           queue counts and total bytes saved
```

## Not built yet, by design

Web UI and monitoring (will mount on the existing HTTP server and read the
same SQLite DB), quarantine pruning, retry policy, Prometheus metrics.
