package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kanade.log")
	l, err := Open(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 12; i++ {
		l.Write([]byte(line))
	}
	l.Close()
	for _, name := range []string{"kanade.log", "kanade.log.1", "kanade.log.2"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Size() == 0 || st.Size() > 100 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
	}
	if _, err := os.Stat(p + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept more than 2 old files")
	}
	// Reopening appends.
	l, _ = Open(p, 100, 2)
	l.Write([]byte("y\n"))
	l.Close()
	if b, _ := os.ReadFile(p); !strings.HasSuffix(string(b), "y\n") || len(b) < 40 {
		t.Fatalf("after reopen %q", b)
	}
}
