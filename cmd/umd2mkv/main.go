package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kacboy/umd2mkv/internal/pspsubs"
)

const sectorSize = 2048

type IsoEntry struct {
	Path   string
	Name   string
	Size   uint32
	Extent uint32
	IsDir  bool
}

type ScanResult struct {
	Movie    IsoEntry
	ClipInfo *IsoEntry
	Subs     []IsoEntry
}

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func readDirRecord(data []byte, pos int) (IsoEntry, int, error) {
	if pos >= len(data) {
		return IsoEntry{}, pos, io.EOF
	}
	l := int(data[pos])
	if l == 0 {
		return IsoEntry{}, ((pos / sectorSize) + 1) * sectorSize, nil
	}
	if pos+l > len(data) || l < 34 {
		return IsoEntry{}, pos, fmt.Errorf("invalid ISO directory record")
	}
	rec := data[pos : pos+l]
	extent := binary.LittleEndian.Uint32(rec[2:6])
	size := binary.LittleEndian.Uint32(rec[10:14])
	flags := rec[25]
	nameLen := int(rec[32])
	if 33+nameLen > len(rec) {
		return IsoEntry{}, pos, fmt.Errorf("invalid ISO filename")
	}
	nameBytes := rec[33 : 33+nameLen]
	name := string(nameBytes)
	if nameLen == 1 && nameBytes[0] == 0 {
		name = "."
	}
	if nameLen == 1 && nameBytes[0] == 1 {
		name = ".."
	}
	return IsoEntry{Name: name, Size: size, Extent: extent, IsDir: flags&2 != 0}, pos + l, nil
}

func scanISO(filename string) (ScanResult, error) {
	f, err := os.Open(filename)
	if err != nil {
		return ScanResult{}, err
	}
	defer f.Close()
	pvd := make([]byte, sectorSize)
	if _, err = f.ReadAt(pvd, 16*sectorSize); err != nil {
		return ScanResult{}, fmt.Errorf("read ISO volume descriptor: %w", err)
	}
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return ScanResult{}, fmt.Errorf("not an ISO9660 image")
	}
	root, _, err := readDirRecord(pvd, 156)
	if err != nil {
		return ScanResult{}, err
	}
	var entries []IsoEntry
	seen := map[uint32]bool{}
	var walk func(IsoEntry, string) error
	walk = func(dir IsoEntry, parent string) error {
		if seen[dir.Extent] {
			return nil
		}
		seen[dir.Extent] = true
		b := make([]byte, int(dir.Size))
		if _, err := f.ReadAt(b, int64(dir.Extent)*sectorSize); err != nil && err != io.EOF {
			return err
		}
		for pos := 0; pos < len(b); {
			e, next, er := readDirRecord(b, pos)
			if er == io.EOF {
				break
			}
			if er != nil {
				return er
			}
			if next <= pos {
				break
			}
			pos = next
			if e.Name == "" || e.Name == "." || e.Name == ".." {
				continue
			}
			clean := strings.Split(e.Name, ";")[0]
			e.Path = strings.TrimRight(parent, "/") + "/" + clean
			e.Name = clean
			if e.IsDir {
				if er := walk(e, e.Path); er != nil {
					return er
				}
			} else {
				entries = append(entries, e)
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return ScanResult{}, err
	}
	var movies, subs []IsoEntry
	subExt := map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".sup": true, ".idx": true}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name))
		up := strings.ToUpper(strings.ReplaceAll(e.Path, "\\", "/"))
		if ext == ".mps" && strings.Contains(up, "/UMD_VIDEO/STREAM/") {
			movies = append(movies, e)
		}
		if subExt[ext] {
			subs = append(subs, e)
		}
	}
	if len(movies) == 0 {
		return ScanResult{}, fmt.Errorf("no .MPS movie stream found under UMD_VIDEO/STREAM")
	}
	sort.Slice(movies, func(i, j int) bool { return movies[i].Size > movies[j].Size })
	movie := movies[0]
	stem := strings.TrimSuffix(movie.Name, filepath.Ext(movie.Name))
	var clip *IsoEntry
	for i := range entries {
		e := entries[i]
		up := strings.ToUpper(strings.ReplaceAll(e.Path, "\\", "/"))
		if strings.Contains(up, "/UMD_VIDEO/CLIPINF/") && strings.EqualFold(strings.TrimSuffix(e.Name, filepath.Ext(e.Name)), stem) && strings.EqualFold(filepath.Ext(e.Name), ".CLP") {
			x := e
			clip = &x
			break
		}
	}
	return ScanResult{Movie: movie, ClipInfo: clip, Subs: subs}, nil
}

func extractEntryProgress(isoPath string, e IsoEntry, out string, progress func(int)) error {
	in, err := os.Open(isoPath)
	if err != nil {
		return err
	}
	defer in.Close()
	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	defer dst.Close()
	r := io.NewSectionReader(in, int64(e.Extent)*sectorSize, int64(e.Size))
	buf := make([]byte, 4*1024*1024)
	var copied int64
	last := -1
	for copied < int64(e.Size) {
		n, er := r.Read(buf)
		if n > 0 {
			if _, ew := dst.Write(buf[:n]); ew != nil {
				return ew
			}
			copied += int64(n)
			if progress != nil && e.Size > 0 {
				pct := int(copied * 100 / int64(e.Size))
				if pct >= last+10 || pct == 100 {
					last = pct
					progress(pct)
				}
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return er
		}
	}
	return nil
}

func findFFmpeg() string {
	exe, _ := os.Executable()
	names := []string{"ffmpeg", "ffmpeg.exe"}
	for _, n := range names {
		p := filepath.Join(filepath.Dir(exe), n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func runFFmpeg(args []string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

func demuxAtracFromMPS(mpsPath, outDir string) ([]string, error) {
	f, err := os.Open(mpsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()
	type atracOut struct {
		id             byte
		rawPath        string
		file           *os.File
		bytes, packets int64
	}
	streams := map[byte]*atracOut{}
	closeAll := func() {
		for _, s := range streams {
			if s.file != nil {
				s.file.Close()
				s.file = nil
			}
		}
	}
	defer closeAll()
	readAt := func(off int64, n int) ([]byte, error) { b := make([]byte, n); _, e := f.ReadAt(b, off); return b, e }
	isPSCode := func(c byte) bool {
		return c == 0xBA || c == 0xB9 || c == 0xBB || c == 0xBD || c == 0xBE || c == 0xBF || (c >= 0xC0 && c <= 0xDF) || (c >= 0xE0 && c <= 0xEF)
	}
	findNext := func(from int64) int64 {
		const chunk = 256 * 1024
		buf := make([]byte, chunk+3)
		pos := from
		for pos < fileSize-4 {
			want := int64(chunk)
			if fileSize-pos < want {
				want = fileSize - pos
			}
			n, e := f.ReadAt(buf[:want], pos)
			if e != nil && e != io.EOF {
				return -1
			}
			for i := 0; i+3 < n; i++ {
				if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 && isPSCode(buf[i+3]) {
					return pos + int64(i)
				}
			}
			if n <= 3 {
				break
			}
			pos += int64(n - 3)
		}
		return -1
	}
	off := findNext(0)
	if off < 0 {
		return nil, fmt.Errorf("could not find MPEG program-stream packet")
	}
	last := -1
	for off >= 0 && off+4 <= fileSize {
		h4, e := readAt(off, 4)
		if e != nil {
			break
		}
		if h4[0] != 0 || h4[1] != 0 || h4[2] != 1 || !isPSCode(h4[3]) {
			off = findNext(off + 1)
			continue
		}
		code := h4[3]
		if code == 0xB9 {
			break
		}
		if code == 0xBA {
			ph, e := readAt(off, 14)
			if e != nil {
				break
			}
			off += int64(14 + int(ph[13]&7))
		} else {
			h6, e := readAt(off, 6)
			if e != nil {
				break
			}
			plen := int64(binary.BigEndian.Uint16(h6[4:6]))
			if plen <= 0 || off+6+plen > fileSize {
				off = findNext(off + 4)
				continue
			}
			end := off + 6 + plen
			if code == 0xBD && plen >= 8 {
				pes, e := readAt(off, 9)
				if e == nil {
					extra := int64(pes[8])
					sidPos := off + 9 + extra
					payloadStart := sidPos + 4
					if sidPos < end && payloadStart < end {
						idb, er := readAt(sidPos, 1)
						if er == nil && idb[0] < 0x20 {
							id := idb[0]
							payload, er := readAt(payloadStart, int(end-payloadStart))
							if er != nil && er != io.EOF {
								return nil, er
							}
							so := streams[id]
							if so == nil {
								rp := filepath.Join(outDir, fmt.Sprintf("audio_stream_%02X.pspaudio", id))
								rf, ce := os.Create(rp)
								if ce != nil {
									return nil, ce
								}
								so = &atracOut{id: id, rawPath: rp, file: rf}
								streams[id] = so
							}
							w, we := so.file.Write(payload)
							if we != nil {
								return nil, we
							}
							so.bytes += int64(w)
							so.packets++
						}
					}
				}
			}
			off = end
		}
		pct := int(off * 100 / fileSize)
		if pct >= last+10 {
			last = pct
			logf("Audio scan: %d%%", pct)
		}
	}
	closeAll()
	if len(streams) == 0 {
		return nil, fmt.Errorf("no PSP ATRAC3+ private-stream audio packets found")
	}
	var max int64
	for _, s := range streams {
		if s.bytes > max {
			max = s.bytes
		}
	}
	min := max / 20
	if min < 256*1024 {
		min = 256 * 1024
	}
	ids := make([]int, 0, len(streams))
	for id := range streams {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var outs []string
	for _, idi := range ids {
		s := streams[byte(idi)]
		if s.bytes < min {
			logf("Ignoring short private stream %02X (%d bytes)", s.id, s.bytes)
			continue
		}
		logf("PSP audio stream %02X: %d PES packets, %d bytes", s.id, s.packets, s.bytes)
		out := filepath.Join(outDir, fmt.Sprintf("audio_%02d_stream_%02X.oma", len(outs), s.id))
		frames, resync, cfg, er := convertPSPAudioToOMA(s.rawPath, out)
		if er != nil {
			logf("Rejecting audio stream %02X: %v", s.id, er)
			continue
		}
		logf("Rebuilt stream %02X: %d ATRAC frames, %d resync(s), config %04X", s.id, frames, resync, cfg)
		outs = append(outs, out)
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("no valid full-length OMA audio stream could be rebuilt")
	}
	return outs, nil
}

func convertPSPAudioToOMA(rawPath, outPath string) (frames, resyncs int, config uint16, err error) {
	data, err := os.ReadFile(rawPath)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(data) < 16 {
		return 0, 0, 0, fmt.Errorf("audio stream too short")
	}
	valid := func(pos int, want uint16, enforce bool) bool {
		if pos < 0 || pos+8 > len(data) || data[pos] != 0x0F || data[pos+1] != 0xD0 {
			return false
		}
		cfg := binary.BigEndian.Uint16(data[pos+2 : pos+4])
		if enforce && cfg != want {
			return false
		}
		sr := (cfg >> 13) & 7
		ch := (cfg >> 10) & 7
		block := int(cfg&0x03FF)*8 + 8
		return sr <= 4 && ch >= 1 && ch <= 7 && block >= 16 && block <= 8192
	}
	first := -1
	for i := 0; i+8 <= len(data); i++ {
		if valid(i, 0, false) {
			first = i
			break
		}
	}
	if first < 0 {
		return 0, 0, 0, fmt.Errorf("no valid 0F D0 sound-frame header")
	}
	if first > 0 {
		resyncs++
	}
	config = binary.BigEndian.Uint16(data[first+2 : first+4])
	block := int(config&0x03FF)*8 + 8
	dst, err := os.Create(outPath)
	if err != nil {
		return 0, 0, 0, err
	}
	defer dst.Close()
	if _, err = dst.Write(makeOMAHeader(data[first+2], data[first+3])); err != nil {
		return 0, 0, 0, err
	}
	pos := first
	for pos+8 <= len(data) {
		if !valid(pos, config, true) {
			found := -1
			limit := pos + block + 4096
			if limit > len(data)-8 {
				limit = len(data) - 8
			}
			for i := pos + 1; i <= limit; i++ {
				if valid(i, config, true) {
					found = i
					break
				}
			}
			if found < 0 {
				break
			}
			pos = found
			resyncs++
			continue
		}
		start := pos + 8
		next := start + block
		if next > len(data) {
			break
		}
		if _, err = dst.Write(data[start:next]); err != nil {
			return frames, resyncs, config, err
		}
		frames++
		if next+8 <= len(data) && valid(next, config, true) {
			pos = next
			continue
		}
		found := -1
		lo := next - 64
		if lo < start {
			lo = start
		}
		hi := next + 4096
		if hi > len(data)-8 {
			hi = len(data) - 8
		}
		for i := lo; i <= hi; i++ {
			if valid(i, config, true) {
				found = i
				break
			}
		}
		if found < 0 {
			break
		}
		if found != next {
			resyncs++
		}
		pos = found
	}
	if frames < 100 {
		return frames, resyncs, config, fmt.Errorf("only %d valid ATRAC frames reconstructed", frames)
	}
	return frames, resyncs, config, nil
}

func makeOMAHeader(c1, c2 byte) []byte {
	h := make([]byte, 96)
	binary.BigEndian.PutUint32(h[0:4], 0x45413301)
	binary.BigEndian.PutUint16(h[4:6], 96)
	binary.BigEndian.PutUint16(h[6:8], 0xFFFF)
	binary.BigEndian.PutUint32(h[12:16], 0x010F5000)
	binary.BigEndian.PutUint32(h[16:20], 0x00040000)
	binary.BigEndian.PutUint32(h[20:24], 0x0000F5CE)
	binary.BigEndian.PutUint32(h[24:28], 0xD2929132)
	binary.BigEndian.PutUint32(h[28:32], 0x2480451C)
	h[32] = 1
	h[34] = c1
	h[35] = c2
	return h
}

type LanguageInfo struct {
	StreamID byte
	Code     string
	Name     string
}

var iso6392 = map[string]LanguageInfo{
	"en": {Code: "eng", Name: "English"},
	"fr": {Code: "fra", Name: "French"},
	"es": {Code: "spa", Name: "Spanish"},
	"de": {Code: "deu", Name: "German"},
	"it": {Code: "ita", Name: "Italian"},
	"ja": {Code: "jpn", Name: "Japanese"},
	"pt": {Code: "por", Name: "Portuguese"},
	"nl": {Code: "nld", Name: "Dutch"},
	"ru": {Code: "rus", Name: "Russian"},
	"ko": {Code: "kor", Name: "Korean"},
	"zh": {Code: "zho", Name: "Chinese"},
	"ar": {Code: "ara", Name: "Arabic"},
	"pl": {Code: "pol", Name: "Polish"},
	"sv": {Code: "swe", Name: "Swedish"},
	"no": {Code: "nor", Name: "Norwegian"},
	"da": {Code: "dan", Name: "Danish"},
	"fi": {Code: "fin", Name: "Finnish"},
	"cs": {Code: "ces", Name: "Czech"},
	"hu": {Code: "hun", Name: "Hungarian"},
	"tr": {Code: "tur", Name: "Turkish"},
}

// detectCLPLanguages parses the UMD Video clip descriptor table.  Audio
// descriptors use private_stream_1 (0xBD), followed by the PSP substream ID.
// In the tested retail CLP layout the two-byte ISO-639-1 language code is at
// descriptor offsets +8/+9.  Audio substreams are 0x00-0x1F; 0x80+ entries
// describe other private streams (commonly subtitles) and are intentionally
// not returned here.
func detectCLPLanguages(isoPath string, clip *IsoEntry) []LanguageInfo {
	if clip == nil {
		return nil
	}
	tmp, err := os.CreateTemp("", "umd2mkv-clp-*.bin")
	if err != nil {
		return nil
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if extractEntryProgress(isoPath, *clip, name, nil) != nil {
		return nil
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	var out []LanguageInfo
	seen := map[byte]bool{}
	for i := 0; i+14 <= len(b); i++ {
		if b[i] != 0xBD {
			continue
		}
		streamID := b[i+1]
		if streamID >= 0x20 || seen[streamID] {
			continue
		}
		// Retail UMD Video CLP stream descriptors observed here are 14 bytes:
		// BD <id> 00 00 00 08 F0 00 <lang2> ...
		if b[i+2] != 0x00 || b[i+3] != 0x00 || b[i+4] != 0x00 || b[i+5] != 0x08 || b[i+6] != 0xF0 || b[i+7] != 0x00 {
			continue
		}
		raw := strings.ToLower(string(b[i+8 : i+10]))
		info, ok := iso6392[raw]
		if !ok {
			continue
		}
		info.StreamID = streamID
		seen[streamID] = true
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StreamID < out[j].StreamID })
	return out
}

func languageForAudioFile(path string, langs []LanguageInfo) (LanguageInfo, bool) {
	base := strings.ToUpper(filepath.Base(path))
	for _, l := range langs {
		needle := fmt.Sprintf("_STREAM_%02X", l.StreamID)
		if strings.Contains(base, needle) {
			return l, true
		}
	}
	return LanguageInfo{}, false
}

func languageSummary(langs []LanguageInfo) string {
	parts := make([]string, 0, len(langs))
	for _, l := range langs {
		parts = append(parts, fmt.Sprintf("%02X=%s (%s)", l.StreamID, l.Code, l.Name))
	}
	return strings.Join(parts, ", ")
}

func detectCLPPrivateLanguages(isoPath string, clip *IsoEntry) map[byte]LanguageInfo {
	out := map[byte]LanguageInfo{}
	if clip == nil {
		return out
	}
	tmp, err := os.CreateTemp("", "umd2mkv-clp-*.bin")
	if err != nil {
		return out
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if extractEntryProgress(isoPath, *clip, name, nil) != nil {
		return out
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return out
	}
	for i := 0; i+14 <= len(b); i++ {
		if b[i] != 0xBD {
			continue
		}
		id := b[i+1]
		if id < 0x80 || id > 0x9F {
			continue
		}
		if b[i+2] != 0 || b[i+3] != 0 || b[i+4] != 0 || b[i+5] != 0x08 || b[i+6] != 0xF0 || b[i+7] != 0 {
			continue
		}
		raw := strings.ToLower(string(b[i+8 : i+10]))
		if info, ok := iso6392[raw]; ok {
			info.StreamID = id
			out[id] = info
		}
	}
	return out
}

func subtitleLanguageMap(in map[byte]LanguageInfo) map[byte]pspsubs.Language {
	out := map[byte]pspsubs.Language{}
	for id, li := range in {
		out[id] = pspsubs.Language{Code: li.Code, Name: li.Name}
	}
	return out
}

func main() {
	iso := flag.String("iso", "", "path to PSP UMD Video ISO")
	out := flag.String("out", "", "output MKV path")
	inspect := flag.Bool("inspect", false, "scan ISO metadata and print detected CLP track labels")
	scanTracks := flag.Bool("scan-tracks", false, "deep-scan the selected MPS and report actual audio/subtitle tracks without creating an MKV")
	audioMode := flag.String("audio", "aac", "audio output: aac (default, AAC-LC 256k) or flac")
	includeSubs := flag.Bool("subs", true, "include original UMD image subtitles as selectable PGS tracks")
	flag.Parse()
	if *iso == "" {
		fmt.Fprintln(os.Stderr, "usage: umd2mkv -iso movie.iso [-out movie.mkv] [-audio aac|flac] [-subs=true|false] [-inspect] [-scan-tracks]")
		os.Exit(2)
	}
	r, err := scanISO(*iso)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}
	logf("Movie: %s (%d bytes)", r.Movie.Path, r.Movie.Size)
	langs := detectCLPLanguages(*iso, r.ClipInfo)
	if len(langs) > 0 {
		logf("Audio tracks (CLP): %s", languageSummary(langs))
	}
	priv := detectCLPPrivateLanguages(*iso, r.ClipInfo)
	if len(priv) > 0 {
		ids := make([]int, 0, len(priv))
		for id := range priv {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		for _, ii := range ids {
			li := priv[byte(ii)]
			logf("Subtitle %02X (CLP): %s [%s]", ii, li.Name, li.Code)
		}
	}
	if *inspect && !*scanTracks {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(*audioMode))
	if mode != "aac" && mode != "flac" {
		fmt.Fprintln(os.Stderr, "-audio must be aac or flac")
		os.Exit(2)
	}
	tmp, err := os.MkdirTemp("", "umd2mkv-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	movie := filepath.Join(tmp, "movie.mps")
	logf("Extracting movie stream...")
	if err = extractEntryProgress(*iso, r.Movie, movie, func(p int) { logf("Movie extraction: %d%%", p) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logf("Scanning/rebuilding PSP ATRAC3+ audio...")
	audio, err := demuxAtracFromMPS(movie, tmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *scanTracks {
		logf("Actual audio tracks: %d", len(audio))
		for _, a := range audio {
			if li, ok := languageForAudioFile(a, langs); ok {
				logf("  Audio %02X: %s [%s]", li.StreamID, li.Name, li.Code)
			} else {
				logf("  Audio: %s", filepath.Base(a))
			}
		}
		tracks, serr := pspsubs.Scan(movie, tmp, subtitleLanguageMap(priv), func(s string) { logf("%s", s) })
		if serr != nil {
			logf("Subtitle scan: %v", serr)
		} else {
			logf("Actual subtitle tracks: %d", len(tracks))
			for _, t := range tracks {
				name, code := "Unknown", "und"
				if t.Language.Name != "" {
					name = t.Language.Name
				}
				if t.Language.Code != "" {
					code = t.Language.Code
				}
				logf("  Subtitle %02X: %s [%s], %d images", t.StreamID, name, code, t.Records)
			}
		}
		return
	}
	ff := findFFmpeg()
	if ff == "" {
		fmt.Fprintln(os.Stderr, "ffmpeg not found; install it or place it beside the executable")
		os.Exit(1)
	}
	if *out == "" {
		*out = strings.TrimSuffix(*iso, filepath.Ext(*iso)) + ".mkv"
	}
	var subTracks []pspsubs.Track
	if *includeSubs {
		logf("Extracting original UMD PNG subtitles as PGS...")
		subTracks, err = pspsubs.BuildPGS(movie, tmp, subtitleLanguageMap(priv), func(s string) { logf("%s", s) })
		if err != nil {
			logf("Subtitle extraction skipped: %v", err)
			subTracks = nil
		}
	}
	args := []string{ff, "-y", "-hide_banner", "-loglevel", "warning", "-i", movie}
	for _, a := range audio {
		args = append(args, "-i", a)
	}
	for _, t := range subTracks {
		args = append(args, "-i", t.Path)
	}
	maps := []string{"-map", "0:v:0"}
	for i := range audio {
		maps = append(maps, "-map", fmt.Sprintf("%d:a:0", i+1))
	}
	for i := range subTracks {
		maps = append(maps, "-map", fmt.Sprintf("%d:s:0", 1+len(audio)+i))
	}
	args = append(args, maps...)
	for i, audioFile := range audio {
		if lang, ok := languageForAudioFile(audioFile, langs); ok {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+lang.Code, fmt.Sprintf("-metadata:s:a:%d", i), "title="+lang.Name)
			logf("Audio stream %02X -> %s [%s]", lang.StreamID, lang.Name, lang.Code)
		}
	}
	for i, t := range subTracks {
		code, name := "und", fmt.Sprintf("UMD Subtitle %02X", t.StreamID)
		if t.Language.Code != "" {
			code = t.Language.Code
		}
		if t.Language.Name != "" {
			name = t.Language.Name
		}
		args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+code, fmt.Sprintf("-metadata:s:s:%d", i), "title="+name)
	}
	args = append(args, "-c:v", "copy")
	if mode == "flac" {
		args = append(args, "-c:a", "flac")
	} else {
		args = append(args, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "256k")
	}
	if len(subTracks) > 0 {
		args = append(args, "-c:s", "copy")
	}
	args = append(args, *out)
	logf("Muxing %d audio track(s) and %d subtitle track(s)...", len(audio), len(subTracks))
	txt, err := runFFmpeg(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, txt)
		os.Exit(1)
	}
	logf("Done: %s", *out)
}
