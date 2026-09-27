//go:build windows

package main

import "testing"

func TestEpisodeOutputPath(t *testing.T) {
	tests := []struct {
		out  string
		n    int
		want string
	}{
		{`C:\Shows\Family Guy.mkv`, 1, `C:\Shows\Family Guy - E01.mkv`},
		{`C:\Shows\Series`, 12, `C:\Shows\Series - E12.mkv`},
	}
	for _, test := range tests {
		if got := episodeOutputPath(test.out, test.n); got != test.want {
			t.Errorf("episodeOutputPath(%q, %d) = %q, want %q", test.out, test.n, got, test.want)
		}
	}
}

func TestStreamOutputPath(t *testing.T) {
	got := streamOutputPath(`C:\Shows\Family Guy.mkv`, IsoEntry{Name: "00007.MPS"}, 7)
	want := `C:\Shows\Family Guy - 00007.mkv`
	if got != want {
		t.Fatalf("streamOutputPath = %q, want %q", got, want)
	}
}

func TestClipForStream(t *testing.T) {
	r := ScanResult{Clips: []IsoEntry{{Name: "00001.CLP"}, {Name: "00002.clp"}}}
	clip := clipForStream(r, IsoEntry{Name: "00002.MPS"})
	if clip == nil || clip.Name != "00002.clp" {
		t.Fatalf("clipForStream did not match the stream stem case-insensitively: %#v", clip)
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(1349.965); got != "22:30" {
		t.Fatalf("formatDuration = %q, want 22:30", got)
	}
}
