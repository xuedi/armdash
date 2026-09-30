package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const storeVersion = 1

// Store is settings.json: the values saved on the settings page, keyed by
// variable name like the env files. Secrets are kept in the clear, as in the
// env file, and the file is as protected as that is: 0600, owned by the
// service user.
type Store struct {
	path   string
	values map[string]string
}

type storeFile struct {
	Version int               `json:"version"`
	Values  map[string]string `json:"values"`
}

func openStore(path string) (*Store, error) {
	s := &Store{path: path, values: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version > storeVersion {
		return nil, fmt.Errorf("%s: written by a newer armdash (format %d)", path, f.Version)
	}
	if f.Values != nil {
		s.values = f.Values
	}
	return s, nil
}

// save writes a temp file next to the real one and renames it over, so a
// crash leaves either the old settings or the new ones, never half of each.
func (s *Store) save() error {
	b, err := json.MarshalIndent(storeFile{Version: storeVersion, Values: s.values}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
