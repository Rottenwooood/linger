package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplexai-labs/linger/apps/computer/local-daemon/internal/platform/privatefs"
)

func TestBrowserWorkflowStoreSerializesVersionsAndStatistics(t *testing.T) {
	workspace := t.TempDir()
	steps := []browserWorkflowStep{{Tool: "browser_snapshot"}}
	const saves = 16
	start := make(chan struct{})
	versions := make(chan int, saves)
	var pending sync.WaitGroup
	for range saves {
		pending.Go(func() {
			<-start
			version, err := saveBrowserWorkflow(t.Context(), workspace, "checkout", steps, nil)
			if err != nil {
				t.Error(err)
				return
			}
			versions <- version
		})
	}
	close(start)
	pending.Wait()
	close(versions)
	seen := map[int]bool{}
	for version := range versions {
		if version < 1 || version > saves || seen[version] {
			t.Errorf("unexpected version %d", version)
		}
		seen[version] = true
	}
	if len(seen) != saves {
		t.Fatalf("saved %d distinct versions, want %d", len(seen), saves)
	}

	const runsPerStatus = 8
	start = make(chan struct{})
	for range runsPerStatus {
		for _, status := range []string{"pass", "ambiguous"} {
			pending.Go(func() {
				<-start
				if err := recordBrowserWorkflowRun(t.Context(), workspace, "checkout", saves, status); err != nil {
					t.Error(err)
				}
			})
		}
	}
	close(start)
	pending.Wait()
	file, err := readBrowserWorkflowFile(workspace, "checkout")
	if err != nil || file.Version != saves || file.Stats.SuccessCount != runsPerStatus || file.Stats.FailCount != runsPerStatus {
		t.Fatalf("saved version=%d, stats=%+v, error=%v", file.Version, file.Stats, err)
	}
}

func TestBrowserWorkflowStoreRejectsOversizedReplacement(t *testing.T) {
	workspace := t.TempDir()
	const name = "checkout"
	steps := []browserWorkflowStep{{Tool: "browser_snapshot"}}
	if _, err := saveBrowserWorkflow(t.Context(), workspace, name, steps, nil); err != nil {
		t.Fatal(err)
	}

	requestSteps := make([]browserWorkflowStep, maxBrowserWorkflowSteps)
	for index := range requestSteps {
		requestSteps[index].Tool = "browser_snapshot"
	}
	request, err := json.Marshal(browserWorkflowRequest{
		Workflow:   name,
		Save:       true,
		Steps:      requestSteps,
		Parameters: map[string]string{"TEXT": strings.Repeat("a", browserWorkflowMaxFileBytes-1400)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request) >= browserWorkflowMaxFileBytes {
		t.Fatalf("the oversized file fixture should fit in a tool request, got %d bytes", len(request))
	}
	plan, err := prepareBrowserWorkflow(request, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveBrowserWorkflow(t.Context(), workspace, name, plan.RawSteps, plan.Parameters); err == nil {
		t.Fatal("an oversized template replaced the saved version")
	}
	existing, err := readBrowserWorkflowFile(workspace, name)
	if err != nil || existing.Version != 1 || len(existing.Steps) != 1 || len(existing.Parameters) != 0 {
		t.Fatalf("valid template was lost: version=%d, steps=%d, parameters=%d, error=%v",
			existing.Version, len(existing.Steps), len(existing.Parameters), err)
	}
}

func TestBrowserWorkflowStoreLeavesSpaceForRunStatistics(t *testing.T) {
	workspace := t.TempDir()
	const name = "checkout"
	steps := []browserWorkflowStep{{Tool: "browser_snapshot"}}
	parameters := map[string]string{"TEXT": ""}
	file := browserWorkflowFile{SchemaVersion: browserWorkflowFileVersion, Workflow: name, Version: 1, Steps: steps, Parameters: parameters}
	empty, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	parameters["TEXT"] = strings.Repeat("a", browserWorkflowMaxFileBytes-browserWorkflowMetadataBytes-len(empty)-1)
	version, err := saveBrowserWorkflow(t.Context(), workspace, name, steps, parameters)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordBrowserWorkflowRun(t.Context(), workspace, name, version, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	readback, err := readBrowserWorkflowFile(workspace, name)
	if err != nil || readback.Stats.FailCount != 1 || readback.Stats.LastStatus != "ambiguous" {
		t.Fatalf("statistics did not fit beside the template: %+v, %v", readback.Stats, err)
	}
}

func TestBrowserWorkflowStoreWaitForLockHonorsCancellation(t *testing.T) {
	workspace := t.TempDir()
	const name = "checkout"
	steps := []browserWorkflowStep{{Tool: "browser_snapshot"}}
	if _, err := saveBrowserWorkflow(t.Context(), workspace, name, steps, nil); err != nil {
		t.Fatal(err)
	}
	lock, err := lockBrowserWorkflow(t.Context(), browserWorkflowPath(workspace, name))
	if err != nil {
		t.Fatal(err)
	}
	defer privatefs.ReleaseLock(lock)

	wait, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if _, err := saveBrowserWorkflow(wait, workspace, name, steps, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended save did not honor its deadline: %v", err)
	}
	file, err := readBrowserWorkflowFile(workspace, name)
	if err != nil || file.Version != 1 {
		t.Fatalf("reader did not see the committed version: %d, %v", file.Version, err)
	}
}
