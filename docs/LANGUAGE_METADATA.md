# UMD Video language metadata notes

UMD Video stores per-clip stream metadata in `.CLP` files under `UMD_VIDEO/CLIPINF`. UMD2MKV pairs the selected movie with the matching clip-info file by basename, for example:

```text
UMD_VIDEO/STREAM/00001.MPS
UMD_VIDEO/CLIPINF/00001.CLP
```

## Tested retail descriptor layout

In the supplied retail `00001.CLP`, audio descriptors are 14-byte records beginning with MPEG `private_stream_1` (`0xBD`):

```text
BD <substream> 00 00 00 08 F0 00 <lang-2> ...
```

The sample contains:

```text
BD 00 ... en   -> audio substream 00 -> English -> eng
BD 01 ... fr   -> audio substream 01 -> French  -> fra
BD 02 ... es   -> audio substream 02 -> Spanish -> spa
```

Entries with substream IDs `0x80` and above are not treated as audio by the current parser. The sample also contains such descriptors with language codes; these are likely other private streams such as subtitle-related data and are reserved for future work.

## MKV tagging

The converter matches the reconstructed OMA track's PSP substream ID (`00`, `01`, `02`, ...) to the CLP descriptor with the same ID. It then writes both:

```text
language=eng
title=English
```

and equivalent metadata for the other languages. This stream-ID mapping is more reliable than the earlier heuristic that merely scanned for three-letter language strings.

If a CLP uses an unknown descriptor layout or an unsupported language code, that track is left unlabeled rather than guessed.
