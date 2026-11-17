package config

import (
	"bytes"
	"errors"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// ConfigWriteMu serializes ConfigV8 read/ETag/write and runtime updates.
var ConfigWriteMu sync.Mutex

var ErrConfigWriteConflict = errors.New("config bytes changed during update")

// UpdateConfigFile mutates the latest on-disk configuration under the same
// lock as ConfigV8. The callback returns nil to leave it unchanged.
func UpdateConfigFile(path string, mutate func([]byte) ([]byte, error)) error {
	ConfigWriteMu.Lock()
	defer ConfigWriteMu.Unlock()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return errRead
	}
	data, errMutate := mutate(raw)
	if errMutate != nil || data == nil {
		return errMutate
	}
	return writeConfigFile(path, data, func() error {
		current, errCurrent := os.ReadFile(path)
		if errCurrent != nil {
			return errCurrent
		}
		if !bytes.Equal(raw, current) {
			return ErrConfigWriteConflict
		}
		return nil
	})
}

// WriteConfigFile saves local config bytes; the config watcher reloads and mirrors
// them to any configured token store. Writes retain the inode of mounted files.
func WriteConfigFile(path string, data []byte) error {
	return writeConfigFile(path, data, nil)
}

func writeConfigFile(path string, data []byte, check func() error) error {
	var doc yaml.Node
	if errParse := yaml.Unmarshal(data, &doc); errParse != nil {
		return errParse
	}
	if len(doc.Content) > 0 && IsV8ConfigLayout(doc.Content[0]) {
		var errNormalize error
		data, _, errNormalize = NormalizeConfigLayout(data, true)
		if errNormalize != nil {
			return errNormalize
		}
	}
	data = NormalizeCommentIndentation(data)
	if check != nil {
		if errCheck := check(); errCheck != nil {
			return errCheck
		}
	}
	f, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if errOpen != nil {
		return errOpen
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}
