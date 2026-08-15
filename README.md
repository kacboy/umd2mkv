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

Choose a UMD Video ISO and it is scanned automatically. The GUI selects the largest `UMD_VIDEO/STREAM/*.MPS`, shows any language metadata it can recognize, and converts it to MKV.

Audio defaults to **AAC-LC 256 kbps**. Enable **Use lossless FLAC audio (larger files)** if you want an archival/lossless intermediate transcode instead.

## CLI usage

Default AAC-LC output:

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv
```

Lossless FLAC audio:

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv -audio flac
```

Inspect the selected movie and language metadata without converting:

```sh
umd2mkv -iso /path/to/movie.iso -inspect
```

## Audio language labels

UMD Video discs can contain multiple language tracks. UMD2MKV looks for the `.CLP` file matching the selected `.MPS` (for example `STREAM/00001.MPS` -> `CLIPINF/00001.CLP`) and scans it for recognized ISO-639 language codes.

When codes are found, they are assigned to reconstructed audio tracks in on-disc order. For example, three detected codes `eng`, `fre`, `spa` applied to PSP audio substreams `00`, `01`, `02` become MKV tracks tagged as English, French, and Spanish.

This is **best-effort** because the complete commercial UMD Video CLP binary format is not publicly documented. The converter logs the detected order before FFmpeg runs. If no recognized codes are found, tracks are left unlabeled rather than guessing.

## How conversion works

1. Read the ISO9660 filesystem directly; mounting/extracting the ISO first is not required.
2. Find the largest `.MPS` under `UMD_VIDEO/STREAM`.
3. Find the corresponding `.CLP` under `UMD_VIDEO/CLIPINF` for language metadata.
4. Extract the selected MPS to a temporary directory.
5. Parse the MPEG program stream and collect genuine PSP `private_stream_1` audio substreams.
6. Rebuild PSP `0F D0` ATRAC3+ sound frames into Sony EA3/OMA streams.
7. Run FFmpeg with H.264 video stream-copy and either AAC-LC 256 kbps (default) or FLAC audio.
8. Write Matroska language/title metadata for audio tracks when CLP language codes were detected.
9. Write the final MKV.

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
- Audio-language mapping is best-effort and depends on recognizable codes in the matching CLP file.
- UMD subtitle streams are not fully decoded yet; separate recognizable subtitle files are only attempted by the Windows GUI.
- The main feature is selected by largest `.MPS` size, which can theoretically select a long bonus feature on an unusual disc.

## Legal

Use UMD2MKV with discs/images you are legally entitled to access. The project does not include FFmpeg or copyrighted disc content.
