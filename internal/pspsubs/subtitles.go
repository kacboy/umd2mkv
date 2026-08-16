package pspsubs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type Language struct {
	Code string
	Name string
}

type Track struct {
	StreamID byte
	Language Language
	Path     string
	Records  int
	Packets  int64
	Bytes    int64
}

type packetMeta struct {
	start, end int64
	pts        int64
	hasPTS     bool
}

type rawStream struct {
	id             byte
	path           string
	f              *os.File
	bytes, packets int64
	meta           []packetMeta
}

type record struct {
	startPTS int64
	duration int64
	x, y     int
	pngData  []byte
}

func decodePTS(h []byte) (int64, bool) {
	if len(h) < 14 || h[7]&0x80 == 0 || h[8] < 5 {
		return 0, false
	}
	p := h[9:14]
	pts := (int64((p[0]>>1)&0x07) << 30) |
		(int64(p[1]) << 22) |
		(int64((p[2]>>1)&0x7F) << 15) |
		(int64(p[3]) << 7) |
		int64((p[4]>>1)&0x7F)
	return pts, true
}

func findPacket(meta []packetMeta, off int64) (packetMeta, bool) {
	lo, hi := 0, len(meta)
	for lo < hi {
		mid := (lo + hi) / 2
		if meta[mid].end <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(meta) && meta[lo].start <= off && off < meta[lo].end {
		if meta[lo].hasPTS {
			return meta[lo], true
		}
		// Some continuation PES packets omit PTS. Prefer the closest prior
		// timestamp, then the next timestamp if the record begins in a packet
		// before its timestamp-bearing continuation.
		for i := lo - 1; i >= 0; i-- {
			if meta[i].hasPTS {
				return meta[i], true
			}
		}
		for i := lo + 1; i < len(meta); i++ {
			if meta[i].hasPTS {
				return meta[i], true
			}
		}
	}
	return packetMeta{}, false
}

func collectPrivateSubtitleStreams(mpsPath, outDir string, logf func(string)) (map[byte]*rawStream, error) {
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
	streams := map[byte]*rawStream{}
	closeAll := func() {
		for _, s := range streams {
			if s.f != nil {
				s.f.Close()
				s.f = nil
			}
		}
	}
	defer closeAll()

	readAt := func(off int64, n int) ([]byte, error) {
		b := make([]byte, n)
		_, e := f.ReadAt(b, off)
		return b, e
	}
	isPSCode := func(c byte) bool {
		return c == 0xBA || c == 0xB9 || c == 0xBB || c == 0xBD || c == 0xBE || c == 0xBF ||
			(c >= 0xC0 && c <= 0xDF) || (c >= 0xE0 && c <= 0xEF)
	}
	findNext := func(from int64) int64 {
		buf := make([]byte, 256*1024+3)
		pos := from
		for pos < fileSize-4 {
			want := int64(256 * 1024)
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
		return nil, fmt.Errorf("could not find MPEG program-stream packets")
	}
	lastPct := -1
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
			l := int64(14 + int(ph[13]&7))
			if off+l > fileSize {
				break
			}
			off += l
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
				pes9, e := readAt(off, 9)
				if e == nil {
					extra := int64(pes9[8])
					idpos := off + 9 + extra
					payload := idpos + 4
					if idpos < end && payload < end {
						privateHdr, er := readAt(idpos, 4)
						if er == nil && privateHdr[0] >= 0x80 && privateHdr[0] <= 0x9F {
							id := privateHdr[0]
							data, er := readAt(payload, int(end-payload))
							if er != nil && er != io.EOF {
								return nil, er
							}

							pts, hasPTS := int64(0), false
							if extra >= 5 {
								hdr, he := readAt(off, 14)
								if he == nil {
									pts, hasPTS = decodePTS(hdr)
								}
							}

							so := streams[id]
							if so == nil {
								p := filepath.Join(outDir, fmt.Sprintf("umd2mkv_sub_%02X.raw", id))
								rf, ce := os.Create(p)
								if ce != nil {
									return nil, ce
								}
								so = &rawStream{id: id, path: p, f: rf}
								streams[id] = so
							}

							start := so.bytes
							written := 0
							// PSP UMD subtitle continuation PES packets omit PTS. On those
							// packets, bytes 2..3 of the four-byte private-stream header
							// are actually the first two bytes of the continued PNG payload.
							// Initial (PTS-bearing) packets use 00 00 there as control bytes.
							// Preserving these two bytes is required for PNGs that cross a
							// PES boundary; dropping them corrupts the IDAT/IEND stream.
							if !hasPTS {
								// Only append header bytes if they contain non-zero payload data
								if privateHdr[2] != 0x00 || privateHdr[3] != 0x00 {
									w, we := so.f.Write(privateHdr[2:4])
									if we != nil {
										return nil, we
									}
									written += w
								}
							}
							w, we := so.f.Write(data)
							if we != nil {
								return nil, we
							}
							written += w
							so.meta = append(so.meta, packetMeta{start: start, end: start + int64(written), pts: pts, hasPTS: hasPTS})
							so.bytes += int64(written)
							so.packets++
						}
					}
				}
			}
			off = end
		}
		if logf != nil && fileSize > 0 {
			pct := int(off * 100 / fileSize)
			if pct >= lastPct+10 || pct == 100 {
				lastPct = pct
				logf(fmt.Sprintf("Subtitle scan: %d%%", pct))
			}
		}
	}
	closeAll()
	return streams, nil
}

func extractRecords(so *rawStream) ([]record, error) {
	data, err := os.ReadFile(so.path)
	if err != nil {
		return nil, err
	}
	sig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	var out []record
	search := 0
	for search < len(data) {
		rel := bytes.Index(data[search:], sig)
		if rel < 0 {
			break
		}
		pngPos := search + rel
		start := pngPos - 18
		search = pngPos + len(sig)
		if start < 0 || start+26 > len(data) || string(data[start+2:start+6]) != "0088" {
			continue
		}
		total := int(binary.BigEndian.Uint16(data[start:start+2])) + 2
		if total < 26 || start+total > len(data) || start+18 != pngPos {
			continue
		}
		m, ok := findPacket(so.meta, int64(start))
		if !ok {
			continue
		}
		dur := int64(binary.BigEndian.Uint32(data[start+6 : start+10]))
		if dur <= 0 {
			continue
		}
		x := int(binary.BigEndian.Uint16(data[start+12 : start+14]))
		y := int(binary.BigEndian.Uint16(data[start+14 : start+16]))
		p := append([]byte(nil), data[pngPos:start+total]...)
		out = append(out, record{startPTS: m.pts, duration: dur, x: x, y: y, pngData: p})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].startPTS < out[j].startPTS })
	return out, nil
}

func writeSeg(w io.Writer, pts uint32, typ byte, payload []byte) error {
	if len(payload) > 0xFFFF {
		return fmt.Errorf("PGS segment too large: %d bytes", len(payload))
	}
	var h [13]byte
	h[0], h[1] = 'P', 'G'
	binary.BigEndian.PutUint32(h[2:6], pts)
	binary.BigEndian.PutUint32(h[6:10], pts)
	h[10] = typ
	binary.BigEndian.PutUint16(h[11:13], uint16(len(payload)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

func rgbToYCbCr(c color.Color) (byte, byte, byte, byte) {
	rr, gg, bb, aa := c.RGBA()
	r, g, b, a := int(rr>>8), int(gg>>8), int(bb>>8), int(aa>>8)
	clamp := func(v int) byte {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return byte(v)
	}
	y := ((66*r + 129*g + 25*b + 128) >> 8) + 16
	cb := ((-38*r - 74*g + 112*b + 128) >> 8) + 128
	cr := ((112*r - 94*g - 18*b + 128) >> 8) + 128
	return clamp(y), clamp(cr), clamp(cb), byte(a)
}

func paletted(img image.Image) (*image.Paletted, error) {
	if p, ok := img.(*image.Paletted); ok {
		return p, nil
	}
	b := img.Bounds()
	p := image.NewPaletted(b, color.Palette{})
	idx := map[color.RGBA]uint8{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			n, ok := idx[c]
			if !ok {
				if len(p.Palette) >= 255 {
					return nil, fmt.Errorf("subtitle PNG uses more than 255 colors")
				}
				n = uint8(len(p.Palette))
				idx[c] = n
				p.Palette = append(p.Palette, c)
			}
			p.SetColorIndex(x, y, n)
		}
	}
	return p, nil
}

func encodeRLE(p *image.Paletted) []byte {
	b := p.Bounds()
	width, height := b.Dx(), b.Dy()
	out := make([]byte, 0, width*height/2)
	for yy := 0; yy < height; yy++ {
		row := p.Pix[yy*p.Stride : yy*p.Stride+width]
		for x := 0; x < width; {
			c := row[x]
			run := 1
			for x+run < width && row[x+run] == c && run < 0x3FFF {
				run++
			}
			if run == 1 && c != 0 {
				out = append(out, c)
			} else {
				out = append(out, 0)
				if run <= 0x3F {
					f := byte(run)
					if c != 0 {
						f |= 0x80
					}
					out = append(out, f)
				} else {
					f := byte(0x40 | ((run >> 8) & 0x3F))
					if c != 0 {
						f |= 0x80
					}
					out = append(out, f, byte(run))
				}
				if c != 0 {
					out = append(out, c)
				}
			}
			x += run
		}
		out = append(out, 0, 0)
	}
	return out
}

func makePCS(width, height int, comp uint16, state byte, object bool, x, y int) []byte {
	n := 11
	if object {
		n += 8
	}
	p := make([]byte, n)
	binary.BigEndian.PutUint16(p[0:2], uint16(width))
	binary.BigEndian.PutUint16(p[2:4], uint16(height))
	p[4] = 0x40 // 29.97-ish frame-rate code; FFmpeg treats this as informational.
	binary.BigEndian.PutUint16(p[5:7], comp)
	p[7] = state
	p[8] = 0
	p[9] = 0
	if object {
		p[10] = 1
		binary.BigEndian.PutUint16(p[11:13], 0)
		p[13] = 0
		p[14] = 0
		binary.BigEndian.PutUint16(p[15:17], uint16(x))
		binary.BigEndian.PutUint16(p[17:19], uint16(y))
	} else {
		p[10] = 0
	}
	return p
}

func writeDisplaySet(w io.Writer, rec record, comp uint16, videoW, videoH int) error {
	img, err := png.Decode(bytes.NewReader(rec.pngData))
	if err != nil {
		return err
	}
	p, err := paletted(img)
	if err != nil {
		return err
	}
	bw, bh := p.Bounds().Dx(), p.Bounds().Dy()
	if rec.x < 0 || rec.y < 0 || rec.x+bw > videoW || rec.y+bh > videoH {
		return fmt.Errorf("subtitle image %dx%d at %d,%d exceeds %dx%d frame", bw, bh, rec.x, rec.y, videoW, videoH)
	}
	pts := uint32(rec.startPTS & 0xffffffff)
	end := uint32((rec.startPTS + rec.duration) & 0xffffffff)
	pcs := makePCS(videoW, videoH, comp, 0x80, true, rec.x, rec.y)
	wds := make([]byte, 10)
	wds[0] = 1
	wds[1] = 0
	binary.BigEndian.PutUint16(wds[2:4], uint16(rec.x))
	binary.BigEndian.PutUint16(wds[4:6], uint16(rec.y))
	binary.BigEndian.PutUint16(wds[6:8], uint16(bw))
	binary.BigEndian.PutUint16(wds[8:10], uint16(bh))
	pds := []byte{0, byte(comp)}
	for i, c := range p.Palette {
		y, cr, cb, a := rgbToYCbCr(c)
		pds = append(pds, byte(i), y, cr, cb, a)
	}
	rle := encodeRLE(p)
	ods := make([]byte, 11+len(rle))
	binary.BigEndian.PutUint16(ods[0:2], 0)
	ods[2] = 0
	ods[3] = 0xC0
	dataLen := len(rle) + 4
	ods[4] = byte(dataLen >> 16)
	ods[5] = byte(dataLen >> 8)
	ods[6] = byte(dataLen)
	binary.BigEndian.PutUint16(ods[7:9], uint16(bw))
	binary.BigEndian.PutUint16(ods[9:11], uint16(bh))
	copy(ods[11:], rle)
	if err := writeSeg(w, pts, 0x16, pcs); err != nil {
		return err
	}
	if err := writeSeg(w, pts, 0x17, wds); err != nil {
		return err
	}
	if err := writeSeg(w, pts, 0x14, pds); err != nil {
		return err
	}
	if len(ods) <= 0xFFFF {
		if err := writeSeg(w, pts, 0x15, ods); err != nil {
			return err
		}
	} else {
		// Split large ODS payloads at RLE boundaries accepted by the PGS decoder.
		firstMax := 0xFFFF
		if err := writeSeg(w, pts, 0x15, ods[:firstMax]); err != nil {
			return err
		}
		rest := ods[firstMax:]
		for len(rest) > 0 {
			n := len(rest)
			if n > 0xFFFF-4 {
				n = 0xFFFF - 4
			}
			cont := make([]byte, 4+n)
			binary.BigEndian.PutUint16(cont[0:2], 0)
			cont[2] = 0
			cont[3] = 0x40
			copy(cont[4:], rest[:n])
			if err := writeSeg(w, pts, 0x15, cont); err != nil {
				return err
			}
			rest = rest[n:]
		}
	}
	if err := writeSeg(w, pts, 0x80, nil); err != nil {
		return err
	}
	clear := makePCS(videoW, videoH, comp+1, 0x00, false, 0, 0)
	if err := writeSeg(w, end, 0x16, clear); err != nil {
		return err
	}
	return writeSeg(w, end, 0x80, nil)
}

// BuildPGS extracts all UMD subtitle private streams and converts their original
// PNG artwork/timing into standard HDMV PGS .sup tracks suitable for Matroska.
func BuildPGS(mpsPath, outDir string, langs map[byte]Language, logf func(string)) ([]Track, error) {
	streams, err := collectPrivateSubtitleStreams(mpsPath, outDir, logf)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(streams))
	for id := range streams {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	tracks := make([]Track, 0, len(ids))
	for _, ii := range ids {
		so := streams[byte(ii)]
		recs, er := extractRecords(so)
		if er != nil {
			return nil, er
		}
		if len(recs) == 0 {
			continue
		}
		lang := langs[so.id]
		suffix := lang.Code
		if suffix == "" {
			suffix = "und"
		}
		out := filepath.Join(outDir, fmt.Sprintf("subtitle_stream_%02X_%s.sup", so.id, suffix))
		wf, er := os.Create(out)
		if er != nil {
			return nil, er
		}
		validFrames := 0
		for i, r := range recs {
			if er = writeDisplaySet(wf, r, uint16(i*2), 720, 480); er != nil {
				if logf != nil {
					logf(fmt.Sprintf("Warning: subtitle stream %02X frame %d skipped (%v)", so.id, i, er))
				}
				continue
			}
			validFrames++
		}

		if validFrames == 0 {
			wf.Close()
			os.Remove(out)
			return nil, fmt.Errorf("subtitle stream %02X: no valid PNG records could be rendered", so.id)
		}
		ce := wf.Close()
		if er == nil {
			er = ce
		}
		if !ok || er != nil {
			return nil, fmt.Errorf("subtitle stream %02X: %w", so.id, er)
		}
		t := Track{StreamID: so.id, Language: lang, Path: out, Records: len(recs), Packets: so.packets, Bytes: so.bytes}
		tracks = append(tracks, t)
		if logf != nil {
			logf(fmt.Sprintf("Subtitle stream %02X: %d image subtitle(s) -> %s", so.id, len(recs), filepath.Base(out)))
		}
		os.Remove(so.path)
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("no usable PSP PNG subtitle records found")
	}
	return tracks, nil
}

// Scan returns the same track inventory as BuildPGS while also validating that
// the PNG subtitle records can be decoded. It writes temporary PGS files to
// outDir; callers may remove the directory afterwards.
func Scan(mpsPath, outDir string, langs map[byte]Language, logf func(string)) ([]Track, error) {
	return BuildPGS(mpsPath, outDir, langs, logf)
}
