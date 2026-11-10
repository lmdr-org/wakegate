// Package jsonfile keeps small settings files on disk.
package jsonfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Load reads path into a value of type T. A missing file, or an empty path, gives the
// zero value.
func Load[T any](path string) (T, error) {
	var v T
	if path == "" {
		return v, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(raw, &v)
}

// Save writes v to path atomically: a temporary file, then a rename. An empty path saves
// nothing.
func Save(path string, v any) error {
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
