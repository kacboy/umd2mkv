//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/kacboy/umd2mkv/internal/pspsubs"
)

const (
	sectorSize = 2048
	appTitle   = "UMD2MKV"

	WS_OVERLAPPEDWINDOW  = 0x00CF0000
	WS_VISIBLE           = 0x10000000
	WS_CHILD             = 0x40000000
	WS_BORDER            = 0x00800000
	WS_TABSTOP           = 0x00010000
	WS_VSCROLL           = 0x00200000
	ES_AUTOHSCROLL       = 0x0080
	ES_MULTILINE         = 0x0004
	ES_AUTOVSCROLL       = 0x0040
	ES_READONLY          = 0x0800
	BS_PUSHBUTTON        = 0x00000000
	BS_DEFPUSHBUTTON     = 0x00000001
	BS_AUTOCHECKBOX      = 0x00000003
	BS_AUTORADIOBUTTON   = 0x00000009
	WS_GROUP             = 0x00020000
	SS_LEFT              = 0x00000000
	SS_RIGHT             = 0x00000002
	CW_USEDEFAULT        = 0x80000000
	SW_SHOW              = 5
	WM_DESTROY           = 0x0002
	WM_COMMAND           = 0x0111
	WM_CLOSE             = 0x0010
	WM_APP_UI            = 0x8001
	BM_GETCHECK          = 0x00F0
	BM_SETCHECK          = 0x00F1
	BST_CHECKED          = 1
	EM_SETSEL            = 0x00B1
	EM_REPLACESEL        = 0x00C2
	EM_SCROLLCARET       = 0x00B7
	MB_OK                = 0x00000000
	MB_ICONERROR         = 0x00000010
	MB_ICONINFORMATION   = 0x00000040
	OFN_FILEMUSTEXIST    = 0x00001000
	OFN_PATHMUSTEXIST    = 0x00000800
	OFN_OVERWRITEPROMPT  = 0x00000002
	OFN_EXPLORER         = 0x00080000
	STARTF_USESHOWWINDOW = 0x00000001
	SW_HIDE              = 0
)

const (
	idISOEdit    = 101
	idISOBrowse  = 102
	idOutEdit    = 103
	idOutBrowse  = 104
	idFFRecheck  = 106
	idConvert    = 108
	idSubs       = 109
	idSelection  = 110
	idLog        = 111
	idAAC        = 112
	idFLAC       = 113
	idScanTracks = 114
	idTVMode     = 115
	idAllStreams = 116
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pMessageBoxW      = user32.NewProc("MessageBoxW")
	pGetWindowTextW   = user32.NewProc("GetWindowTextW")
	pSetWindowTextW   = user32.NewProc("SetWindowTextW")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pPostMessageW     = user32.NewProc("PostMessageW")
	pEnableWindow     = user32.NewProc("EnableWindow")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
	pGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
)

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type POINT struct{ X, Y int32 }
type MSG struct {
	Hwnd     syscall.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       POINT
	LPrivate uint32
}

type OPENFILENAME struct {
	LStructSize       uint32
	HwndOwner         syscall.Handle
	HInstance         syscall.Handle
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        unsafe.Pointer
	DwReserved        uint32
	FlagsEx           uint32
}

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
	Streams  []IsoEntry
	Clips    []IsoEntry
	Subs     []IsoEntry
}

var hwndMain, hISO, hOut, hSelection, hLog, hConvert, hSubs, hAAC, hFLAC, hTVMode, hAllStreams, hFFStatus, hScanTracks syscall.Handle
var converting atomic.Bool
var uiQueue = make(chan func(), 256)

func utf16p(s string) *uint16 { return syscall.StringToUTF16Ptr(s) }

func loword(v uintptr) uint16 { return uint16(v & 0xffff) }

func createControl(class, text string, style uint32, x, y, w, h int32, parent syscall.Handle, id int) syscall.Handle {
	r, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(utf16p(class))), uintptr(unsafe.Pointer(utf16p(text))), uintptr(style|WS_CHILD|WS_VISIBLE), uintptr(x), uintptr(y), uintptr(w), uintptr(h), uintptr(parent), uintptr(id), 0, 0)
	return syscall.Handle(r)
}

func getText(h syscall.Handle) string {
	buf := make([]uint16, 32768)
	n, _, _ := pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

func setText(h syscall.Handle, s string) {
	pSetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(utf16p(s))))
}

func appendLog(s string) {
	if hLog == 0 {
		return
	}
	// Append directly to the edit control instead of replacing all text.
	// Replacing the whole buffer makes the control jump back to the top.
	text := s
	if getText(hLog) != "" {
		text = "\r\n" + text
	}
	pSendMessageW.Call(uintptr(hLog), EM_SETSEL, ^uintptr(0), ^uintptr(0))
	u := syscall.StringToUTF16(text)
	pSendMessageW.Call(uintptr(hLog), EM_REPLACESEL, 0, uintptr(unsafe.Pointer(&u[0])))
	pSendMessageW.Call(uintptr(hLog), EM_SCROLLCARET, 0, 0)
}

func message(text string, flags uintptr) {
	pMessageBoxW.Call(uintptr(hwndMain), uintptr(unsafe.Pointer(utf16p(text))), uintptr(unsafe.Pointer(utf16p(appTitle))), flags)
}

// Worker goroutines never call Win32 controls directly. They queue a closure
// and wake the UI thread with a private window message.
func postUI(fn func()) {
	uiQueue <- fn
	pPostMessageW.Call(uintptr(hwndMain), WM_APP_UI, 0, 0)
}

func postLog(s string) {
	postUI(func() { appendLog(s) })
}

func postMessage(text string, flags uintptr) {
	postUI(func() { message(text, flags) })
}

func drainUIQueue() {
	for {
		select {
		case fn := <-uiQueue:
			fn()
		default:
			return
		}
	}
}

func enableControls(v bool) {
	n := uintptr(0)
	if v {
		n = 1
	}
	pEnableWindow.Call(uintptr(hConvert), n)
	pEnableWindow.Call(uintptr(hTVMode), n)
	pEnableWindow.Call(uintptr(hAllStreams), n)
}

func animateConvertButton() {
	frames := []string{"|", "/", "-", "\\"}
	i := 0
	for converting.Load() {
		frame := frames[i%len(frames)]
		postUI(func() {
			if converting.Load() {
				setText(hConvert, "Converting "+frame)
			}
		})
		i++
		time.Sleep(160 * time.Millisecond)
	}
}

func updateFFmpegStatus(logResult bool) string {
	p := findFFmpeg()
	if p == "" {
		setText(hFFStatus, "FFmpeg: NOT FOUND - put ffmpeg.exe beside UMD2MKV.exe or add it to PATH")
		if logResult {
			appendLog("FFmpeg check: not found.")
		}
		return ""
	}
	setText(hFFStatus, "FFmpeg: detected - "+p)
	if logResult {
		appendLog("FFmpeg check: detected at " + p)
	}
	return p
}

func browseFile(title, filter string, save bool, initial string) string {
	buf := make([]uint16, 32768)
	if initial != "" {
		copy(buf, syscall.StringToUTF16(initial))
	}
	// OPENFILENAME filters use embedded NUL separators. syscall.StringToUTF16
	// deliberately panics on strings containing NUL, so build the UTF-16
	// buffer manually instead.
	filterParts := strings.Split(filter, "|")
	f16 := make([]uint16, 0, len(filter)+2)
	for _, part := range filterParts {
		u := syscall.StringToUTF16(part)
		f16 = append(f16, u...)
	}
	f16 = append(f16, 0)
	t16 := syscall.StringToUTF16(title)
	ofn := OPENFILENAME{LStructSize: uint32(unsafe.Sizeof(OPENFILENAME{})), HwndOwner: hwndMain, LpstrFilter: &f16[0], LpstrFile: &buf[0], NMaxFile: uint32(len(buf)), LpstrTitle: &t16[0], Flags: OFN_EXPLORER | OFN_PATHMUSTEXIST}
	var r uintptr
	if save {
		ofn.Flags |= OFN_OVERWRITEPROMPT
		def := syscall.StringToUTF16("mkv")
		ofn.LpstrDefExt = &def[0]
		r, _, _ = pGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	} else {
		ofn.Flags |= OFN_FILEMUSTEXIST
		r, _, _ = pGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	}
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func wndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_APP_UI:
		drainUIQueue()
		return 0
	case WM_COMMAND:
		switch int(loword(wParam)) {
		case idISOBrowse:
			if p := browseFile("Open UMD Video ISO", "ISO images (*.iso)|*.iso|All files (*.*)|*.*", false, ""); p != "" {
				setText(hISO, p)
				if strings.TrimSpace(getText(hOut)) == "" {
					setText(hOut, strings.TrimSuffix(p, filepath.Ext(p))+".mkv")
				}
				doScan(p)
			}
		case idOutBrowse:
			if p := browseFile("Save MKV", "Matroska (*.mkv)|*.mkv|All files (*.*)|*.*", true, getText(hOut)); p != "" {
				setText(hOut, p)
			}
		case idFFRecheck:
			updateFFmpegStatus(true)
		case idTVMode:
			tvRaw, _, _ := pSendMessageW.Call(uintptr(hTVMode), BM_GETCHECK, 0, 0)
			if tvRaw == BST_CHECKED {
				pSendMessageW.Call(uintptr(hAllStreams), BM_SETCHECK, 0, 0)
			}
			if p := strings.TrimSpace(getText(hISO)); p != "" && !converting.Load() {
				doScan(p)
			}
		case idAllStreams:
			allRaw, _, _ := pSendMessageW.Call(uintptr(hAllStreams), BM_GETCHECK, 0, 0)
			if allRaw == BST_CHECKED {
				pSendMessageW.Call(uintptr(hTVMode), BM_SETCHECK, 0, 0)
			}
			if p := strings.TrimSpace(getText(hISO)); p != "" && !converting.Load() {
				doScan(p)
			}
		case idConvert:
			if converting.CompareAndSwap(false, true) {
				isoPath := strings.TrimSpace(getText(hISO))
				out := strings.TrimSpace(getText(hOut))
				includeSubsRaw, _, _ := pSendMessageW.Call(uintptr(hSubs), BM_GETCHECK, 0, 0)
				includeSubs := includeSubsRaw == BST_CHECKED
				flacRaw, _, _ := pSendMessageW.Call(uintptr(hFLAC), BM_GETCHECK, 0, 0)
				useFLAC := flacRaw == BST_CHECKED
				tvRaw, _, _ := pSendMessageW.Call(uintptr(hTVMode), BM_GETCHECK, 0, 0)
				tvMode := tvRaw == BST_CHECKED
				allRaw, _, _ := pSendMessageW.Call(uintptr(hAllStreams), BM_GETCHECK, 0, 0)
				allStreams := allRaw == BST_CHECKED
				if isoPath == "" || out == "" {
					converting.Store(false)
					message("Choose an ISO and output MKV first.", MB_OK|MB_ICONERROR)
					break
				}
				ffmpeg := updateFFmpegStatus(false)
				if ffmpeg == "" {
					converting.Store(false)
					message("ffmpeg.exe was not found. Put ffmpeg.exe beside UMD2MKV.exe or add it to PATH.", MB_OK|MB_ICONERROR)
					break
				}
				enableControls(false)
				setText(hConvert, "Converting |")
				appendLog("Starting conversion worker...")
				go animateConvertButton()
				go doConvert(isoPath, out, ffmpeg, includeSubs, useFLAC, tvMode, allStreams)
			}
		}
		return 0
	case WM_DESTROY:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

func humanSize(n uint32) string {
	v := float64(n)
	units := []string{"B", "KB", "MB", "GB"}
	for _, u := range units {
		if v < 1024 || u == "GB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= 1024
	}
	return fmt.Sprintf("%d B", n)
}

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
		return ScanResult{}, fmt.Errorf("not an ISO9660 image (PSP UMD ISOs normally expose ISO9660)")
	}
	root, _, err := readDirRecord(pvd, 156)
	if err != nil {
		return ScanResult{}, err
	}
	root.Path = "/"
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
	var pool []IsoEntry
	for _, e := range entries {
		if strings.Contains(strings.ToUpper(e.Path), "/UMD_VIDEO/") {
			pool = append(pool, e)
		}
	}
	if len(pool) == 0 {
		pool = entries
	}
	// UMD Video discs commonly store the complete movie (H.264 video plus
	// ATRAC3+ audio) inside .MPS files under UMD_VIDEO/STREAM.  Do not
	// require a separate .OMA file: those are often produced only by older
	// extraction workflows and are not necessarily present on the disc.
	subExt := map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".sup": true, ".idx": true}
	var movies, subs []IsoEntry
	for _, e := range pool {
		ext := strings.ToLower(filepath.Ext(e.Name))
		upperPath := strings.ToUpper(strings.ReplaceAll(e.Path, "\\", "/"))
		if ext == ".mps" && strings.Contains(upperPath, "/UMD_VIDEO/STREAM/") {
			movies = append(movies, e)
		}
		if subExt[ext] {
			subs = append(subs, e)
		}
	}
	// Be a little more permissive for unusual dumps where the directory
	// casing/layout differs but the files are still MPS streams.
	if len(movies) == 0 {
		for _, e := range pool {
			if strings.EqualFold(filepath.Ext(e.Name), ".mps") {
				movies = append(movies, e)
			}
		}
	}
	if len(movies) == 0 {
		return ScanResult{}, fmt.Errorf("no .MPS movie stream found under UMD_VIDEO/STREAM")
	}
	// Stream names normally reflect authored playback order (00001, 00002,
	// ...), which is the most useful order for TV episode output.
	sort.Slice(movies, func(i, j int) bool { return strings.ToLower(movies[i].Path) < strings.ToLower(movies[j].Path) })
	sort.Slice(subs, func(i, j int) bool { return subs[i].Size > subs[j].Size })
	movie := movies[0]
	for _, candidate := range movies[1:] {
		if candidate.Size > movie.Size {
			movie = candidate
		}
	}
	var clips []IsoEntry
	for _, e := range entries {
		up := strings.ToUpper(strings.ReplaceAll(e.Path, "\\", "/"))
		if strings.Contains(up, "/UMD_VIDEO/CLIPINF/") && strings.EqualFold(filepath.Ext(e.Name), ".CLP") {
			clips = append(clips, e)
		}
	}
	stem := strings.TrimSuffix(movie.Name, filepath.Ext(movie.Name))
	var clip *IsoEntry
	for i := range clips {
		e := clips[i]
		if strings.EqualFold(strings.TrimSuffix(e.Name, filepath.Ext(e.Name)), stem) {
			x := e
			clip = &x
			break
		}
	}
	return ScanResult{Movie: movie, ClipInfo: clip, Streams: movies, Clips: clips, Subs: subs}, nil
}

func clipForStream(r ScanResult, stream IsoEntry) *IsoEntry {
	stem := strings.TrimSuffix(stream.Name, filepath.Ext(stream.Name))
	for i := range r.Clips {
		if strings.EqualFold(strings.TrimSuffix(r.Clips[i].Name, filepath.Ext(r.Clips[i].Name)), stem) {
			clip := r.Clips[i]
			return &clip
		}
	}
	return nil
}

func extractEntry(isoPath string, e IsoEntry, out string) error {
	return extractEntryProgress(isoPath, e, out, nil)
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
	local := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local
	}
	if p, err := exec.LookPath("ffmpeg.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

func findFFprobe(ffmpeg string) string {
	if ffmpeg != "" {
		for _, name := range []string{"ffprobe.exe", "ffprobe"} {
			p := filepath.Join(filepath.Dir(ffmpeg), name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	for _, name := range []string{"ffprobe.exe", "ffprobe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func probeDuration(ffprobe, mediaPath string) (float64, error) {
	text, err := runFFmpeg([]string{ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", mediaPath})
	if err != nil {
		return 0, fmt.Errorf("ffprobe could not read duration: %w (%s)", err, strings.TrimSpace(text))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe returned an invalid duration %q", strings.TrimSpace(text))
	}
	return duration, nil
}

func runFFmpeg(args []string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	return stderr.String(), err
}

// demuxAtracFromMPS extracts PSP ATRAC3+ audio from genuine MPEG private-stream
// packets and rebuilds each track as a Sony OMA file.
//
// PSP MPS private_stream_1 packets have a PSP substream byte followed by three
// private-header bytes.  The remaining bytes form one continuous PSP audio
// stream.  That stream contains 8-byte sound-frame headers beginning 0F D0;
// those headers describe the ATRAC3+ frame size/configuration but are not part
// of the ATRAC frame payload stored in OMA.  Frames can cross PES boundaries,
// so we first concatenate the genuine payload for each PSP audio substream and
// only then strip the 8-byte sound-frame headers.
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
		id      byte
		rawPath string
		file    *os.File
		bytes   int64
		packets int64
	}
	streams := map[byte]*atracOut{}
	closeAll := func() {
		for _, so := range streams {
			if so.file != nil {
				so.file.Close()
				so.file = nil
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
	findNextPacket := func(from int64) int64 {
		const chunkSize = 256 * 1024
		buf := make([]byte, chunkSize+3)
		pos := from
		for pos < fileSize-4 {
			want := int64(chunkSize)
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

	off := findNextPacket(0)
	if off < 0 {
		return nil, fmt.Errorf("could not find an MPEG program-stream packet in the selected MPS")
	}
	lastPercent := -1
	for off >= 0 && off+4 <= fileSize {
		hdr4, e := readAt(off, 4)
		if e != nil {
			break
		}
		if hdr4[0] != 0 || hdr4[1] != 0 || hdr4[2] != 1 || !isPSCode(hdr4[3]) {
			off = findNextPacket(off + 1)
			continue
		}
		code := hdr4[3]
		if code == 0xB9 {
			break
		}
		if code == 0xBA {
			ph, e := readAt(off, 14)
			if e != nil {
				break
			}
			packLen := int64(14 + int(ph[13]&0x07))
			if off+packLen > fileSize {
				break
			}
			off += packLen
		} else {
			h6, e := readAt(off, 6)
			if e != nil {
				break
			}
			packetLen := int64(binary.BigEndian.Uint16(h6[4:6]))
			if packetLen <= 0 || off+6+packetLen > fileSize {
				off = findNextPacket(off + 4)
				continue
			}
			packetEnd := off + 6 + packetLen
			if code == 0xBD && packetLen >= 8 {
				pes, e := readAt(off, 9)
				if e == nil {
					extra := int64(pes[8])
					streamIDPos := off + 9 + extra
					// JPCSP/PSMF demuxers remove four PSP-private bytes here:
					// channel + three private-header bytes. What follows is the
					// continuous sound-frame stream (0F D0 headers + ATRAC data).
					payloadStart := streamIDPos + 4
					if streamIDPos < packetEnd && payloadStart < packetEnd {
						idb, er := readAt(streamIDPos, 1)
						if er == nil && idb[0] < 0x20 {
							streamID := idb[0]
							payloadLen := int(packetEnd - payloadStart)
							payload, er := readAt(payloadStart, payloadLen)
							if er != nil && er != io.EOF {
								return nil, er
							}
							so := streams[streamID]
							if so == nil {
								rawPath := filepath.Join(outDir, fmt.Sprintf("audio_stream_%02X.pspaudio", streamID))
								rf, ce := os.Create(rawPath)
								if ce != nil {
									return nil, ce
								}
								so = &atracOut{id: streamID, rawPath: rawPath, file: rf}
								streams[streamID] = so
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
			off = packetEnd
		}
		pct := int((off * 100) / fileSize)
		if pct >= lastPercent+10 {
			lastPercent = pct
			postLog(fmt.Sprintf("Audio scan: %d%%", pct))
		}
	}
	closeAll()
	if len(streams) == 0 {
		return nil, fmt.Errorf("no PSP ATRAC3+ private-stream audio packets found in the selected MPS")
	}

	var maxBytes int64
	for _, so := range streams {
		if so.bytes > maxBytes {
			maxBytes = so.bytes
		}
	}
	minUseful := maxBytes / 20
	if minUseful < 256*1024 {
		minUseful = 256 * 1024
	}
	ids := make([]int, 0, len(streams))
	for id := range streams {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var outs []string
	for _, idi := range ids {
		so := streams[byte(idi)]
		if so.bytes < minUseful {
			postLog(fmt.Sprintf("Ignoring short private stream %02X (%d bytes)", so.id, so.bytes))
			continue
		}
		postLog(fmt.Sprintf("PSP audio stream %02X: %d PES packets, %d bytes", so.id, so.packets, so.bytes))
		out := filepath.Join(outDir, fmt.Sprintf("audio_%02d_stream_%02X.oma", len(outs), so.id))
		frames, resyncs, cfg, er := convertPSPAudioToOMA(so.rawPath, out)
		if er != nil {
			postLog(fmt.Sprintf("Rejecting audio stream %02X: %v", so.id, er))
			continue
		}
		postLog(fmt.Sprintf("Rebuilt stream %02X: %d ATRAC frames, %d resync(s), config %04X", so.id, frames, resyncs, cfg))
		outs = append(outs, out)
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("ATRAC packets were found, but no valid full-length OMA audio stream could be rebuilt")
	}
	return outs, nil
}

// convertPSPAudioToOMA follows the PSP/JPCSP sound-frame layout.  Each sound
// frame begins with an 8-byte header: 0F D0 + a 16-bit ATRAC configuration +
// four more header bytes.  The OMA file stores a 96-byte EA3 header followed by
// only the ATRAC frame payload, so the 8-byte PSP headers are removed.
func convertPSPAudioToOMA(rawPath, outPath string) (frames int, resyncs int, config uint16, err error) {
	data, err := os.ReadFile(rawPath)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(data) < 16 {
		return 0, 0, 0, fmt.Errorf("audio stream is too short")
	}

	validHeader := func(pos int, want uint16, enforce bool) bool {
		if pos < 0 || pos+8 > len(data) || data[pos] != 0x0F || data[pos+1] != 0xD0 {
			return false
		}
		cfg := binary.BigEndian.Uint16(data[pos+2 : pos+4])
		if enforce && cfg != want {
			return false
		}
		srate := (cfg >> 13) & 7
		chid := (cfg >> 10) & 7
		block := int(cfg&0x03FF)*8 + 8
		return srate <= 4 && chid >= 1 && chid <= 7 && block >= 16 && block <= 8192
	}
	first := -1
	for i := 0; i+8 <= len(data); i++ {
		if validHeader(i, 0, false) {
			first = i
			break
		}
	}
	if first < 0 {
		return 0, 0, 0, fmt.Errorf("no valid 0F D0 PSP sound-frame header found")
	}
	if first > 0 {
		resyncs++
	}
	config = binary.BigEndian.Uint16(data[first+2 : first+4])
	blockAlign := int(config&0x03FF)*8 + 8

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
		if !validHeader(pos, config, true) {
			// Search forward for the next header with the same config. This is
			// intentionally conservative to avoid mistaking ATRAC payload for a
			// frame header.
			found := -1
			limit := pos + blockAlign + 4096
			if limit > len(data)-8 {
				limit = len(data) - 8
			}
			for i := pos + 1; i <= limit; i++ {
				if validHeader(i, config, true) {
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
		dataStart := pos + 8
		expectedNext := dataStart + blockAlign
		if expectedNext > len(data) {
			// Do not emit a truncated final ATRAC frame.
			break
		}
		if _, err = dst.Write(data[dataStart:expectedNext]); err != nil {
			return frames, resyncs, config, err
		}
		frames++
		if expectedNext+8 <= len(data) && validHeader(expectedNext, config, true) {
			pos = expectedNext
			continue
		}
		// Some discs split/repack frames unusually. Find the next same-config
		// sound header near the expected boundary and continue from there.
		found := -1
		lo := expectedNext - 64
		if lo < dataStart {
			lo = dataStart
		}
		hi := expectedNext + 4096
		if hi > len(data)-8 {
			hi = len(data) - 8
		}
		for i := lo; i <= hi; i++ {
			if validHeader(i, config, true) {
				found = i
				break
			}
		}
		if found < 0 {
			break
		}
		if found != expectedNext {
			resyncs++
		}
		pos = found
	}
	if frames < 100 {
		return frames, resyncs, config, fmt.Errorf("only %d valid ATRAC frames were reconstructed", frames)
	}
	return frames, resyncs, config, nil
}

func makeOMAHeader(headerCode1, headerCode2 byte) []byte {
	// 96-byte EA3/OMA header used for ATRAC3+. The two configuration bytes are
	// copied directly from the PSP 0F D0 sound-frame header.
	h := make([]byte, 96)
	binary.BigEndian.PutUint32(h[0:4], 0x45413301) // EA3\x01
	binary.BigEndian.PutUint16(h[4:6], 96)
	binary.BigEndian.PutUint16(h[6:8], 0xFFFF)
	binary.BigEndian.PutUint32(h[8:12], 0x00000000)
	binary.BigEndian.PutUint32(h[12:16], 0x010F5000)
	binary.BigEndian.PutUint32(h[16:20], 0x00040000)
	binary.BigEndian.PutUint32(h[20:24], 0x0000F5CE)
	binary.BigEndian.PutUint32(h[24:28], 0xD2929132)
	binary.BigEndian.PutUint32(h[28:32], 0x2480451C)
	h[32] = 0x01 // ATRAC3+
	h[33] = 0x00
	h[34] = headerCode1
	h[35] = headerCode2
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

func doScan(path string) {
	enableControls(false)
	defer enableControls(true)
	appendLog("Scanning ISO filesystem...")
	r, err := scanISO(path)
	if err != nil {
		appendLog("ERROR: " + err.Error())
		message(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	langs := detectCLPLanguages(path, r.ClipInfo)
	langText := "not detected"
	if len(langs) > 0 {
		langText = languageSummary(langs)
	}
	priv := detectCLPPrivateLanguages(path, r.ClipInfo)
	var subParts []string
	var subIDs []int
	for id := range priv {
		subIDs = append(subIDs, int(id))
	}
	sort.Ints(subIDs)
	for _, ii := range subIDs {
		li := priv[byte(ii)]
		subParts = append(subParts, fmt.Sprintf("%02X=%s (%s)", ii, li.Code, li.Name))
	}
	subText := "not detected"
	if len(subParts) > 0 {
		subText = strings.Join(subParts, ", ")
	}
	tvRaw, _, _ := pSendMessageW.Call(uintptr(hTVMode), BM_GETCHECK, 0, 0)
	if tvRaw == BST_CHECKED {
		s := fmt.Sprintf("TV mode: %d MPS streams found; conversion keeps streams 3:00 or longer\r\nOutput: separate files named ... - E01.mkv, E02.mkv, etc.\r\nEpisode durations are verified with FFprobe during conversion.", len(r.Streams))
		setText(hSelection, s)
		appendLog(fmt.Sprintf("TV mode found %d MPS streams. Duration probing will exclude menus and clips shorter than 3:00.", len(r.Streams)))
		return
	}
	allRaw, _, _ := pSendMessageW.Call(uintptr(hAllStreams), BM_GETCHECK, 0, 0)
	if allRaw == BST_CHECKED {
		s := fmt.Sprintf("All-stream mode: all %d MPS streams will be converted\r\nNo duration filter: menus, intros, transitions, and short extras are preserved.\r\nOutput filenames retain each original stream ID.", len(r.Streams))
		setText(hSelection, s)
		appendLog(fmt.Sprintf("All-stream mode found %d MPS streams. Every stream will be converted without duration filtering.", len(r.Streams)))
		return
	}
	s := fmt.Sprintf("Movie stream: %s - %s\r\nAudio tracks (CLP): %s\r\nUMD subtitle tracks (CLP): %s", r.Movie.Path, humanSize(r.Movie.Size), langText, subText)
	setText(hSelection, s)
	appendLog("Selected movie stream: " + r.Movie.Path)
	appendLog("Audio will be demuxed internally from PSP private-stream packets in the selected MPS.")
	if len(langs) > 0 {
		appendLog("CLP language candidates: " + languageSummary(langs))
	}
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

func decodePESPTS(h []byte) (int64, bool) {
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

type subtitlePacketMeta struct {
	start, end int64
	packet     int64
	pts        int64
	hasPTS     bool
}

type subtitleDumpOut struct {
	id             byte
	path           string
	f              *os.File
	bytes, packets int64
	meta           []subtitlePacketMeta
}

func subtitleLangSuffix(id byte, langs map[byte]LanguageInfo) string {
	if li, ok := langs[id]; ok {
		return li.Code + "_" + strings.ToLower(strings.ReplaceAll(li.Name, " ", "_"))
	}
	return "unknown"
}

func findPacketForOffset(meta []subtitlePacketMeta, off int64) (subtitlePacketMeta, bool) {
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
		m := meta[lo]
		if m.hasPTS {
			return m, true
		}
		for i := lo - 1; i >= 0; i-- {
			if meta[i].hasPTS {
				return meta[i], true
			}
		}
		return m, false
	}
	return subtitlePacketMeta{}, false
}

func writeTimedSubtitleRecords(so *subtitleDumpOut, outDir string, langs map[byte]LanguageInfo) (string, int, error) {
	data, err := os.ReadFile(so.path)
	if err != nil {
		return "", 0, err
	}
	suffix := subtitleLangSuffix(so.id, langs)
	pngDir := filepath.Join(outDir, fmt.Sprintf("stream_%02X_%s_png", so.id, suffix))
	if err := os.MkdirAll(pngDir, 0755); err != nil {
		return "", 0, err
	}
	csvPath := filepath.Join(outDir, fmt.Sprintf("stream_%02X_%s_timing.csv", so.id, suffix))
	cf, err := os.Create(csvPath)
	if err != nil {
		return "", 0, err
	}
	defer cf.Close()
	fmt.Fprintln(cf, "record,stream_id,language,start_pts,start_seconds,duration_pts,duration_seconds,end_seconds,x,y,width,height,png_file,packet_index,record_offset,record_bytes")

	lang := "und"
	if li, ok := langs[so.id]; ok {
		lang = li.Code
	}
	pngSig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	pos, rec := 0, 0
	for pos+18 <= len(data) {
		bodyLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		total := bodyLen + 2
		if total < 26 || pos+total > len(data) || string(data[pos+2:pos+6]) != "0088" || !bytes.Equal(data[pos+18:pos+26], pngSig) {
			next := bytes.Index(data[pos+1:], []byte("0088"))
			if next < 1 {
				break
			}
			candidate := pos + 1 + next - 2
			if candidate <= pos {
				candidate = pos + 1
			}
			pos = candidate
			continue
		}
		duration := int64(binary.BigEndian.Uint32(data[pos+6 : pos+10]))
		x := int(binary.BigEndian.Uint16(data[pos+12 : pos+14]))
		y := int(binary.BigEndian.Uint16(data[pos+14 : pos+16]))
		png := data[pos+18 : pos+total]
		width, height := 0, 0
		if len(png) >= 24 && bytes.Equal(png[:8], pngSig) && string(png[12:16]) == "IHDR" {
			width = int(binary.BigEndian.Uint32(png[16:20]))
			height = int(binary.BigEndian.Uint32(png[20:24]))
		}
		rec++
		pngName := fmt.Sprintf("%06d.png", rec)
		if err := os.WriteFile(filepath.Join(pngDir, pngName), png, 0644); err != nil {
			return "", rec - 1, err
		}
		m, hasPTS := findPacketForOffset(so.meta, int64(pos))
		startPTS := int64(-1)
		packetIndex := int64(-1)
		startSec, endSec := "", ""
		if hasPTS {
			startPTS = m.pts
			packetIndex = m.packet
			startSec = fmt.Sprintf("%.6f", float64(startPTS)/90000.0)
			endSec = fmt.Sprintf("%.6f", float64(startPTS+duration)/90000.0)
		}
		durSec := fmt.Sprintf("%.6f", float64(duration)/90000.0)
		fmt.Fprintf(cf, "%d,%02X,%s,%d,%s,%d,%s,%s,%d,%d,%d,%d,%s,%d,%d,%d\n",
			rec, so.id, lang, startPTS, startSec, duration, durSec, endSec, x, y, width, height, pngName, packetIndex, pos, total)
		pos += total
	}
	return csvPath, rec, nil
}

func dumpSubtitlePrivateStreams(mpsPath, outDir string, langs map[byte]LanguageInfo) ([]string, error) {
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
	streams := map[byte]*subtitleDumpOut{}
	closeAll := func() {
		for _, s := range streams {
			if s.f != nil {
				s.f.Close()
				s.f = nil
			}
		}
	}
	defer closeAll()
	readAt := func(off int64, n int) ([]byte, error) { b := make([]byte, n); _, e := f.ReadAt(b, off); return b, e }
	isPSCode := func(c byte) bool {
		return c == 0xBA || c == 0xB9 || c == 0xBB || c == 0xBD || c == 0xBE || c == 0xBF || (c >= 0xC0 && c <= 0xDF) || (c >= 0xE0 && c <= 0xEF)
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
						ib, er := readAt(idpos, 1)
						if er == nil && ib[0] >= 0x80 && ib[0] <= 0x9F {
							id := ib[0]
							n := int(end - payload)
							data, er := readAt(payload, n)
							if er != nil && er != io.EOF {
								return nil, er
							}
							so := streams[id]
							if so == nil {
								path := filepath.Join(outDir, fmt.Sprintf("subtitle_%02X_%s.bin", id, subtitleLangSuffix(id, langs)))
								rf, ce := os.Create(path)
								if ce != nil {
									return nil, ce
								}
								so = &subtitleDumpOut{id: id, path: path, f: rf}
								streams[id] = so
							}
							start := so.bytes
							w, we := so.f.Write(data)
							if we != nil {
								return nil, we
							}
							var pts int64
							hasPTS := false
							if extra >= 5 {
								hdr, he := readAt(off, 14)
								if he == nil {
									pts, hasPTS = decodePESPTS(hdr)
								}
							}
							so.meta = append(so.meta, subtitlePacketMeta{start: start, end: start + int64(w), packet: so.packets, pts: pts, hasPTS: hasPTS})
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
			postLog(fmt.Sprintf("Subtitle timing scan: %d%%", pct))
		}
	}
	closeAll()
	ids := make([]int, 0, len(streams))
	for id := range streams {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var csvPaths []string
	for _, i := range ids {
		so := streams[byte(i)]
		csvPath, records, er := writeTimedSubtitleRecords(so, outDir, langs)
		if er != nil {
			return nil, er
		}
		postLog(fmt.Sprintf("Subtitle stream %02X: %d PES packets, %d subtitle record(s), %d bytes", so.id, so.packets, records, so.bytes))
		postLog("Timing CSV: " + filepath.Base(csvPath))
		csvPaths = append(csvPaths, csvPath)
	}
	if len(csvPaths) == 0 {
		return nil, fmt.Errorf("no PSP private substreams 0x80-0x9F were found")
	}
	return csvPaths, nil
}

func subtitleLanguageMap(in map[byte]LanguageInfo) map[byte]pspsubs.Language {
	out := map[byte]pspsubs.Language{}
	for id, li := range in {
		out[id] = pspsubs.Language{Code: li.Code, Name: li.Name}
	}
	return out
}

func doAnalyzeTracks(isoPath string) {
	defer func() {
		converting.Store(false)
		postUI(func() { setText(hScanTracks, "Scan Tracks"); enableControls(true) })
	}()
	postLog("Track scan: reading ISO metadata...")
	r, err := scanISO(isoPath)
	if err != nil {
		postLog("ERROR: " + err.Error())
		postMessage(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	tmp, err := os.MkdirTemp("", "umd2mkv-scan-")
	if err != nil {
		postMessage(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	defer os.RemoveAll(tmp)
	movie := filepath.Join(tmp, "movie.mps")
	postLog("Track scan: extracting the selected movie stream. This reads the full MPS but does not encode anything.")
	if err = extractEntryProgress(isoPath, r.Movie, movie, func(p int) { postLog(fmt.Sprintf("Track scan extraction: %d%%", p)) }); err != nil {
		postLog("ERROR: " + err.Error())
		postMessage(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	audioFiles, aerr := demuxAtracFromMPS(movie, tmp)
	if aerr != nil {
		postLog("Audio scan error: " + aerr.Error())
	} else {
		langs := detectCLPLanguages(isoPath, r.ClipInfo)
		postLog(fmt.Sprintf("Track scan found %d valid audio track(s).", len(audioFiles)))
		for _, a := range audioFiles {
			if li, ok := languageForAudioFile(a, langs); ok {
				postLog(fmt.Sprintf("  Audio %02X: %s [%s]", li.StreamID, li.Name, li.Code))
			} else {
				postLog("  Audio: " + filepath.Base(a))
			}
		}
	}
	priv := detectCLPPrivateLanguages(isoPath, r.ClipInfo)
	tracks, serr := pspsubs.Scan(movie, tmp, subtitleLanguageMap(priv), postLog)
	if serr != nil {
		postLog("Subtitle scan: " + serr.Error())
	} else {
		postLog(fmt.Sprintf("Track scan found %d UMD subtitle track(s).", len(tracks)))
		for _, t := range tracks {
			name := "Unknown"
			code := "und"
			if t.Language.Name != "" {
				name = t.Language.Name
			}
			if t.Language.Code != "" {
				code = t.Language.Code
			}
			postLog(fmt.Sprintf("  Subtitle %02X: %s [%s], %d image(s)", t.StreamID, name, code, t.Records))
		}
	}
	postMessage("Track scan complete. See the log for detected audio/subtitle tracks and subtitle image counts.", MB_OK|MB_ICONINFORMATION)
}

func episodeOutputPath(out string, episode int) string {
	ext := filepath.Ext(out)
	if ext == "" {
		ext = ".mkv"
	}
	stem := strings.TrimSuffix(out, filepath.Ext(out))
	return fmt.Sprintf("%s - E%02d%s", stem, episode, ext)
}

func streamOutputPath(out string, stream IsoEntry, index int) string {
	ext := filepath.Ext(out)
	if ext == "" {
		ext = ".mkv"
	}
	base := strings.TrimSuffix(out, filepath.Ext(out))
	streamID := strings.TrimSpace(strings.TrimSuffix(stream.Name, filepath.Ext(stream.Name)))
	if streamID == "" {
		streamID = fmt.Sprintf("Stream %02d", index)
	}
	return fmt.Sprintf("%s - %s%s", base, streamID, ext)
}

func formatDuration(seconds float64) string {
	total := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func muxStream(isoPath, movie, tmp, out, ffmpeg string, clip *IsoEntry, includeSubs, useFLAC bool) error {
	postLog("Rebuilding PSP ATRAC3+ audio tracks...")
	audioFiles, audioErr := demuxAtracFromMPS(movie, tmp)
	if audioErr != nil {
		return fmt.Errorf("audio demux failed: %w", audioErr)
	}
	langs := detectCLPLanguages(isoPath, clip)
	for _, a := range audioFiles {
		if st, e := os.Stat(a); e == nil {
			postLog(fmt.Sprintf("Extracted audio: %s (%d bytes)", filepath.Base(a), st.Size()))
		}
	}
	if len(langs) > 0 {
		postLog("Detected CLP audio languages: " + languageSummary(langs))
	}

	var subTracks []pspsubs.Track
	if includeSubs {
		postLog("Converting original UMD PNG subtitles to selectable PGS tracks...")
		priv := detectCLPPrivateLanguages(isoPath, clip)
		var subErr error
		subTracks, subErr = pspsubs.BuildPGS(movie, tmp, subtitleLanguageMap(priv), postLog)
		if subErr != nil {
			postLog("Subtitle extraction skipped: " + subErr.Error())
			subTracks = nil
		}
	}

	base := []string{ffmpeg, "-y", "-hide_banner", "-loglevel", "warning", "-i", movie}
	for _, a := range audioFiles {
		base = append(base, "-i", a)
	}
	for _, t := range subTracks {
		base = append(base, "-i", t.Path)
	}
	maps := []string{"-map", "0:v:0"}
	for i := range audioFiles {
		maps = append(maps, "-map", fmt.Sprintf("%d:a:0", i+1))
	}
	for i := range subTracks {
		maps = append(maps, "-map", fmt.Sprintf("%d:s:0", 1+len(audioFiles)+i))
	}
	postLog(fmt.Sprintf("Muxing video, %d audio track(s), and %d subtitle track(s)...", len(audioFiles), len(subTracks)))
	args := append(append([]string{}, base...), maps...)
	for i, audioFile := range audioFiles {
		if lang, ok := languageForAudioFile(audioFile, langs); ok {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+lang.Code, fmt.Sprintf("-metadata:s:a:%d", i), "title="+lang.Name)
			postLog(fmt.Sprintf("Audio stream %02X -> %s [%s]", lang.StreamID, lang.Name, lang.Code))
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
		postLog(fmt.Sprintf("Subtitle stream %02X -> %s [%s], %d images", t.StreamID, name, code, t.Records))
	}
	args = append(args, "-c:v", "copy")
	if useFLAC {
		postLog("Audio output: FLAC (lossless, larger files)")
		args = append(args, "-c:a", "flac")
	} else {
		postLog("Audio output: AAC-LC 256 kbps (recommended compatibility mode)")
		args = append(args, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "256k")
	}
	args = append(args, "-c:s", "copy", out)
	logText, err := runFFmpeg(args)
	if err != nil && len(subTracks) > 0 {
		postLog("PGS subtitle mux failed; retrying video/audio without subtitles.")
		base2 := []string{ffmpeg, "-y", "-hide_banner", "-loglevel", "warning", "-i", movie}
		for _, a := range audioFiles {
			base2 = append(base2, "-i", a)
		}
		maps2 := []string{"-map", "0:v:0"}
		for i := range audioFiles {
			maps2 = append(maps2, "-map", fmt.Sprintf("%d:a:0", i+1))
		}
		args = append(append([]string{}, base2...), maps2...)
		for i, audioFile := range audioFiles {
			if lang, ok := languageForAudioFile(audioFile, langs); ok {
				args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+lang.Code, fmt.Sprintf("-metadata:s:a:%d", i), "title="+lang.Name)
			}
		}
		args = append(args, "-c:v", "copy")
		if useFLAC {
			args = append(args, "-c:a", "flac")
		} else {
			args = append(args, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "256k")
		}
		args = append(args, out)
		logText, err = runFFmpeg(args)
	}
	if err != nil {
		return fmt.Errorf("FFmpeg conversion failed: %w\n%s", err, logText)
	}
	return nil
}

func doConvert(isoPath, out, ffmpeg string, includeSubs, useFLAC, tvMode, allStreams bool) {
	defer func() {
		converting.Store(false)
		postUI(func() { setText(hConvert, "Convert to MKV"); enableControls(true) })
	}()

	postLog("Scanning ISO...")
	r, err := scanISO(isoPath)
	if err != nil {
		postLog("ERROR: " + err.Error())
		postMessage(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	tmp, err := os.MkdirTemp("", "umd2mkv-")
	if err != nil {
		postMessage(err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	defer os.RemoveAll(tmp)
	streams := []IsoEntry{r.Movie}
	batchMode := tvMode || allStreams
	ffprobe := ""
	if batchMode {
		streams = r.Streams
	}
	if tvMode {
		ffprobe = findFFprobe(ffmpeg)
		if ffprobe == "" {
			postLog("ERROR: TV mode requires ffprobe.exe beside ffmpeg.exe or on PATH.")
			postMessage("TV mode needs ffprobe.exe to identify episode-length streams. Put it beside ffmpeg.exe or add it to PATH.", MB_OK|MB_ICONERROR)
			return
		}
		postLog(fmt.Sprintf("TV mode: checking %d streams; streams shorter than 3:00 are skipped.", len(streams)))
	} else if allStreams {
		postLog(fmt.Sprintf("All-stream mode: converting all %d streams without a duration filter.", len(streams)))
	}

	episode, skipped, failed, succeeded := 0, 0, 0, 0
	for index, stream := range streams {
		streamTmp, tempErr := os.MkdirTemp(tmp, "stream-")
		if tempErr != nil {
			postLog("ERROR: " + tempErr.Error())
			failed++
			continue
		}
		movie := filepath.Join(streamTmp, "movie.mps")
		postLog(fmt.Sprintf("Stream %d/%d: extracting %s (%s)", index+1, len(streams), stream.Path, humanSize(stream.Size)))
		err = extractEntryProgress(isoPath, stream, movie, func(pct int) { postLog(fmt.Sprintf("Extraction: %d%%", pct)) })
		if err != nil {
			postLog("EXTRACTION ERROR: " + err.Error())
			os.RemoveAll(streamTmp)
			failed++
			if !batchMode {
				postMessage(err.Error(), MB_OK|MB_ICONERROR)
				return
			}
			continue
		}

		if tvMode {
			duration, probeErr := probeDuration(ffprobe, movie)
			if probeErr != nil {
				postLog("DURATION ERROR: " + probeErr.Error())
				os.RemoveAll(streamTmp)
				failed++
				continue
			}
			postLog(fmt.Sprintf("Duration: %s", formatDuration(duration)))
			if duration < 180 {
				postLog("Skipped short menu/clip (under 3:00): " + stream.Path)
				os.RemoveAll(streamTmp)
				skipped++
				continue
			}
		}

		episode++
		streamOut := out
		if tvMode {
			streamOut = episodeOutputPath(out, episode)
			postLog(fmt.Sprintf("Episode %02d -> %s", episode, streamOut))
		} else if allStreams {
			streamOut = streamOutputPath(out, stream, index+1)
			postLog(fmt.Sprintf("Stream %s -> %s", stream.Name, streamOut))
		}
		err = muxStream(isoPath, movie, streamTmp, streamOut, ffmpeg, clipForStream(r, stream), includeSubs, useFLAC)
		os.RemoveAll(streamTmp)
		if err != nil {
			postLog("CONVERSION ERROR: " + err.Error())
			failed++
			if !batchMode {
				postMessage("FFmpeg conversion failed. See the log in the app.", MB_OK|MB_ICONERROR)
				return
			}
			continue
		}
		postLog("Done: " + streamOut)
		succeeded++
	}

	if !batchMode {
		postMessage("MKV created:\r\n"+out, MB_OK|MB_ICONINFORMATION)
		return
	}
	if succeeded == 0 {
		postMessage("No MKVs were created. See the log for extraction or conversion errors.", MB_OK|MB_ICONERROR)
		return
	}
	summary := fmt.Sprintf("All-stream conversion complete: %d MKV(s) created.", succeeded)
	if tvMode {
		summary = fmt.Sprintf("TV conversion complete: %d episode MKV(s) created; %d short stream(s) skipped.", succeeded, skipped)
	}
	if failed > 0 {
		summary += fmt.Sprintf(" %d stream(s) failed; see the log.", failed)
	}
	postLog(summary)
	postMessage(summary+"\r\n\r\nOutput folder:\r\n"+filepath.Dir(out), MB_OK|MB_ICONINFORMATION)
}

func main() {
	runtime.LockOSThread()
	hInst, _, _ := pGetModuleHandleW.Call(0)
	className := utf16p("UMD2MKVWindow")
	wc := WNDCLASSEX{CbSize: uint32(unsafe.Sizeof(WNDCLASSEX{})), LpfnWndProc: syscall.NewCallback(wndProc), HInstance: syscall.Handle(hInst), HbrBackground: syscall.Handle(6), LpszClassName: className}
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	r, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16p("UMD Video ISO to MKV"))), WS_OVERLAPPEDWINDOW, CW_USEDEFAULT, CW_USEDEFAULT, 800, 680, 0, 0, hInst, 0)
	hwndMain = syscall.Handle(r)
	createControl("STATIC", "PSP UMD Video ISO to MKV", SS_LEFT, 20, 16, 500, 28, hwndMain, 0)
	createControl("STATIC", "ISO:", SS_LEFT, 20, 58, 80, 22, hwndMain, 0)
	hISO = createControl("EDIT", "", WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL, 100, 54, 560, 26, hwndMain, idISOEdit)
	createControl("BUTTON", "Browse...", BS_PUSHBUTTON|WS_TABSTOP, 670, 53, 95, 28, hwndMain, idISOBrowse)
	createControl("STATIC", "Output:", SS_LEFT, 20, 94, 80, 22, hwndMain, 0)
	hOut = createControl("EDIT", "", WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL, 100, 90, 560, 26, hwndMain, idOutEdit)
	createControl("BUTTON", "Save as...", BS_PUSHBUTTON|WS_TABSTOP, 670, 89, 95, 28, hwndMain, idOutBrowse)
	hFFStatus = createControl("STATIC", "FFmpeg: checking...", SS_LEFT, 20, 130, 620, 22, hwndMain, 0)
	createControl("BUTTON", "Recheck", BS_PUSHBUTTON|WS_TABSTOP, 670, 126, 95, 28, hwndMain, idFFRecheck)
	hSelection = createControl("EDIT", "Choose an ISO; it will be scanned automatically.", WS_BORDER|ES_MULTILINE|ES_READONLY, 20, 158, 745, 72, hwndMain, idSelection)
	hTVMode = createControl("BUTTON", "TV show mode (streams 3 minutes or longer)", BS_AUTOCHECKBOX|WS_TABSTOP, 20, 238, 365, 26, hwndMain, idTVMode)
	hAllStreams = createControl("BUTTON", "Convert all streams (include short content)", BS_AUTOCHECKBOX|WS_TABSTOP, 400, 238, 350, 26, hwndMain, idAllStreams)
	hSubs = createControl("BUTTON", "Include UMD subtitles (PGS)", BS_AUTOCHECKBOX|WS_TABSTOP, 20, 270, 220, 26, hwndMain, idSubs)
	pSendMessageW.Call(uintptr(hSubs), BM_SETCHECK, BST_CHECKED, 0)
	createControl("STATIC", "Audio:", SS_LEFT, 260, 270, 55, 22, hwndMain, 0)
	hAAC = createControl("BUTTON", "AAC 256k (recommended)", BS_AUTORADIOBUTTON|WS_GROUP|WS_TABSTOP, 315, 268, 190, 26, hwndMain, idAAC)
	hFLAC = createControl("BUTTON", "FLAC (lossless)", BS_AUTORADIOBUTTON|WS_TABSTOP, 505, 268, 150, 26, hwndMain, idFLAC)
	pSendMessageW.Call(uintptr(hAAC), BM_SETCHECK, BST_CHECKED, 0)
	hConvert = createControl("BUTTON", "Convert to MKV", BS_DEFPUSHBUTTON|WS_TABSTOP, 20, 306, 150, 34, hwndMain, idConvert)
	createControl("STATIC", "Log:", SS_LEFT, 20, 352, 80, 22, hwndMain, 0)
	hLog = createControl("EDIT", "", WS_BORDER|WS_VSCROLL|ES_MULTILINE|ES_AUTOVSCROLL|ES_READONLY, 20, 374, 745, 210, hwndMain, idLog)
	createControl("STATIC", "UMD2MKV v1.3.0", SS_LEFT, 20, 596, 220, 22, hwndMain, 0)
	createControl("STATIC", "@kacboy", SS_RIGHT, 545, 596, 220, 22, hwndMain, 0)
	pShowWindow.Call(uintptr(hwndMain), SW_SHOW)
	pUpdateWindow.Call(uintptr(hwndMain))
	appendLog("UMD2MKV v1.3.0 ready. TV show mode filters short streams; Convert all streams preserves everything.")
	updateFFmpegStatus(true)
	var msg MSG
	for {
		ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	time.Sleep(10 * time.Millisecond)
}
