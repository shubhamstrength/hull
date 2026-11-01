package runtime

import "testing"

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "512": 512, "256m": 256 << 20, "1g": 1 << 30, "1.5G": 3 << 29, "64Mi": 64 << 20} {
		got, err := ParseMemory(in)
		if err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseMemory("lots"); err == nil {
		t.Error("expected error")
	}
}

func TestParseVolume(t *testing.T) {
	dir := t.TempDir()
	v, err := ParseVolume(".:/data:ro", dir)
	if err != nil || v.Source != dir || v.Target != "/data" || !v.ReadOnly {
		t.Errorf("got %+v, %v", v, err)
	}
	for _, bad := range []string{"/nope-does-not-exist:/x", ".:relative", ".", ".:/x:bogus"} {
		if _, err := ParseVolume(bad, dir); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
