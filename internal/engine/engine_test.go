package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("line\n")
	}
	sb.WriteString("last-1\nlast\n")
	os.WriteFile(p, []byte(sb.String()), 0o644)
	got, err := TailLines(p, 2)
	if err != nil || string(got) != "last-1\nlast\n" {
		t.Errorf("TailLines = %q, %v", got, err)
	}
	if got, _ := TailLines(filepath.Join(t.TempDir(), "missing"), 5); got != nil {
		t.Error("missing file should give nothing")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{999: "999 B", 1500: "1.5 kB", 21_000_000: "21.0 MB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %s, want %s", n, got, want)
		}
	}
}
