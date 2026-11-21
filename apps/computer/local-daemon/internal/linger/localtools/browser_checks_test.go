package localtools

import (
	"encoding/json"
	"strings"
	"testing"
)

func browserSnapshotObservation(t *testing.T, text string) browserObservation {
	t.Helper()
	var observations browserObservations
	observations.observeAction("browser_snapshot", nil, browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", text,
	)), "", false)
	return observations.latest
}

func TestBrowserChecksParseShorthandAndStructured(t *testing.T) {
	checks, err := parseBrowserChecks(json.RawMessage(`{
		"url_contains": "/cart",
		"title_eq": "Cart",
		"text_contains": "Pay",
		"dialog": {"read": "role", "role": "dialog", "label": "Confirm", "op": "exists"},
		"mail": {"read": "value", "target": "e2", "op": "contains", "expected": "@"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 5 {
		t.Fatalf("checks=%d: %+v", len(checks), checks)
	}
	byName := map[string]browserCheck{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if byName["url_contains"].Read != "url" || byName["url_contains"].Op != "contains" {
		t.Fatalf("shorthand=%+v", byName["url_contains"])
	}
	if byName["dialog"].Read != "role" || byName["dialog"].Label != "Confirm" {
		t.Fatalf("structured role=%+v", byName["dialog"])
	}
	if byName["mail"].Read != "value" || byName["mail"].Target != "e2" {
		t.Fatalf("structured value=%+v", byName["mail"])
	}
}

func TestBrowserChecksRejectUnknownFieldsAndBadRegex(t *testing.T) {
	if _, err := parseBrowserChecks(json.RawMessage(`{"url_contains": "/cart", "extra": true}`)); err == nil {
		t.Fatal("unknown shorthand name accepted")
	}
	if _, err := parseBrowserChecks(json.RawMessage(`{"read":"url","op":"contains","expected":"x","unknown":true}`)); err == nil {
		t.Fatal("unknown structured field accepted")
	}
	if _, err := parseBrowserChecks(json.RawMessage(`{"read":"url","op":"regex","expected":"["}`)); err == nil {
		t.Fatal("invalid regex accepted")
	}
}

func TestBrowserChecksEvaluateSnapshotAndDiff(t *testing.T) {
	observation := browserSnapshotObservation(t, "- main [ref=e1]:\n  - button \"Pay\" [ref=e2]\n  - textbox \"Email\" [ref=e3]:\n    - text: a@b.com")
	diff := browserStepDiff("browser_click", observation, observation)
	checks, err := parseBrowserChecks(json.RawMessage(`[
		{"name":"url","read":"url","op":"contains","expected":"/cart"},
		{"name":"pay","read":"text","op":"contains","expected":"Pay"},
		{"name":"button","read":"role","role":"button","label":"Pay","op":"exists"},
		{"name":"email","read":"value","target":"e3","op":"eq","expected":"a@b.com"},
		{"name":"nochange","read":"diff","path":"meaningful","op":"eq","expected":false}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	passed, hasError, results := evaluateBrowserChecks(checks, observation, diff)
	if !passed || hasError {
		t.Fatalf("checks failed: %+v", results)
	}
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("check %+v", result)
		}
	}
}

func TestBrowserChecksDistinguishCountAndContains(t *testing.T) {
	observation := browserSnapshotObservation(t, "- main [ref=e1]:\n  - button \"One\" [ref=e2]\n  - button \"Two\" [ref=e3]")
	checks, err := parseBrowserChecks(json.RawMessage(`[
		{"read":"role","role":"button","op":"count_eq","expected":2},
		{"read":"role","role":"button","op":"contains","expected":"Two"},
		{"read":"role","role":"button","op":"not_exists","expected":null}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	passed, _, results := evaluateBrowserChecks(checks, observation, browserObservationDiff{})
	if passed {
		t.Fatalf("not_exists should fail: %+v", results)
	}
	if !results[0].Passed || !results[1].Passed || results[2].Passed {
		t.Fatalf("results=%+v", results)
	}
}

func TestBrowserStepDiffMarksPageChange(t *testing.T) {
	after := browserSnapshotObservation(t, "- main [ref=e1]:\n  - button \"Next\" [ref=e2]")
	diff := browserStepDiff("browser_click", browserObservation{}, after)
	if !diff.pageChanged {
		t.Fatal("a first page read was not marked as a page change")
	}
	if !diff.meaningful() {
		t.Fatal("a page change was not meaningful")
	}
}

func TestBrowserChecksKeepContextForTextChanges(t *testing.T) {
	before := browserSnapshotObservation(t, "- main [ref=e1]:\n  - textbox \"Email\" [ref=e2]:\n    - text: a@b.com")
	after := browserSnapshotObservation(t, "- main [ref=e1]:\n  - textbox \"Email\" [ref=e2]:\n    - text: c@d.com")
	diff := browserStepDiff("browser_type", before, after)
	if diff.tree.rows == 0 {
		t.Fatal("a value change produced no tree rows")
	}
	// The branch diff must keep the textbox as the parent so a reader knows
	// which field changed.
	if !strings.Contains(diff.render(), `textbox "Email"`) {
		t.Fatalf("value change lost its parent: %s", diff.render())
	}
}
