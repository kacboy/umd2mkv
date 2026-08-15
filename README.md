# UMD2MKV

UMD2MKV extracts the largest movie stream from a PSP UMD Video ISO, reconstructs its PSP ATRAC3+ audio streams, and remuxes the result to Matroska (MKV) using FFmpeg.

The video stream is copied without re-encoding. PSP ATRAC3+ audio is rebuilt into temporary OMA streams and converted to lossless FLAC for the MKV.

## Platforms

- **Windows:** native GUI (`UMD2MKV.exe`) and CLI.
- **macOS:** CLI builds for Intel and Apple Silicon.
- **Linux:** CLI builds for amd64 and arm64.

GitHub Actions builds all of these automatically on every push and pull request.

## Requirements

FFmpeg must be installed and available on `PATH`, or placed beside the executable. The converter itself has no runtime dependencies.

### Windows

Download `ffmpeg.exe` and place it beside `UMD2MKV.exe`, or add FFmpeg to PATH.

### macOS

For example with Homebrew:

```sh
brew install ffmpeg
```

### Linux

Use your distribution package manager, for example:

```sh
sudo apt install ffmpeg
```

## CLI usage

```sh
umd2mkv -iso /path/to/movie.iso -out movie.mkv
```

To inspect the ISO without converting it:

```sh
umd2mkv -iso /path/to/movie.iso -inspect
```

The inspect command reports the selected `UMD_VIDEO/STREAM/*.MPS`, its matching `CLIPINF/*.CLP` when present, and any recognizable ISO-639 language-code candidates found in the CLP metadata.

## Audio languages

Official UMD Video authoring data supports language attributes for audio streams, and those attributes are associated with clip metadata. Unfortunately the public UMD Video format documentation is incomplete, so UMD2MKV currently treats automatic language detection as **experimental**.

The CLI `-inspect` mode scans the matching `.CLP` file for recognizable ISO-639 language codes. It deliberately does **not** automatically assign those labels to MKV tracks yet, because a false mapping is worse than an unlabeled track. Once the ordering is verified against a few real discs, the detected codes can be written directly into Matroska track metadata.

## How conversion works

1. Read the ISO9660 filesystem directly; mounting/extracting the ISO first is not required.
2. Find the largest `.MPS` under `UMD_VIDEO/STREAM`.
3. Extract that stream to a temporary directory.
4. Parse the MPEG program stream and collect genuine PSP `private_stream_1` audio substreams.
5. Rebuild PSP `0F D0` ATRAC3+ sound frames into Sony EA3/OMA streams.
6. Run FFmpeg with H.264 video stream-copy and lossless FLAC audio.
7. Write the final MKV.

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

## Current limitations

- The cross-platform build is currently CLI-first. The proven Win32 GUI remains Windows-only.
- Automatic audio-language labeling is experimental.
- UMD subtitle streams are not fully decoded yet; external subtitle files found in unusual disc layouts may be muxed by the Windows GUI.
- The main feature is selected by largest `.MPS` size, which is intentionally simple and may select a long bonus feature on unusual discs.

## Legal

Use UMD2MKV with discs/images you are legally entitled to access. The project does not include FFmpeg or copyrighted disc content.
