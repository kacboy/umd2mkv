# UMD2MKV

UMD2MKV extracts the largest movie stream from a PSP UMD Video ISO, reconstructs its PSP ATRAC3+ audio streams, and remuxes the result to Matroska (MKV) using FFmpeg.

The H.264 video stream is copied without re-encoding. By default, ATRAC3+ audio is converted to **AAC-LC at 256 kbps per track** for broad playback compatibility and a much smaller file than lossless FLAC. A lossless FLAC mode remains available.

## Platforms

- **Windows:** native GUI (`UMD2MKV.exe`) and CLI.
- **macOS:** CLI builds for Intel and Apple Silicon.
- **Linux:** CLI builds for amd64 and arm64.

GitHub Actions builds all of these automatically on every push, pull request, or manual workflow run.

## Requirements

FFmpeg must be installed and available on `PATH`, or placed beside the executable. The converter itself has no runtime dependencies.

### Windows

Place `ffmpeg.exe` beside `UMD2MKV.exe`, or add FFmpeg to PATH. The GUI checks for FFmpeg at startup and has a **Recheck** button.

### macOS

```sh
brew install ffmpeg
```

### Linux

For example:

```sh
sudo apt install ffmpeg
```

## Windows GUI

Choose a UMD Video ISO and it is scanned automatically. The GUI selects the largest `UMD_VIDEO/STREAM/*.MPS` and shows the audio/subtitle language descriptors found in its matching `.CLP`.

Selecting an ISO automatically scans the disc and shows the detected movie, audio languages, and subtitle languages before conversion. There is no separate scan step in the GUI.

Audio output is selectable before conversion:

- **AAC 256k (recommended):** AAC-LC at 256 kbps per track for broad compatibility and smaller output.
- **FLAC (lossless):** lossless conversion of the decoded ATRAC3+ audio, faster on some systems but much larger.

**Include UMD subtitles (PGS)** is enabled by default. UMD2MKV preserves the original PNG subtitle artwork, timing, placement, and CLP language labels as selectable PGS tracks in the MKV.

## CLI usage

Default AAC-LC output:

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv
```

Lossless FLAC audio:

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv -audio flac
```

Inspect the selected movie and CLP language metadata without converting:

```sh
umd2mkv -iso /path/to/movie.iso -inspect
```

Deep-scan the actual audio/subtitle streams without converting:

```sh
umd2mkv -iso /path/to/movie.iso -scan-tracks
```

Disable UMD subtitles if desired:

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv -subs=false
```

## Audio language labels

UMD Video discs can contain multiple language tracks. UMD2MKV looks for the `.CLP` file matching the selected `.MPS` (for example `STREAM/00001.MPS` -> `CLIPINF/00001.CLP`) and parses its PSP private-stream descriptors directly. Audio descriptors identify both the PSP audio substream ID and a two-letter ISO-639 language code.

For the tested retail layout, `BD 00 ... en`, `BD 01 ... fr`, and `BD 02 ... es` map directly to audio substreams `00`, `01`, and `02`. The converter writes standard Matroska language tags (`eng`, `fra`, `spa`) plus readable track titles (`English`, `French`, `Spanish`). The mapping is by **stream ID**, not just by order.

Unknown or unsupported descriptors are left unlabeled rather than guessed. The converter logs each mapping before FFmpeg runs.

## How conversion works

1. Read the ISO9660 filesystem directly; mounting/extracting the ISO first is not required.
2. Find the largest `.MPS` under `UMD_VIDEO/STREAM`.
3. Find the corresponding `.CLP` under `UMD_VIDEO/CLIPINF` for language metadata.
4. Extract the selected MPS to a temporary directory.
5. Parse the MPEG program stream and collect genuine PSP `private_stream_1` audio substreams.
6. Rebuild PSP `0F D0` ATRAC3+ sound frames into Sony EA3/OMA streams.
7. Run FFmpeg with H.264 video stream-copy and either AAC-LC 256 kbps (default) or FLAC audio.
8. Extract PSP `0x80`-`0x9F` private subtitle streams, reassemble PNG data across continuation PES packets, preserve PTS/duration/position, and convert the original artwork to PGS `.sup` tracks.
9. Write Matroska language/title metadata for audio and subtitle tracks when CLP language codes were detected.
10. Write the final MKV.

## Building locally

Requires Go 1.22 or newer.

### CLI

```sh
go build ./cmd/umd2mkv
```

### Windows GUI

```powershell
go build -ldflags="-H=windowsgui" -o UMD2MKV.exe ./cmd/umd2mkv-gui
```

## GitHub Actions builds

The included `.github/workflows/build.yml` produces downloadable artifacts for:

- Windows GUI (`UMD2MKV.exe`)
- Windows CLI
- Linux amd64 / arm64
- macOS Intel / Apple Silicon

Open **Actions -> build** in GitHub after pushing the repo, or use **Run workflow** manually.

## Current limitations

- The cross-platform build is CLI-first; the Win32 GUI is Windows-only.
- Audio-language mapping supports the retail CLP descriptor layout tested so far; unusual CLP variants may remain unlabeled.
- UMD subtitle support is based on the retail PSP PNG/private-stream layout tested so far; unusual discs may use a variant that is not recognized.
- The main feature is selected by largest `.MPS` size, which can theoretically select a long bonus feature on an unusual disc.

## Legal

Use UMD2MKV with discs/images you are legally entitled to access. The project does not include FFmpeg or copyrighted disc content.
