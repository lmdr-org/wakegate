package jsonfile

import (
	"os"
	"path/filepath"
	"testing"
)

type settings struct {
	Minutes int `json:"minutes"`
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if v, err := Load[settings](path); err != nil || v.Minutes != 0 {
		t.Fatalf("missing file: %+v, %v", v, err)
	}
	if err := Save(path, settings{Minutes: 15}); err != nil {
		t.Fatal(err)
	}
	v, err := Load[settings](path)
	if err != nil || v.Minutes != 15 {
		t.Fatalf("got %+v, %v", v, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestEmptyPath(t *testing.T) {
	if err := Save("", settings{Minutes: 1}); err != nil {
		t.Fatal(err)
	}
	if v, err := Load[settings](""); err != nil || v.Minutes != 0 {
		t.Fatalf("got %+v, %v", v, err)
	}
}
