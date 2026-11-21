package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/simplexai-labs/linger/apps/computer/local-daemon/internal/platform/privatefs"
)

const (
	browserWorkflowFileVersion  = 1
	browserWorkflowMaxFileBytes = 1 << 20
	// Leave room for the version, both counters, and the last status to grow.
	browserWorkflowMetadataBytes = 128
	browserWorkflowLockRetry     = 10 * time.Millisecond
)

type browserWorkflowFile struct {
	SchemaVersion int                   `json:"schema_version"`
	Workflow      string                `json:"workflow"`
	Version       int                   `json:"version"`
	Parameters    map[string]string     `json:"parameters,omitempty"`
	Steps         []browserWorkflowStep `json:"steps"`
	Stats         browserWorkflowStats  `json:"stats"`
}

type browserWorkflowStats struct {
	SuccessCount int    `json:"success_count"`
	FailCount    int    `json:"fail_count"`
	LastStatus   string `json:"last_status,omitempty"`
}

func browserWorkflowPath(workspace, name string) string {
	return filepath.Join(workspace, ".linger", "browser-workflows", name+".json")
}

func readBrowserWorkflowFile(workspace, name string) (browserWorkflowFile, error) {
	data, err := privatefs.ReadFile(browserWorkflowPath(workspace, name), browserWorkflowMaxFileBytes)
	if err != nil {
		return browserWorkflowFile{}, fmt.Errorf("cannot read workflow %q: %w", name, err)
	}
	var file browserWorkflowFile
	if err := json.Unmarshal(data, &file); err != nil {
		return browserWorkflowFile{}, fmt.Errorf("workflow %q is not valid JSON: %w", name, err)
	}
	if file.SchemaVersion != browserWorkflowFileVersion {
		return browserWorkflowFile{}, fmt.Errorf("workflow %q has unsupported schema_version %d", name, file.SchemaVersion)
	}
	if file.Workflow != name {
		return browserWorkflowFile{}, fmt.Errorf("workflow file %q names %q", name, file.Workflow)
	}
	file.Version = max(file.Version, 1)
	return file, nil
}

func saveBrowserWorkflow(ctx context.Context, workspace, name string, steps []browserWorkflowStep, parameters map[string]string) (int, error) {
	path := browserWorkflowPath(workspace, name)
	if err := privatefs.EnsureDir(filepath.Dir(path)); err != nil {
		return 0, err
	}
	lock, err := lockBrowserWorkflow(ctx, path)
	if err != nil {
		return 0, err
	}
	defer privatefs.ReleaseLock(lock)

	existing, err := readBrowserWorkflowFile(workspace, name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	file := browserWorkflowFile{
		SchemaVersion: browserWorkflowFileVersion,
		Workflow:      name,
		Version:       existing.Version + 1,
		Parameters:    parameters,
		Steps:         steps,
		Stats:         existing.Stats,
	}
	if err := writeBrowserWorkflowFile(path, file, browserWorkflowMaxFileBytes-browserWorkflowMetadataBytes); err != nil {
		return 0, err
	}
	return file.Version, nil
}

func recordBrowserWorkflowRun(ctx context.Context, workspace, name string, version int, status string) error {
	path := browserWorkflowPath(workspace, name)
	lock, err := lockBrowserWorkflow(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer privatefs.ReleaseLock(lock)

	file, err := readBrowserWorkflowFile(workspace, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if file.Version != version {
		return nil
	}
	if status == "pass" {
		file.Stats.SuccessCount++
	} else {
		file.Stats.FailCount++
	}
	file.Stats.LastStatus = status
	return writeBrowserWorkflowFile(path, file, browserWorkflowMaxFileBytes)
}

// The lock has a stable identity across atomic replacement of the JSON file.
// Readers can open the JSON directly because each publication is atomic.
func lockBrowserWorkflow(ctx context.Context, path string) (*os.File, error) {
	for ctx.Err() == nil {
		lock, acquired, err := privatefs.AcquireLock(path + ".lock")
		if err != nil {
			return nil, err
		}
		if acquired {
			return lock, nil
		}
		if err := sleepContext(ctx, browserWorkflowLockRetry); err != nil {
			return nil, err
		}
	}
	return nil, context.Cause(ctx)
}

func writeBrowserWorkflowFile(path string, file browserWorkflowFile, limit int) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > limit {
		return fmt.Errorf("workflow %q exceeds the %d-byte template file budget", file.Workflow, limit)
	}
	return privatefs.WriteFile(path, append(data, '\n'), false)
}
