package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigApplierPersistsOnlySuccessfulRuntimeState(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	applier := NewConfigApplier(dir, func(context.Context, json.RawMessage) (func() error, error) {
		calls++
		return func() error { return nil }, nil
	})
	content := `{"targets":[],"host_metrics":{"enabled":true,"interval":"15s"}}`
	if err := applier.Apply(context.Background(), 2, content); err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(context.Background(), 2, content); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate revision restarted runtime %d times", calls)
	}
	data, err := os.ReadFile(filepath.Join(dir, "l0-config-applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Version int             `json:"version"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 2 {
		t.Fatalf("invalid applied record: version=%d err=%v", record.Version, err)
	}
	if err := applier.Apply(context.Background(), 2, `{"targets":[]}`); err == nil {
		t.Fatal("same version with different contents was accepted")
	}
	if err := applier.Apply(context.Background(), 1, content); err == nil {
		t.Fatal("older revision was accepted")
	}
}

func TestConfigApplierRuntimeFailureDoesNotPersistApplied(t *testing.T) {
	dir := t.TempDir()
	applier := NewConfigApplier(dir, func(context.Context, json.RawMessage) (func() error, error) {
		return nil, errors.New("plugin failed")
	})
	if err := applier.Apply(context.Background(), 1, `{"targets":[]}`); err == nil {
		t.Fatal("failed runtime application returned success")
	}
	if _, err := os.Stat(filepath.Join(dir, "l0-config-applied.json")); !os.IsNotExist(err) {
		t.Fatalf("failed application persisted last-good: %v", err)
	}
}

func TestConfigApplierPersistenceFailureRollsBackRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "l0-config-applied.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	rolledBack := false
	applier := NewConfigApplier(dir, func(context.Context, json.RawMessage) (func() error, error) {
		return func() error { rolledBack = true; return nil }, nil
	})
	if err := applier.Apply(context.Background(), 1, `{"targets":[]}`); err == nil || !rolledBack {
		t.Fatalf("persistence failure not rolled back: rollback=%t err=%v", rolledBack, err)
	}
}

func TestConfigApplierRestoreRunsLastGoodBeforeAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	content := `{"targets":[],"host_metrics":{"enabled":false,"interval":"30s"}}`
	first := NewConfigApplier(dir, func(context.Context, json.RawMessage) (func() error, error) {
		return func() error { return nil }, nil
	})
	if err := first.Apply(context.Background(), 4, content); err != nil {
		t.Fatal(err)
	}
	calls := 0
	restarted := NewConfigApplier(dir, func(_ context.Context, got json.RawMessage) (func() error, error) {
		calls++
		if string(got) != content {
			t.Errorf("restored a different configuration: %s", got)
		}
		return func() error { return nil }, nil
	})
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Apply(context.Background(), 4, content); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("restore plus duplicate revision applied %d times", calls)
	}
}
