package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type appliedConfig struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Content  json.RawMessage `json:"content"`
}

type ConfigApplier struct {
	mu      sync.Mutex
	dir     string
	apply   func(context.Context, json.RawMessage) (func() error, error)
	current appliedConfig
	durable bool
}

func NewConfigApplier(dir string, apply func(context.Context, json.RawMessage) (func() error, error)) *ConfigApplier {
	return &ConfigApplier{dir: dir, apply: apply}
}

func (a *ConfigApplier) Apply(ctx context.Context, version int, content string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := configRecord(version, []byte(content))
	if err != nil {
		return err
	}
	if version < a.current.Version {
		return errors.New("configuration version is older than the applied version")
	}
	if version == a.current.Version {
		if record.Checksum != a.current.Checksum {
			return errors.New("configuration content conflicts with the applied version")
		}
		if !a.durable {
			if err := syncConfigDir(a.dir); err != nil {
				return err
			}
			a.durable = true
		}
		return nil
	}
	if _, err := a.writeRecord("l0-config-received.json", record); err != nil {
		return fmt.Errorf("persist received configuration: %w", err)
	}
	rollback, err := a.apply(ctx, record.Content)
	if err != nil {
		return fmt.Errorf("apply runtime configuration: %w", err)
	}
	committed, err := a.writeRecord("l0-config-applied.json", record)
	if committed {
		// A failed directory sync leaves a valid running version that must be durably confirmed before ACK.
		a.current, a.durable = record, err == nil
		if err != nil {
			return fmt.Errorf("confirm applied configuration durability: %w", err)
		}
		return nil
	}
	return errors.Join(fmt.Errorf("persist applied configuration: %w", err), rollback())
}

func (a *ConfigApplier) Restore(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(a.dir, "l0-config-applied.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read last-good configuration: %w", err)
	}
	var record appliedConfig
	if err := json.Unmarshal(data, &record); err != nil {
		return errors.New("invalid last-good configuration record")
	}
	checked, err := configRecord(record.Version, record.Content)
	if err != nil || checked.Checksum != record.Checksum {
		return errors.New("last-good configuration checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := a.apply(ctx, checked.Content); err != nil {
		return fmt.Errorf("restore last-good runtime: %w", err)
	}
	a.current, a.durable = checked, true
	return nil
}

func configRecord(version int, content []byte) (appliedConfig, error) {
	if version < 1 || len(content) > 4<<20 {
		return appliedConfig{}, errors.New("invalid configuration version or size")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		return appliedConfig{}, errors.New("invalid configuration JSON")
	}
	digest := sha256.Sum256(compact.Bytes())
	return appliedConfig{Version: version, Checksum: hex.EncodeToString(digest[:]), Content: compact.Bytes()}, nil
}

func (a *ConfigApplier) writeRecord(name string, record appliedConfig) (bool, error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(a.dir, ".l0-config-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), filepath.Join(a.dir, name)); err != nil {
		return false, err
	}
	return true, syncConfigDir(a.dir)
}

func syncConfigDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
