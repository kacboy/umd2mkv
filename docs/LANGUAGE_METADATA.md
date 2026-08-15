# UMD Video language metadata notes

UMD Video authoring software exposes a language-code attribute for each audio stream, and `.CLP` files are part of the clip metadata stored under `UMD_VIDEO/CLIPINF`.

The exact public binary layout is poorly documented. For now, `umd2mkv -inspect` performs a conservative scan of the matching `.CLP` file for known ISO-639 three-letter codes. The converter does not automatically map those candidates to audio tracks until ordering has been validated across multiple commercial discs.

A useful next step is collecting the output of `-inspect` from discs where the audio menu order is known (for example English/French/Spanish). That should make it possible to correlate CLP offsets with PSP audio substream IDs 00/01/02 and implement deterministic Matroska language tags.
