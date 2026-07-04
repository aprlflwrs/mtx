# mtx

A small, self-contained media transcode daemon. It shrinks a media library by
re-encoding video to efficient codecs — safely, with a policy that knows what
it must never touch. One Go binary, one SQLite file, no other services.

Design research and rationale live in `~/docs/optimize/`.

## What it does

- **1080p-and-below** (the bulk of a TV library): hardware HEVC on Intel Quick
  Sync silicon via VAAPI — fast, cheap, typically 60–85% smaller on old
  high-bitrate rips (modest, ~10–15%, on already-efficient modern WEB-DL
  sources — there's just less bloat to remove).
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
sudo apt install ffmpeg intel-media-va-driver libmfx-gen1.2
```

`libmfx-gen1.2` (Intel VPL GPU Runtime) is required even though the HD profile
uses `hevc_vaapi`, not `hevc_qsv` — see the note below on why.

The service user needs `render` group membership (for `/dev/dri/renderD128`)
and read/write access to the library with the same group your media stack uses.

**Why `hevc_vaapi` and not `hevc_qsv`:** on Debian 13 with this box's Alder
Lake iGPU, `hevc_qsv`'s MFX/oneVPL translation layer rejected every parameter
combination outright (confirmed by hand against real ffmpeg — a driver/runtime
compatibility break, not a settings problem). `hevc_vaapi` reaches the
identical Quick Sync hardware encoder block directly via VA-API, bypassing
that broken layer — it's also what Jellyfin itself uses for hardware
transcoding, for the same reason. If a future driver/runtime update fixes the
qsv path, this is a one-profile change in `internal/encode/args.go`.

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

## Quality scoring setup (`mtx score`)

`mtx score <source> <encoded>` measures quality drift with VMAF instead of
relying on eyeballing every file — useful for validating a profile's settings
before trusting it at bulk scale (see the workflow above).

VMAF isn't packaged for Debian, and this system's ffmpeg wasn't built with
libvmaf, so `mtx score` shells out to the standalone `vmaf` CLI built from
[Netflix's libvmaf source](https://github.com/Netflix/vmaf), installed
user-locally (no root, doesn't touch the system ffmpeg):

```sh
pip3 install --user --break-system-packages meson
sudo apt install -y nasm   # ninja, gcc, pkg-config are normally already present

git clone --depth 1 https://github.com/Netflix/vmaf.git
cd vmaf/libvmaf
meson setup build --prefix="$HOME/.local" --buildtype=release
ninja -C build
ninja -C build install
```

That installs the `vmaf` binary to `~/.local/bin` and its shared library to
`~/.local/lib/x86_64-linux-gnu`; `mtx score` finds both automatically (checks
`PATH`, falls back to `~/.local/bin`, and sets `LD_LIBRARY_PATH` itself) — no
shell rc changes needed. Built-in VMAF models (`vmaf_v0.6.1` /
`vmaf_4k_v0.6.1`, chosen automatically by resolution) ship inside libvmaf
itself, so no separate model files are needed.

```sh
mtx score original.mkv encoded.mkv
# VMAF mean: 95.55 (threshold 95.00), worst frame: 91.34 (threshold 90.00) — PASS
```

The worst-frame score matters as much as the mean — a good average can hide
one badly-mangled scene. Note: HDR sources are scored after an 8-bit SDR
quantization applied identically to both files, which is valid for relative
A/B comparison (did the encode drift from its source) but not a literal
"how this looks on an HDR panel" number.

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
mtx score <source> <encoded>         measure quality drift with VMAF
```

## Not built yet, by design

Web UI and monitoring (will mount on the existing HTTP server and read the
same SQLite DB), quarantine pruning, retry policy, Prometheus metrics.
