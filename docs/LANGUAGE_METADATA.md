# UMD Video language metadata notes

UMD Video supports multiple audio languages and stores per-clip metadata in `.CLP` files under `UMD_VIDEO/CLIPINF`.

UMD2MKV pairs the selected movie with the matching clip-info file by basename, for example:

```text
UMD_VIDEO/STREAM/00001.MPS
UMD_VIDEO/CLIPINF/00001.CLP
```

The public binary layout of commercial UMD Video CLP files is incomplete, so the implementation uses a conservative heuristic:

1. scan the matching CLP for recognized three-letter ISO-639 codes;
2. de-duplicate equivalent codes such as `fra`/`fre`;
3. retain their first-occurrence order;
4. assign those codes to PSP audio streams in substream order (`00`, `01`, `02`, ...);
5. write both Matroska `language` metadata and a human-readable track title.

For a disc whose CLP yields `eng`, `fre`, `spa` and whose MPS contains audio streams `00`, `01`, `02`, the resulting tracks are tagged English, French, and Spanish respectively.

If no language codes can be recognized, the tracks remain unlabeled. If the number of detected language codes differs from the number of reconstructed audio streams, UMD2MKV logs the mismatch and only labels tracks for which it has an ordered candidate.

This logic is intentionally easy to inspect in the conversion log so mappings can be corrected if a commercial disc uses a different CLP layout.
