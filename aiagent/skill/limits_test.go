package skill

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkAllowsLargeBinaryButKeepsTextLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.dat")
	content := bytes.Repeat([]byte{'a'}, 16*1024*1024+1)
	content[0] = 0
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := Walk(dir)
	if err != nil || len(files["asset.dat"]) != len(content) {
		t.Fatalf("large binary rejected: len=%d err=%v", len(files["asset.dat"]), err)
	}
	content[0] = 'a'
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Walk(dir); err == nil || !strings.Contains(err.Error(), "16MB limit") {
		t.Fatalf("oversized text accepted: %v", err)
	}
}

func TestWalkRejectsOversizedFileBeforeReading(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "oversized.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(500*1024*1024 + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, err := Walk(dir); err == nil || !strings.Contains(err.Error(), "500MB limit") {
		t.Fatalf("oversized sparse file accepted: %v", err)
	}
}

func TestExtractZipRejectsOversizedTotalBeforeReading(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for _, name := range []string{"one.bin", "two.bin"} {
		// Forged headers let this test exercise the 500 MiB total check
		// without allocating or extracting hundreds of megabytes.
		if _, err := w.CreateRaw(&zip.FileHeader{
			Name: name, Method: zip.Store,
			UncompressedSize64: 300 * 1024 * 1024,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZip(archive.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "total extracted size exceeds 500MB") {
		t.Fatalf("oversized extracted total accepted: %v", err)
	}
}
