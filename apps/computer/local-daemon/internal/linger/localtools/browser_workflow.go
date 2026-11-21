package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/simplexai-labs/linger/apps/computer/local-daemon/internal/permission"
	"github.com/simplexai-labs/linger/apps/computer/local-daemon/internal/platform/privatefs"
)

const (
	browserRunStepsTool         = "browser_run_steps"
	browserWorkflowFileVersion  = 1
	maxBrowserWorkflowSteps     = 20
	maxBrowserWorkflowWaitMs    = 5000
	browserWorkflowPoll         = 200 * time.Millisecond
	browserWorkflowMaxFileBytes = 1 << 20
)

var browserWorkflowName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var browserWorkflowParameter = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

type browserWorkflowStep struct {
	ID         string          `json:"id,omitempty"`
	Tool       string          `json:"tool"`
	Args       map[string]any  `json:"args,omitempty"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	WaitMs     int             `json:"wait_ms,omitempty"`
	Checks     []browserCheck  `json:"-"`
}

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

type browserWorkflowRequest struct {
	Steps      []browserWorkflowStep `json:"steps"`
	Workflow   string                `json:"workflow"`
	Save       bool                  `json:"save"`
	Params     map[string]string     `json:"params"`
	Parameters map[string]string     `json:"parameters"`
	StartStep  int                   `json:"start_step"`
}

type browserPreparedWorkflow struct {
	Name       string
	Version    int
	Steps      []browserWorkflowStep
	RawSteps   []browserWorkflowStep
	Parameters map[string]string
	Save       bool
	Start      int
}

type browserWorkflowStepResult struct {
	Index            int                  `json:"index"`
	ID               string               `json:"id"`
	Tool             string               `json:"tool"`
	Status           string               `json:"status"`
	CheckpointPassed bool                 `json:"checkpoint_passed,omitempty"`
	Assertions       []browserCheckResult `json:"assertions,omitempty"`
	Error            string               `json:"error,omitempty"`
}

type browserWorkflowReport struct {
	Status            string                      `json:"status"`
	Workflow          string                      `json:"workflow,omitempty"`
	Version           int                         `json:"version,omitempty"`
	StepsCompleted    int                         `json:"steps_completed"`
	StepsTotal        int                         `json:"steps_total"`
	CheckpointsPassed int                         `json:"checkpoints_passed"`
	FailedStep        string                      `json:"failed_step,omitempty"`
	FailedStepIndex   int                         `json:"failed_step_index,omitempty"`
	ResumeFrom        int                         `json:"resume_from,omitempty"`
	Steps             []browserWorkflowStepResult `json:"steps,omitempty"`
	Assertions        []browserCheckResult        `json:"assertions,omitempty"`
	Diff              string                      `json:"diff,omitempty"`
	LastResult        string                      `json:"last_result,omitempty"`
	Error             string                      `json:"error,omitempty"`
	IsError           bool                        `json:"-"`
}

// browserRunStepsToolDefinition describes the one tool that runs a planned
// browser workflow and evaluates its checkpoints.
func browserRunStepsToolDefinition() tool {
	step := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":         map[string]any{"type": "string"},
			"tool":       map[string]any{"type": "string"},
			"args":       map[string]any{"type": "object", "additionalProperties": true},
			"checkpoint": map[string]any{"type": "object", "additionalProperties": true, "description": "A check, a list of checks, or a name-to-check object."},
			"wait_ms":    map[string]any{"type": "integer", "minimum": 0, "maximum": maxBrowserWorkflowWaitMs},
		},
		"required":             []string{"tool"},
		"additionalProperties": false,
	}
	return tool{
		Name:        browserRunStepsTool,
		Description: "Run a planned sequence of browser actions. Each step may carry checkpoint checks; the call stops at the first fail or ambiguous and returns the failed step, its diff and a resume_from index. Save steps as a reusable workflow, fill $NAME parameters, and resume a saved workflow at start_step. A step tool is one of the browser tools except browser_close and browser_run_steps.",
		InputSchema: schema(map[string]any{
			"steps":      map[string]any{"type": "array", "minItems": 1, "maxItems": maxBrowserWorkflowSteps, "items": step},
			"workflow":   map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9_-]*$", "maxLength": 64},
			"save":       map[string]any{"type": "boolean"},
			"params":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"parameters": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"start_step": map[string]any{"type": "integer", "minimum": 1},
		}),
	}
}

// prepareBrowserWorkflow loads, substitutes and validates a workflow without
// writing anything. The read-only callers use it for routing and evidence;
// the tool call saves only after the request is approved.
func prepareBrowserWorkflow(raw json.RawMessage, workspace string) (browserPreparedWorkflow, error) {
	var request browserWorkflowRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return browserPreparedWorkflow{}, fmt.Errorf("browser_run_steps arguments are not JSON: %w", err)
	}
	if request.Workflow != "" && !browserWorkflowName.MatchString(request.Workflow) {
		return browserPreparedWorkflow{}, fmt.Errorf("workflow name must match %s", browserWorkflowName)
	}
	if len(request.Steps) > maxBrowserWorkflowSteps {
		return browserPreparedWorkflow{}, fmt.Errorf("steps must contain at most %d actions", maxBrowserWorkflowSteps)
	}
	if request.Save && request.Workflow == "" {
		return browserPreparedWorkflow{}, fmt.Errorf("save requires a workflow name")
	}
	if request.Save && len(request.Steps) == 0 {
		return browserPreparedWorkflow{}, fmt.Errorf("save requires the steps to store")
	}
	if len(request.Steps) > 0 && request.Workflow != "" && !request.Save {
		return browserPreparedWorkflow{}, fmt.Errorf("pass save: true to store steps under workflow %q, or omit workflow to run them inline", request.Workflow)
	}
	if len(request.Steps) == 0 && request.Workflow == "" {
		return browserPreparedWorkflow{}, fmt.Errorf("give steps to run or a saved workflow to load")
	}

	prepared := browserPreparedWorkflow{Name: request.Workflow, Save: request.Save, Parameters: request.Parameters}
	rawSteps := request.Steps
	if len(rawSteps) == 0 {
		file, err := readBrowserWorkflowFile(workspace, request.Workflow)
		if err != nil {
			return browserPreparedWorkflow{}, err
		}
		rawSteps, prepared.Name, prepared.Version = file.Steps, file.Workflow, file.Version
		prepared.Parameters = file.Parameters
	}
	if len(rawSteps) == 0 {
		return browserPreparedWorkflow{}, fmt.Errorf("workflow %q has no steps", prepared.Name)
	}
	if len(rawSteps) > maxBrowserWorkflowSteps {
		return browserPreparedWorkflow{}, fmt.Errorf("workflow %q has more than %d steps", prepared.Name, maxBrowserWorkflowSteps)
	}
	params := map[string]string{}
	for name, value := range prepared.Parameters {
		params[name] = value
	}
	for name, value := range request.Params {
		params[name] = value
	}

	ids := map[string]bool{}
	for index, step := range rawSteps {
		id := step.ID
		if id == "" {
			id = fmt.Sprintf("step%d", index+1)
		}
		if ids[id] {
			return browserPreparedWorkflow{}, fmt.Errorf("workflow %q repeats step id %q", prepared.Name, id)
		}
		ids[id] = true
		if !browserTools[step.Tool] || step.Tool == "browser_close" || step.Tool == browserRunStepsTool {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q uses unsupported browser tool %q", id, step.Tool)
		}
		if step.WaitMs < 0 || step.WaitMs > maxBrowserWorkflowWaitMs {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q wait_ms must be between 0 and %d", id, maxBrowserWorkflowWaitMs)
		}
		args := step.Args
		if args == nil {
			args = map[string]any{}
		}
		resolvedArgs, err := substituteBrowserWorkflowValue(args, params)
		if err != nil {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q arguments: %w", id, err)
		}
		resolvedMap, ok := resolvedArgs.(map[string]any)
		if !ok {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q arguments must be an object", id)
		}
		var checkpoint json.RawMessage
		if len(step.Checkpoint) > 0 && strings.TrimSpace(string(step.Checkpoint)) != "null" {
			var value any
			if err := json.Unmarshal(step.Checkpoint, &value); err != nil {
				return browserPreparedWorkflow{}, fmt.Errorf("step %q checkpoint is not JSON: %w", id, err)
			}
			resolved, err := substituteBrowserWorkflowValue(value, params)
			if err != nil {
				return browserPreparedWorkflow{}, fmt.Errorf("step %q checkpoint: %w", id, err)
			}
			encoded, err := json.Marshal(resolved)
			if err != nil {
				return browserPreparedWorkflow{}, fmt.Errorf("step %q checkpoint: %w", id, err)
			}
			checkpoint = encoded
		}
		checks, err := parseBrowserChecks(checkpoint)
		if err != nil {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q checkpoint: %w", id, err)
		}
		if len(checkpoint) > 0 && len(checks) == 0 {
			return browserPreparedWorkflow{}, fmt.Errorf("step %q checkpoint has no checks", id)
		}
		prepared.Steps = append(prepared.Steps, browserWorkflowStep{ID: id, Tool: step.Tool, Args: resolvedMap, Checkpoint: checkpoint, WaitMs: step.WaitMs, Checks: checks})
		prepared.RawSteps = append(prepared.RawSteps, browserWorkflowStep{ID: id, Tool: step.Tool, Args: step.Args, Checkpoint: step.Checkpoint, WaitMs: step.WaitMs})
	}
	prepared.Start = request.StartStep
	if prepared.Start == 0 {
		prepared.Start = 1
	}
	if prepared.Start < 1 || prepared.Start > len(prepared.Steps) {
		return browserPreparedWorkflow{}, fmt.Errorf("start_step must be between 1 and %d", len(prepared.Steps))
	}
	return prepared, nil
}

func substituteBrowserWorkflowValue(value any, params map[string]string) (any, error) {
	switch typed := value.(type) {
	case string:
		missing := ""
		replaced := browserWorkflowParameter.ReplaceAllStringFunc(typed, func(match string) string {
			name := match[1:]
			if replacement, ok := params[name]; ok {
				return replacement
			}
			missing = name
			return match
		})
		if missing != "" {
			return nil, fmt.Errorf("parameter $%s is missing and has no default", missing)
		}
		return replaced, nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			replaced, err := substituteBrowserWorkflowValue(item, params)
			if err != nil {
				return nil, err
			}
			out[key] = replaced
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			replaced, err := substituteBrowserWorkflowValue(item, params)
			if err != nil {
				return nil, err
			}
			out[index] = replaced
		}
		return out, nil
	default:
		return value, nil
	}
}

func browserWorkflowPath(workspace, name string) string {
	return filepath.Join(workspace, ".linger", "browser-workflows", name+".json")
}

func readBrowserWorkflowFile(workspace, name string) (browserWorkflowFile, error) {
	data, err := privatefs.ReadFile(browserWorkflowPath(workspace, name), browserWorkflowMaxFileBytes)
	if err != nil {
		return browserWorkflowFile{}, fmt.Errorf("workflow %q was not found: %w", name, err)
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
	if file.Version < 1 {
		file.Version = 1
	}
	if len(file.Steps) == 0 {
		return browserWorkflowFile{}, fmt.Errorf("workflow %q has no steps", name)
	}
	return file, nil
}

func saveBrowserWorkflow(workspace, name string, steps []browserWorkflowStep, parameters map[string]string, save bool) (int, error) {
	if !save {
		return 0, nil
	}
	path := browserWorkflowPath(workspace, name)
	version := 1
	stats := browserWorkflowStats{}
	if data, err := privatefs.ReadFile(path, browserWorkflowMaxFileBytes); err == nil {
		var existing browserWorkflowFile
		if err := json.Unmarshal(data, &existing); err != nil {
			return 0, fmt.Errorf("workflow %q is not valid JSON: %w", name, err)
		}
		if existing.SchemaVersion != browserWorkflowFileVersion || existing.Workflow != name {
			return 0, fmt.Errorf("workflow %q has an incompatible saved file", name)
		}
		if existing.Version < 1 {
			existing.Version = 1
		}
		version, stats = existing.Version+1, existing.Stats
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("workflow %q cannot be read: %w", name, err)
	}
	if parameters == nil {
		parameters = map[string]string{}
	}
	file := browserWorkflowFile{
		SchemaVersion: browserWorkflowFileVersion,
		Workflow:      name,
		Version:       version,
		Parameters:    parameters,
		Steps:         steps,
		Stats:         stats,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := privatefs.EnsureDir(filepath.Dir(path)); err != nil {
		return 0, err
	}
	if err := privatefs.WriteFile(path, append(data, '\n'), false); err != nil {
		return 0, err
	}
	return version, nil
}

func recordBrowserWorkflowRun(workspace, name, status string) {
	path := browserWorkflowPath(workspace, name)
	data, err := privatefs.ReadFile(path, browserWorkflowMaxFileBytes)
	if err != nil {
		return
	}
	var file browserWorkflowFile
	if json.Unmarshal(data, &file) != nil {
		return
	}
	if status == "pass" {
		file.Stats.SuccessCount++
	} else {
		file.Stats.FailCount++
	}
	file.Stats.LastStatus = status
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	_ = privatefs.WriteFile(path, append(encoded, '\n'), false)
}

// runBrowserSteps executes one browser_run_steps call under the browser call
// hold and journal that nativeBrowserCall already acquired.
func (op *operation) runBrowserSteps(ctx context.Context, raw json.RawMessage, confirmed *bool) (json.RawMessage, error) {
	*confirmed = true
	prepared, err := prepareBrowserWorkflow(raw, op.spec.WorkingDirectory)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	if err := op.refuseBrowserWorkflow(prepared); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	if op.userChromeSession != nil {
		for index := browserWorkflowStart(prepared) - 1; index < len(prepared.Steps); index++ {
			step := prepared.Steps[index]
			if err := op.userChromeFileUploadRefused(step.Tool, step.Args); err != nil {
				return nil, err
			}
		}
	}
	if prepared.Save {
		version, err := saveBrowserWorkflow(op.spec.WorkingDirectory, prepared.Name, prepared.RawSteps, prepared.Parameters, true)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errNotSent, err)
		}
		prepared.Version = version
	}
	report, runErr := runBrowserWorkflow(ctx, prepared, op.requestBrowser, func() browserObservation {
		return op.browserObservations.latest
	})
	if runErr != nil {
		if errors.Is(runErr, errOutcomeUnknown) || ctx.Err() != nil {
			*confirmed = false
		}
		if prepared.Name != "" {
			recordBrowserWorkflowRun(op.spec.WorkingDirectory, prepared.Name, "error")
		}
		return nil, runErr
	}
	if prepared.Name != "" {
		recordBrowserWorkflowRun(op.spec.WorkingDirectory, prepared.Name, report.Status)
	}
	return browserWorkflowResult(report), nil
}

func runBrowserWorkflow(ctx context.Context, prepared browserPreparedWorkflow, call func(context.Context, string, json.RawMessage) (json.RawMessage, error), latest func() browserObservation) (browserWorkflowReport, error) {
	report := browserWorkflowReport{
		Status:     "pass",
		Workflow:   prepared.Name,
		Version:    prepared.Version,
		StepsTotal: len(prepared.Steps),
	}
	for index := browserWorkflowStart(prepared) - 1; index < len(prepared.Steps); index++ {
		step := prepared.Steps[index]
		args, err := json.Marshal(step.Args)
		if err != nil {
			report.Status, report.IsError, report.Error = "error", true, err.Error()
			report.FailedStep, report.FailedStepIndex, report.ResumeFrom = step.ID, index+1, index+1
			report.Steps = append(report.Steps, browserWorkflowStepResult{Index: index + 1, ID: step.ID, Tool: step.Tool, Status: "error", Error: err.Error()})
			return report, nil //nolint:nilerr // the report carries the marshal error
		}
		before := latest()
		result, err := call(ctx, step.Tool, args)
		if err != nil {
			if errors.Is(err, errOutcomeUnknown) || ctx.Err() != nil {
				return report, err
			}
			return failedBrowserWorkflow(report, step, index, result, "error", err.Error())
		}
		if toolErr := toolResultError(result); toolErr != nil {
			return failedBrowserWorkflow(report, step, index, result, "error", toolErr.Error())
		}
		lastResult := result
		after := latest()
		diff := browserStepDiff(step.Tool, before, after)
		allPassed, checkError, checks := evaluateBrowserChecks(step.Checks, after, diff)
		if checkError {
			report, _ = failedBrowserWorkflow(report, step, index, result, "error", "a checkpoint could not be evaluated")
			report.Assertions = checks
			report.Steps[len(report.Steps)-1].Assertions = checks
			return report, nil
		}
		if len(step.Checks) > 0 && !allPassed && step.WaitMs > 0 {
			deadline := time.Now().Add(time.Duration(step.WaitMs) * time.Millisecond)
			for !allPassed && time.Now().Before(deadline) {
				if err := sleepContext(ctx, browserWorkflowPoll); err != nil {
					return failedBrowserWorkflow(report, step, index, lastResult, "error", err.Error())
				}
				refresh, err := call(ctx, "browser_snapshot", json.RawMessage(`{}`))
				if err != nil {
					if errors.Is(err, errOutcomeUnknown) || ctx.Err() != nil {
						return report, err
					}
					return failedBrowserWorkflow(report, step, index, lastResult, "error", err.Error())
				}
				if toolErr := toolResultError(refresh); toolErr != nil {
					return failedBrowserWorkflow(report, step, index, lastResult, "error", toolErr.Error())
				}
				lastResult = refresh
				after = latest()
				diff = browserStepDiff(step.Tool, before, after)
				allPassed, checkError, checks = evaluateBrowserChecks(step.Checks, after, diff)
				if checkError {
					report, _ = failedBrowserWorkflow(report, step, index, lastResult, "error", "a checkpoint could not be evaluated")
					report.Assertions = checks
					report.Steps[len(report.Steps)-1].Assertions = checks
					return report, nil
				}
			}
		}
		stepResult := browserWorkflowStepResult{Index: index + 1, ID: step.ID, Tool: step.Tool, Assertions: checks}
		if len(step.Checks) > 0 {
			stepResult.CheckpointPassed = allPassed
			if allPassed {
				report.CheckpointsPassed++
			}
		}
		if allPassed {
			stepResult.Status = "pass"
			report.StepsCompleted++
			report.Steps = append(report.Steps, stepResult)
			report.LastResult = browserWorkflowResultText(lastResult)
			continue
		}
		if diff.meaningful() {
			stepResult.Status = "ambiguous"
		} else {
			stepResult.Status = "fail"
		}
		report.Status = stepResult.Status
		report.FailedStep, report.FailedStepIndex, report.ResumeFrom = step.ID, index+1, index+1
		report.Assertions = checks
		report.Diff = diff.render()
		report.LastResult = browserWorkflowResultText(lastResult)
		report.Steps = append(report.Steps, stepResult)
		return report, nil
	}
	return report, nil
}

func failedBrowserWorkflow(report browserWorkflowReport, step browserWorkflowStep, index int, result json.RawMessage, status, message string) (browserWorkflowReport, error) {
	report.Status, report.IsError, report.Error = status, true, message
	report.FailedStep, report.FailedStepIndex, report.ResumeFrom = step.ID, index+1, index+1
	report.Steps = append(report.Steps, browserWorkflowStepResult{Index: index + 1, ID: step.ID, Tool: step.Tool, Status: status, Error: message})
	report.LastResult = browserWorkflowResultText(result)
	return report, nil
}

func browserWorkflowResultText(result json.RawMessage) string {
	return strings.Join(resultTexts(result), "\n")
}

func (report browserWorkflowReport) text() string {
	var out strings.Builder
	out.WriteString("### Workflow result\n")
	fmt.Fprintf(&out, "- status: %s\n", report.Status)
	if report.Workflow != "" {
		if report.Version > 0 {
			fmt.Fprintf(&out, "- workflow: %s (v%d)\n", report.Workflow, report.Version)
		} else {
			fmt.Fprintf(&out, "- workflow: %s\n", report.Workflow)
		}
	}
	fmt.Fprintf(&out, "- steps completed: %d/%d\n", report.StepsCompleted, report.StepsTotal)
	if report.CheckpointsPassed > 0 {
		fmt.Fprintf(&out, "- checkpoints passed: %d\n", report.CheckpointsPassed)
	}
	for _, step := range report.Steps {
		fmt.Fprintf(&out, "- step %d (%s: %s): %s", step.Index, step.ID, step.Tool, step.Status)
		if step.CheckpointPassed {
			out.WriteString("; checkpoint pass")
		}
		if step.Error != "" {
			fmt.Fprintf(&out, "; error: %s", step.Error)
		}
		out.WriteString("\n")
	}
	if report.FailedStep != "" {
		fmt.Fprintf(&out, "- failed step: %d (%s)\n", report.FailedStepIndex, report.FailedStep)
		fmt.Fprintf(&out, "- resume_from: %d\n", report.ResumeFrom)
	}
	if report.Error != "" {
		fmt.Fprintf(&out, "- error: %s\n", report.Error)
	}
	if len(report.Assertions) > 0 {
		out.WriteString("- assertions:\n")
		for _, check := range report.Assertions {
			mark := "PASS"
			if check.Error != "" {
				mark = "ERROR"
			} else if !check.Passed {
				mark = "FAIL"
			}
			fmt.Fprintf(&out, "  - %s %s (%s %s): expected %s, actual %s\n",
				mark, check.Name, check.Read, check.Op, browserValueText(check.Expected), browserValueText(check.Actual))
			if check.Error != "" {
				fmt.Fprintf(&out, "    error: %s\n", check.Error)
			}
		}
	}
	if report.Diff != "" && !strings.Contains(report.LastResult, "### Snapshot diff") {
		out.WriteString("\n### Step diff\n")
		out.WriteString(report.Diff)
		if !strings.HasSuffix(report.Diff, "\n") {
			out.WriteString("\n")
		}
	}
	if report.LastResult != "" {
		out.WriteString("\n### Last step result\n")
		out.WriteString(report.LastResult)
		if !strings.HasSuffix(report.LastResult, "\n") {
			out.WriteString("\n")
		}
	}
	return out.String()
}

func browserValueText(value any) string {
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	default:
		if encoded, err := json.Marshal(typed); err == nil {
			text = string(encoded)
		} else {
			text = fmt.Sprint(typed)
		}
	}
	if runes := []rune(text); len(runes) > 240 {
		text = string(runes[:240]) + "..."
	}
	return text
}

func browserWorkflowStart(prepared browserPreparedWorkflow) int {
	if prepared.Start < 1 {
		return 1
	}
	return prepared.Start
}

// browserWorkflowURL is the address a step opens, if it names one.
func browserWorkflowURL(step browserWorkflowStep) string {
	switch step.Tool {
	case "browser_navigate":
		address, _ := step.Args["url"].(string)
		return address
	case "browser_tabs":
		action, _ := step.Args["action"].(string)
		if action != "new" {
			return ""
		}
		address, _ := step.Args["url"].(string)
		return address
	default:
		return ""
	}
}

// browserWorkflowEvidence names every step target and address the approver
// needs, without adding fields to permission.Browser.
func browserWorkflowEvidence(prepared browserPreparedWorkflow, pages *browserPages) *permission.Browser {
	page, title := pages.current()
	if page == "" {
		return nil
	}
	browser := &permission.Browser{URL: page, Title: title}
	var fields []string
	for index, step := range prepared.Steps {
		if index+1 < browserWorkflowStart(prepared) {
			continue
		}
		label := fmt.Sprintf("step %d (%s)", index+1, step.Tool)
		if address := browserWorkflowURL(step); address != "" {
			fields = append(fields, label+": "+address)
		}
		for _, target := range browserStepTargets(step) {
			if role, name, ok := pages.element(target); ok {
				fields = append(fields, label+": "+role+" "+strconv.Quote(name))
				if browser.TargetRole == "" {
					browser.TargetRole, browser.TargetName = role, name
				}
			} else {
				fields = append(fields, label+": target "+target)
			}
		}
	}
	browser.FormFields = strings.Join(fields, "\n")
	return browser
}

func browserStepTargets(step browserWorkflowStep) []string {
	var targets []string
	add := func(value any) {
		if text, ok := value.(string); ok && text != "" {
			targets = append(targets, text)
		}
	}
	add(step.Args["target"])
	add(step.Args["startTarget"])
	add(step.Args["endTarget"])
	if fields, ok := step.Args["fields"].([]any); ok {
		for _, item := range fields {
			if field, ok := item.(map[string]any); ok {
				add(field["target"])
			}
		}
	}
	return targets
}

func (op *operation) userChromeFileUploadRefused(name string, args map[string]any) error {
	if op.userChrome == nil || name != "browser_file_upload" || op.manager == nil {
		return nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("browser_file_upload arguments cannot be checked: %w", err)
	}
	if !uploadsFiles(encoded) {
		return nil
	}
	data := chromeUserDataDirectory(op.manager.env)
	if data == "" {
		return nil
	}
	if extensionFileAccessOff(filepath.Join(data, op.userChrome.Profile)) {
		return errUserChromeFileAccess
	}
	return nil
}

// browserWorkflowResult is the MCP result of a completed workflow.
func browserWorkflowResult(report browserWorkflowReport) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": report.text()}},
		"isError": report.IsError,
	})
	return encoded
}
