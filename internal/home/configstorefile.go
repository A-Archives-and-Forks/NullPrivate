package home

import (
	"fmt"
	"os"

	"github.com/AdguardTeam/AdGuardHome/internal/aghos"
	"github.com/google/renameio/v2/maybe"
)

// fileConfigStore persists configuration in a local YAML file.
type fileConfigStore struct {
	path string
}

func newFileConfigStore(path string) (store *fileConfigStore) {
	return &fileConfigStore{path: path}
}

// type check
var _ configStore = (*fileConfigStore)(nil)

func (s *fileConfigStore) Exists() (ok bool, err error) {
	_, err = os.Stat(s.path)
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("checking config file: %w", err)
}

func (s *fileConfigStore) Read() (data []byte, err error) {
	data, err = os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	return data, nil
}

func (s *fileConfigStore) Write(data []byte) (err error) {
	err = maybe.WriteFile(s.path, data, aghos.DefaultPermFile)
	if err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}

func (s *fileConfigStore) BootstrapFromFile(_ string) (imported bool, err error) {
	return false, nil
}

func (s *fileConfigStore) Close() (err error) {
	return nil
}
