package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/simplexai-labs/linger/apps/computer/local-daemon/internal/provider"
)

type workflowCall struct {
	t            *testing.T
	observations browserObservations
	results      map[string][]string
	calls        []string
}

func (c *workflowCall) call(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	c.calls = append(c.calls, name)
	texts := c.results[name]
	if len(texts) == 0 {
		c.t.Fatalf("unexpected browser call %s", name)
	}
	text := texts[0]
	c.results[name] = texts[1:]
	result := browserResult(c.t, text)
	return c.observations.observeAction(name, args, result, "", false), nil
}

func (c *workflowCall) latest() browserObservation {
	return c.observations.latest
}

func TestBrowserWorkflowPrepareSubstitutesAndSaves(t *testing.T) {
	workspace := t.TempDir()
	raw := json.RawMessage(`{
		"workflow": "checkout",
		"save": true,
		"params": {"URL": "https://shop.example/cart"},
		"parameters": {"URL": "https://shop.example/cart"},
		"steps": [
			{"id": "open", "tool": "browser_navigate", "args": {"url": "$URL"}},
			{"id": "pay", "tool": "browser_click", "args": {"target": "e2"}, "checkpoint": {"url_contains": "/cart"}}
		]
	}`)
	prepared, err := prepareBrowserWorkflow(raw, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Name != "checkout" || len(prepared.Steps) != 2 || prepared.Start != 1 {
		t.Fatalf("prepared=%+v", prepared)
	}
	if prepared.Steps[0].Args["url"] != "https://shop.example/cart" {
		t.Fatalf("parameter was not substituted: %+v", prepared.Steps[0].Args)
	}
	if prepared.RawSteps[0].Args["url"] != "$URL" {
		t.Fatalf("saved steps lost the placeholder: %+v", prepared.RawSteps[0].Args)
	}
	if len(prepared.Steps[1].Checks) != 1 {
		t.Fatalf("checkpoint was not parsed: %+v", prepared.Steps[1].Checks)
	}
	version, err := saveBrowserWorkflow(workspace, prepared.Name, prepared.RawSteps, prepared.Parameters, true)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("first version=%d", version)
	}
	file, err := readBrowserWorkflowFile(workspace, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if file.Version != 1 || len(file.Steps) != 2 || file.Parameters["URL"] == "" {
		t.Fatalf("file=%+v", file)
	}
	loaded, err := prepareBrowserWorkflow(json.RawMessage(`{"workflow":"checkout","params":{"URL":"https://shop.example/next"}}`), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Steps[0].Args["url"] != "https://shop.example/next" || loaded.Version != 1 {
		t.Fatalf("loaded=%+v", loaded)
	}
	resumed, err := prepareBrowserWorkflow(json.RawMessage(`{"workflow":"checkout","start_step":2}`), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Start != 2 {
		t.Fatalf("start=%d", resumed.Start)
	}
	recordBrowserWorkflowRun(workspace, "checkout", version, "pass")
	version, err = saveBrowserWorkflow(workspace, "checkout", prepared.RawSteps, prepared.Parameters, true)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("second version=%d", version)
	}
	file, err = readBrowserWorkflowFile(workspace, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if file.Stats.SuccessCount != 1 || file.Stats.LastStatus != "pass" {
		t.Fatalf("stats=%+v", file.Stats)
	}
}

func TestBrowserWorkflowRejectsMissingParameter(t *testing.T) {
	_, err := prepareBrowserWorkflow(json.RawMessage(`{
		"steps": [{"tool":"browser_navigate","args":{"url":"$MISSING"}}]
	}`), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "$MISSING") {
		t.Fatalf("missing parameter accepted: %v", err)
	}
}

func TestBrowserWorkflowExecutesTheResolvedPlanFromReview(t *testing.T) {
	workspace := t.TempDir()
	original := []browserWorkflowStep{{Tool: "browser_navigate", Args: map[string]any{"url": "$URL"}}}
	const approvedURL = "https://shop.example/approved"
	if _, err := saveBrowserWorkflow(workspace, "checkout", original, nil, true); err != nil {
		t.Fatal(err)
	}
	op := &operation{spec: provider.OperationSpec{WorkingDirectory: workspace}}
	reviews := 0
	op.reviewWorkflow = func(_ context.Context, input json.RawMessage) error {
		reviews++
		var plan browserPreparedWorkflow
		if err := json.Unmarshal(input, &plan); err != nil {
			return err
		}
		if plan.Version != 1 || plan.Name != "checkout" || browserWorkflowURL(plan.Steps[0]) != approvedURL {
			t.Fatalf("review did not receive the resolved plan: %s", input)
		}
		updated := []browserWorkflowStep{{Tool: "browser_navigate", Args: map[string]any{"url": "https://shop.example/next"}}}
		_, err := saveBrowserWorkflow(workspace, "checkout", updated, nil, true)
		return err
	}
	plan, err := op.prepareReviewedBrowserWorkflow(t.Context(), json.RawMessage(`{"workflow":"checkout","params":{"URL":"`+approvedURL+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	report, err := runBrowserWorkflow(t.Context(), plan, func(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
		calls++
		if name != "browser_navigate" || string(args) != `{"url":"`+approvedURL+`"}` {
			t.Fatalf("execution differs from approval: %s %s", name, args)
		}
		return browserResult(t, "approved page"), nil
	}, func() browserObservation { return browserObservation{} })
	if err != nil || report.Version != 1 || report.Status != "pass" || reviews != 1 || calls != 1 {
		t.Fatalf("approved workflow: %+v, %v, reviews=%d calls=%d", report, err, reviews, calls)
	}
	recordBrowserWorkflowRun(workspace, plan.Name, plan.Version, report.Status)
	file, err := readBrowserWorkflowFile(workspace, plan.Name)
	if err != nil || file.Version != 2 || file.Stats.SuccessCount != 0 {
		t.Fatalf("the newer template was changed by the old run: %+v, %v", file, err)
	}
}

func TestBrowserWorkflowPassAmbiguousAndFail(t *testing.T) {
	tree := "- main [ref=e1]:\n  - button \"Pay\" [ref=e2]"
	passChecks, err := parseBrowserChecks(json.RawMessage(`{"text_contains":"Pay"}`))
	if err != nil {
		t.Fatal(err)
	}
	failChecks, err := parseBrowserChecks(json.RawMessage(`{"text_contains":"Done"}`))
	if err != nil {
		t.Fatal(err)
	}

	pass := &workflowCall{t: t, results: map[string][]string{
		"browser_click": {snapshotPage("https://shop.example/cart", "Cart", tree)},
	}}
	report, err := runBrowserWorkflow(t.Context(), browserPreparedWorkflow{
		Start: 1,
		Steps: []browserWorkflowStep{{ID: "pay", Tool: "browser_click", Args: map[string]any{"target": "e2"}, Checks: passChecks}},
	}, pass.call, pass.latest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || report.StepsCompleted != 1 || report.CheckpointsPassed != 1 {
		t.Fatalf("pass report=%+v", report)
	}
	if !strings.Contains(report.text(), "### Workflow result") || !strings.Contains(report.text(), "### Last step result") {
		t.Fatalf("pass text=%s", report.text())
	}

	ambiguous := &workflowCall{t: t, results: map[string][]string{
		"browser_click": {snapshotPage("https://shop.example/cart", "Cart", tree)},
	}}
	report, err = runBrowserWorkflow(t.Context(), browserPreparedWorkflow{
		Start: 1,
		Steps: []browserWorkflowStep{{ID: "pay", Tool: "browser_click", Args: map[string]any{"target": "e2"}, Checks: failChecks}},
	}, ambiguous.call, ambiguous.latest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ambiguous" || report.ResumeFrom != 0 || len(report.Assertions) != 1 ||
		report.Steps[0].ActionOutcome != browserStepConfirmed {
		t.Fatalf("ambiguous report=%+v", report)
	}
	if !strings.Contains(report.Diff, "page changed") {
		t.Fatalf("ambiguous diff=%s", report.Diff)
	}

	failed := &workflowCall{t: t, results: map[string][]string{
		"browser_snapshot": {snapshotPage("https://shop.example/cart", "Cart", tree)},
	}}
	report, err = runBrowserWorkflow(t.Context(), browserPreparedWorkflow{
		Start: 1,
		Steps: []browserWorkflowStep{{ID: "read", Tool: "browser_snapshot", Checks: failChecks}},
	}, failed.call, failed.latest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || report.ResumeFrom != 0 || report.Steps[0].ActionOutcome != browserStepConfirmed {
		t.Fatalf("fail report=%+v", report)
	}
}

func TestBrowserWorkflowWaitsForSnapshot(t *testing.T) {
	before := snapshotPage("https://shop.example/cart", "Cart", "- main [ref=e1]:\n  - button \"Pay\" [ref=e2]")
	after := snapshotPage("https://shop.example/cart", "Cart", "- main [ref=e1]:\n  - button \"Pay\" [ref=e2]\n  - text \"Done\"")
	checks, err := parseBrowserChecks(json.RawMessage(`{"text_contains":"Done"}`))
	if err != nil {
		t.Fatal(err)
	}
	calls := &workflowCall{t: t, results: map[string][]string{
		"browser_click":    {before},
		"browser_snapshot": {after},
	}}
	report, err := runBrowserWorkflow(t.Context(), browserPreparedWorkflow{
		Start: 1,
		Steps: []browserWorkflowStep{{ID: "pay", Tool: "browser_click", Args: map[string]any{"target": "e2"}, Checks: checks, WaitMs: 600}},
	}, calls.call, calls.latest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || report.CheckpointsPassed != 1 {
		t.Fatalf("report=%+v", report)
	}
	if len(calls.calls) != 2 || calls.calls[1] != "browser_snapshot" {
		t.Fatalf("calls=%v", calls.calls)
	}
	if !strings.Contains(report.LastResult, "Done") {
		t.Fatalf("last result did not include the waited snapshot: %s", report.LastResult)
	}
}

func TestBrowserWorkflowChecksTheSelectedTab(t *testing.T) {
	const cart = "https://shop.example/cart"
	const receipt = "https://shop.example/receipt"
	const tree = "- main [ref=e1]:\n  - heading \"Order\" [ref=e2]"
	calls := &workflowCall{t: t, results: map[string][]string{
		"browser_tabs": {"### Result\n- 0: [Cart](" + cart + ")\n- 1: (current) [Receipt](" + receipt + ")"},
		"browser_snapshot": {"### Open tabs\n- 0: [Cart](" + cart + ")\n- 1: (current) [Receipt](" + receipt + ")\n" +
			snapshotPage(receipt, "Receipt", tree)},
	}}
	calls.observations.observeAction("browser_navigate", nil, browserResult(t, snapshotPage(cart, "Cart", tree)), "", false)
	plan, err := prepareBrowserWorkflow(json.RawMessage(`{"steps":[
		{"tool":"browser_tabs","args":{"action":"select","index":1},"checkpoint":{"url":"`+cart+`"}},
		{"tool":"browser_click","args":{"target":"e2"}}
	]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := runBrowserWorkflow(t.Context(), plan, calls.call, calls.latest)
	if err != nil || report.Status != "ambiguous" || report.ResumeFrom != 0 ||
		len(report.Assertions) != 1 || report.Assertions[0].Actual != receipt ||
		strings.Join(calls.calls, ",") != "browser_tabs,browser_snapshot" {
		t.Fatalf("selected tab was not checked before continuing: %+v %v (%v)", report, err, calls.calls)
	}
}

func TestBrowserWorkflowDiffAssertionsUseAllChangedRefs(t *testing.T) {
	const tree = "- main [ref=e1]:\n  - region \"Inventory\" [ref=e2]:\n    - heading \"Products\" [ref=e3]"
	const billing = "\n  - region \"Billing\" [ref=e4]:\n    - heading \"Address\" [ref=e5]"
	var after strings.Builder
	after.WriteString(tree)
	for i := range maxSnapshotDiffRows + 1 {
		fmt.Fprintf(&after, "\n    - button \"Item %d\" [ref=e%d]", i, i+100)
	}
	after.WriteString(billing + "\n    - link \"Continue\" [ref=e6]:\n      - /url: /checkout")
	before := browserSnapshotObservation(t, tree+billing)
	current := browserSnapshotObservation(t, after.String())
	diff := browserStepDiff("browser_click", before, current)
	checks, err := parseBrowserChecks(json.RawMessage(fmt.Sprintf(`[
		{"read":"diff","path":"elements.added","op":"not_contains","expected":"Continue"},
		{"read":"diff","path":"elements.added","op":"count_eq","expected":%d}
	]`, maxSnapshotDiffRows+2)))
	if err != nil {
		t.Fatal(err)
	}
	passed, unavailable, assertions := evaluateBrowserChecks(checks, current, diff)
	if passed || unavailable || assertions[0].Passed || !assertions[1].Passed || diff.tree.rows != 0 {
		t.Fatalf("large element diff: %+v, %s", assertions, diff.render())
	}
	_, unavailable, assertions = evaluateBrowserChecks(checks, current, browserStepDiff("browser_click", browserObservation{}, current))
	if !unavailable || assertions[0].Passed || assertions[0].Error == "" {
		t.Fatalf("absence was inferred without a before snapshot: %+v", assertions)
	}
}

func TestBrowserWorkflowReportsOnlyUndispatchedActionsAsResumable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workflowCtx := context.WithValue(t.Context(), toolSchedulingKey{}, ctx)
	plan := browserPreparedWorkflow{Steps: []browserWorkflowStep{
		{ID: "first", Tool: "browser_click"},
		{ID: "second", Tool: "browser_click"},
	}}
	calls := 0
	report, err := runBrowserWorkflow(workflowCtx, plan, func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		calls++
		cancel()
		return browserResult(t, "delivered"), nil
	}, func() browserObservation { return browserObservation{} })
	if err != nil || calls != 1 || report.ResumeFrom != 2 ||
		report.Steps[0].ActionOutcome != browserStepConfirmed || report.Steps[1].ActionOutcome != browserStepNotDispatched {
		t.Fatalf("cancelled workflow: %+v, %v (%d calls)", report, err, calls)
	}
}

func TestBrowserWorkflowResumesFromStep(t *testing.T) {
	tree := snapshotPage("https://shop.example/cart", "Cart", "- main [ref=e1]:")
	calls := &workflowCall{t: t, results: map[string][]string{"browser_snapshot": {tree}}}
	report, err := runBrowserWorkflow(t.Context(), browserPreparedWorkflow{
		Start: 2,
		Steps: []browserWorkflowStep{
			{ID: "first", Tool: "browser_click"},
			{ID: "second", Tool: "browser_snapshot"},
		},
	}, calls.call, calls.latest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || report.StepsCompleted != 1 || report.StepsTotal != 2 {
		t.Fatalf("report=%+v", report)
	}
	if len(report.Steps) != 1 || report.Steps[0].Index != 2 || report.Steps[0].ID != "second" {
		t.Fatalf("steps=%+v", report.Steps)
	}
}

func TestBrowserWorkflowEvidenceSummarizesTargets(t *testing.T) {
	prepared := browserPreparedWorkflow{
		Start: 1,
		Steps: []browserWorkflowStep{
			{ID: "open", Tool: "browser_navigate", Args: map[string]any{"url": "https://shop.example/cart"}},
			{ID: "pay", Tool: "browser_click", Args: map[string]any{"target": "e2"}},
			{ID: "fill", Tool: "browser_fill_form", Args: map[string]any{"fields": []any{map[string]any{"target": "e3"}}}},
		},
	}
	pages := &browserPages{url: "https://shop.example/cart", title: "Cart", refs: map[string]string{
		"e2": `button "Pay"`,
		"e3": `textbox "Email"`,
	}}
	browser := browserWorkflowEvidence(prepared, pages)
	if browser.TargetRole != "button" || browser.TargetName != "Pay" {
		t.Fatalf("target=%+v", browser)
	}
	for _, want := range []string{
		`step 1 (browser_navigate): https://shop.example/cart`,
		`step 2 (browser_click): button "Pay"`,
		`step 3 (browser_fill_form): textbox "Email"`,
	} {
		if !strings.Contains(browser.FormFields, want) {
			t.Fatalf("evidence lacks %q: %s", want, browser.FormFields)
		}
	}
}

func TestBrowserAllowedAcceptsReadOnlyWorkflow(t *testing.T) {
	op := &operation{
		spec:  provider.OperationSpec{WorkingDirectory: t.TempDir()},
		pages: browserPages{url: "https://shop.example/cart", links: map[string]bool{"https://shop.example/cart/next": true}},
	}
	readOnly := json.RawMessage(`{
		"steps": [
			{"tool":"browser_snapshot"},
			{"tool":"browser_navigate","args":{"url":"https://shop.example/cart/next"}},
			{"tool":"browser_hover","args":{"target":"e2"}}
		]
	}`)
	if !op.browserAllowed(browserRunStepsTool, readOnly) {
		t.Fatal("read-only workflow was not allowed")
	}
	write := json.RawMessage(`{"steps":[{"tool":"browser_click","args":{"target":"e2"}}]}`)
	if op.browserAllowed(browserRunStepsTool, write) {
		t.Fatal("write workflow was allowed")
	}
	external := json.RawMessage(`{"steps":[{"tool":"browser_navigate","args":{"url":"https://evil.example/"}}]}`)
	if op.browserAllowed(browserRunStepsTool, external) {
		t.Fatal("external navigation was allowed")
	}
	saved := json.RawMessage(`{"workflow":"checkout","save":true,"steps":[{"tool":"browser_snapshot"}]}`)
	if op.browserAllowed(browserRunStepsTool, saved) {
		t.Fatal("saving a workflow was allowed without review")
	}
	if _, err := saveBrowserWorkflow(op.spec.WorkingDirectory, "readonly", []browserWorkflowStep{{Tool: "browser_snapshot"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	if op.browserAllowed(browserRunStepsTool, json.RawMessage(`{"workflow":"readonly"}`)) {
		t.Fatal("a saved workflow was allowed without review")
	}
}

func TestBrowserRefuseWorkflowOnChromeSite(t *testing.T) {
	op := &operation{
		spec:        provider.OperationSpec{WorkingDirectory: t.TempDir()},
		chromeSites: map[string]bool{"example.com": true},
	}
	for _, raw := range []string{
		`{"steps":[{"tool":"browser_navigate","args":{"url":"https://example.com/note"}}]}`,
		`{"steps":[{"tool":"browser_tabs","args":{"action":"new","url":"https://example.com/other"}}]}`,
		`{"steps":[{"tool":"browser_navigate","args":{"url":"$URL"}}],"params":{"URL":"https://example.com/param"}}`,
	} {
		prepared, err := prepareBrowserWorkflow(json.RawMessage(raw), op.spec.WorkingDirectory)
		if err == nil {
			err = op.refuseBrowserWorkflow(prepared)
		}
		if err == nil || !strings.Contains(err.Error(), "COMPUTER_BROWSER_REFUSED") {
			t.Fatalf("protected workflow was accepted: %s: %v", raw, err)
		}
	}
}

func TestBrowserRunStepsToolDefinition(t *testing.T) {
	definition := browserRunStepsToolDefinition()
	if definition.Name != browserRunStepsTool {
		t.Fatalf("name=%q", definition.Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"steps", "workflow", "save", "params", "parameters", "start_step"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("schema lacks %q: %s", name, definition.InputSchema)
		}
	}
}
