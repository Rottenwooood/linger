package localtools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A browser check is the ARIA-observable subset of agent-gavel's Check IR.
// expr, html, attr and CSS selectors are absent because this browser channel
// deliberately exposes no arbitrary JavaScript or DOM evaluation.
type browserCheck struct {
	Name     string
	Read     string
	Op       string
	Expected any
	Target   string
	Role     string
	Label    string
	Path     string
}

type browserCheckResult struct {
	Name     string `json:"name,omitempty"`
	Read     string `json:"read"`
	Op       string `json:"op"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Passed   bool   `json:"passed"`
	Error    string `json:"error,omitempty"`
}

type browserCheckActual struct {
	Value  any
	Text   string
	List   []string
	Count  int
	Exists bool
}

var browserCheckReads = map[string]bool{
	"url": true, "title": true, "status": true, "console": true,
	"text": true, "ref": true, "value": true, "role": true, "diff": true,
}

var browserCheckOps = map[string]bool{
	"eq": true, "neq": true, "contains": true, "not_contains": true, "regex": true,
	"exists": true, "not_exists": true, "count_eq": true, "count_gt": true,
	"count_gte": true, "gt": true, "lt": true, "gte": true, "lte": true,
}

var browserCheckDiffPaths = map[string]bool{
	"meaningful": true, "page.changed": true, "title.changed": true,
	"status.changed": true, "console.changed": true, "downloads": true,
	"tabs.added": true, "tabs.removed": true, "tabs.changed": true,
	"elements.added": true, "elements.changed": true, "elements.removed": true,
}

var browserCheckPrefixes = map[string]string{
	"url": "url", "title": "title", "text": "text", "status": "status", "console": "console",
}

// parseBrowserChecks reads every checkpoint shape agent-gavel accepts: one
// check, a list, a name-to-check map, a top-level op, or a checks wrapper.
// It validates every check before the first browser action runs.
func parseBrowserChecks(raw json.RawMessage) ([]browserCheck, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("checkpoint is not JSON: %w", err)
	}
	checks, err := parseBrowserCheckValue(value, "")
	if err != nil {
		return nil, err
	}
	for i := range checks {
		if err := validateBrowserCheck(&checks[i]); err != nil {
			return nil, err
		}
	}
	return checks, nil
}

func parseBrowserCheckValue(value any, name string) ([]browserCheck, error) {
	switch typed := value.(type) {
	case []any:
		var checks []browserCheck
		for _, item := range typed {
			parsed, err := parseBrowserCheckValue(item, name)
			if err != nil {
				return nil, err
			}
			checks = append(checks, parsed...)
		}
		return checks, nil
	case map[string]any:
		if wrapper, ok := typed["checks"]; ok {
			if len(typed) != 1 {
				return nil, fmt.Errorf("checkpoint checks cannot be combined with other fields")
			}
			return parseBrowserCheckValue(wrapper, name)
		}
		if len(typed) == 1 {
			for key, item := range typed {
				if prefix, ok := browserCheckPrefixes[key]; ok {
					check := browserCheck{Name: key, Read: prefix, Op: "eq", Expected: item}
					return []browserCheck{check}, nil
				}
			}
		}
		if op, ok := typed["op"].(string); ok && len(typed) == 2 {
			var read string
			var expected any
			for key, item := range typed {
				if key == "op" {
					continue
				}
				if prefix, ok := browserCheckPrefixes[key]; ok {
					read, expected = prefix, item
				}
			}
			if read != "" && browserCheckOps[op] {
				return []browserCheck{{Name: "check", Read: read, Op: op, Expected: expected}}, nil
			}
		}
		if hasBrowserCheckFields(typed) {
			check, err := parseBrowserStructuredCheck(typed, name)
			if err != nil {
				return nil, err
			}
			return []browserCheck{check}, nil
		}
		var checks []browserCheck
		for key, item := range typed {
			parsed, err := parseBrowserNamedCheck(key, item)
			if err != nil {
				return nil, err
			}
			checks = append(checks, parsed...)
		}
		return checks, nil
	default:
		return nil, fmt.Errorf("checkpoint must be an object or a list")
	}
}

func hasBrowserCheckFields(value map[string]any) bool {
	for _, key := range []string{"read", "op", "expected", "value", "target", "role", "label", "path"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func parseBrowserNamedCheck(name string, value any) ([]browserCheck, error) {
	if typed, ok := value.(map[string]any); ok {
		if !hasBrowserCheckFields(typed) {
			return nil, fmt.Errorf("check %q is not an object with check fields", name)
		}
		check, err := parseBrowserStructuredCheck(typed, name)
		if err != nil {
			return nil, err
		}
		return []browserCheck{check}, nil
	}
	check, err := parseBrowserShorthand(name, value)
	if err != nil {
		return nil, err
	}
	return []browserCheck{check}, nil
}

func parseBrowserShorthand(name string, expected any) (browserCheck, error) {
	if name == "downloads_contains" {
		return browserCheck{Name: name, Read: "diff", Op: "contains", Path: "downloads", Expected: expected}, nil
	}
	if name == "meaningful" {
		return browserCheck{Name: name, Read: "diff", Op: "eq", Path: "meaningful", Expected: expected}, nil
	}
	if prefix, ok := browserCheckPrefixes[name]; ok {
		return browserCheck{Name: name, Read: prefix, Op: "eq", Expected: expected}, nil
	}
	for _, suffix := range []string{"not_contains", "contains", "regex", "neq", "eq"} {
		base, found := strings.CutSuffix(name, "_"+suffix)
		if !found {
			continue
		}
		if prefix, ok := browserCheckPrefixes[base]; ok {
			return browserCheck{Name: name, Read: prefix, Op: suffix, Expected: expected}, nil
		}
	}
	return browserCheck{}, fmt.Errorf("unknown checkpoint %q", name)
}

func parseBrowserStructuredCheck(value map[string]any, defaultName string) (browserCheck, error) {
	check := browserCheck{Name: defaultName, Op: "eq"}
	hasExpected, hasValue := false, false
	for key, item := range value {
		switch key {
		case "name":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check name must be a string")
			}
			if text != "" {
				check.Name = text
			}
		case "read":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check read must be a string")
			}
			check.Read = text
		case "op":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check op must be a string")
			}
			check.Op = text
		case "expected":
			if hasValue {
				return check, fmt.Errorf("check cannot set both expected and value")
			}
			check.Expected, hasExpected = item, true
		case "value":
			if hasExpected {
				return check, fmt.Errorf("check cannot set both expected and value")
			}
			check.Expected, hasValue = item, true
		case "target":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check target must be a string")
			}
			check.Target = text
		case "role":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check role must be a string")
			}
			check.Role = text
		case "label":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check label must be a string")
			}
			check.Label = text
		case "path":
			text, ok := item.(string)
			if !ok {
				return check, fmt.Errorf("check path must be a string")
			}
			check.Path = text
		default:
			return check, fmt.Errorf("check has unknown field %q", key)
		}
	}
	if check.Name == "" {
		check.Name = "check"
	}
	if check.Read == "" {
		switch {
		case check.Path != "":
			check.Read = "diff"
		case check.Role != "":
			check.Read = "role"
		case check.Target != "":
			check.Read = "ref"
		}
	}
	return check, nil
}

func validateBrowserCheck(check *browserCheck) error {
	if check.Name == "" {
		check.Name = "check"
	}
	if !browserCheckReads[check.Read] {
		return fmt.Errorf("check %q has unknown read %q", check.Name, check.Read)
	}
	if !browserCheckOps[check.Op] {
		return fmt.Errorf("check %q has unknown op %q", check.Name, check.Op)
	}
	switch check.Read {
	case "ref", "value":
		if check.Target == "" {
			return fmt.Errorf("check %q reads %s without a target ref", check.Name, check.Read)
		}
	case "role":
		if check.Role == "" {
			return fmt.Errorf("check %q reads role without a role", check.Name)
		}
		if check.Op == "eq" || check.Op == "neq" {
			return fmt.Errorf("check %q cannot compare a role with %s; use exists, count or contains", check.Name, check.Op)
		}
	case "diff":
		if !browserCheckDiffPaths[check.Path] {
			return fmt.Errorf("check %q has unknown diff path %q", check.Name, check.Path)
		}
	case "url", "title", "status", "console":
		switch check.Op {
		case "exists", "not_exists", "count_eq", "count_gt", "count_gte", "gt", "lt", "gte", "lte":
			return fmt.Errorf("check %q cannot use %s on %s", check.Name, check.Op, check.Read)
		}
	}
	if check.Op == "exists" || check.Op == "not_exists" {
		if check.Read != "diff" && check.Target == "" && check.Role == "" {
			return fmt.Errorf("check %q uses %s without a target, role or diff path", check.Name, check.Op)
		}
	}
	if check.Op == "regex" {
		pattern, ok := check.Expected.(string)
		if !ok {
			return fmt.Errorf("check %q uses regex without a string pattern", check.Name)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("check %q has an invalid regex: %w", check.Name, err)
		}
	}
	switch check.Op {
	case "count_eq", "count_gt", "count_gte", "gt", "lt", "gte", "lte":
		if _, ok := browserNumber(check.Expected); !ok {
			return fmt.Errorf("check %q uses %s without a number", check.Name, check.Op)
		}
		if check.Read == "text" && check.Target == "" && check.Role == "" && check.Read != "diff" {
			return fmt.Errorf("check %q counts text without a target or role", check.Name)
		}
	case "eq", "neq", "contains", "not_contains":
		if check.Expected == nil {
			return fmt.Errorf("check %q has no expected value", check.Name)
		}
	}
	return nil
}

func browserNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(typed, 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func browserExpectedString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func browserValuesEqual(actual, expected any) bool {
	if left, ok := browserNumber(actual); ok {
		if right, ok := browserNumber(expected); ok {
			return left == right
		}
	}
	switch typed := actual.(type) {
	case string:
		return typed == browserExpectedString(expected)
	case bool:
		if value, ok := expected.(bool); ok {
			return typed == value
		}
		return typed == (browserExpectedString(expected) == "true")
	case []string:
		values, ok := expected.([]any)
		if !ok || len(values) != len(typed) {
			return false
		}
		for i := range typed {
			if typed[i] != browserExpectedString(values[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(actual) == fmt.Sprint(expected)
	}
}

func readBrowserCheck(check browserCheck, observation browserObservation, diff browserObservationDiff) (browserCheckActual, error) {
	switch check.Read {
	case "url":
		return browserCheckActual{Value: observation.url, Text: observation.url, Exists: observation.url != ""}, nil
	case "title":
		return browserCheckActual{Value: observation.title, Text: observation.title, Exists: observation.title != ""}, nil
	case "status":
		return browserCheckActual{Value: observation.status, Text: observation.status, Exists: observation.status != ""}, nil
	case "console":
		return browserCheckActual{Value: observation.console, Text: observation.console, Exists: observation.console != ""}, nil
	case "diff":
		return readBrowserDiff(check, diff)
	case "text", "ref", "value", "role":
		if observation.snapshot == nil {
			return browserCheckActual{}, fmt.Errorf("check %q reads %s without a page snapshot", check.Name, check.Read)
		}
		switch check.Read {
		case "text":
			return readBrowserText(check, observation.snapshot), nil
		case "ref", "value":
			node := observation.snapshot.findNode(check.Target)
			if node == nil {
				return browserCheckActual{Value: "", Text: "", Exists: false}, nil
			}
			if check.Read == "value" {
				value := browserNodeValue(node)
				return browserCheckActual{Value: value, Text: value, List: []string{value}, Count: 1, Exists: true}, nil
			}
			text := strings.Join(node.rows, "\n")
			return browserCheckActual{Value: text, Text: text, List: []string{text}, Count: 1, Exists: true}, nil
		default:
			return readBrowserRole(check, observation.snapshot), nil
		}
	}
	return browserCheckActual{}, fmt.Errorf("check %q has unknown read %q", check.Name, check.Read)
}

func readBrowserText(check browserCheck, snapshot *browserSnapshot) browserCheckActual {
	if check.Target != "" {
		node := snapshot.findNode(check.Target)
		if node == nil {
			return browserCheckActual{Value: "", Text: "", Exists: false}
		}
		text := strings.Join(node.rows, "\n")
		return browserCheckActual{Value: text, Text: text, List: []string{text}, Count: 1, Exists: true}
	}
	if check.Role != "" {
		actual := readBrowserRole(check, snapshot)
		actual.Value = actual.Text
		return actual
	}
	text := snapshot.text
	return browserCheckActual{Value: text, Text: text, Exists: text != ""}
}

func readBrowserRole(check browserCheck, snapshot *browserSnapshot) browserCheckActual {
	var rows []string
	matches := 0
	for _, ref := range snapshot.order {
		node := snapshot.findNode(ref)
		if node == nil {
			continue
		}
		role, label := browserNodeRoleLabel(node)
		if role != check.Role {
			continue
		}
		if check.Label != "" && label != check.Label {
			continue
		}
		matches++
		rows = append(rows, node.rows...)
	}
	if matches == 0 {
		return browserCheckActual{Value: 0, Text: "", List: nil, Count: 0, Exists: false}
	}
	text := strings.Join(rows, "\n")
	return browserCheckActual{Value: matches, Text: text, List: rows, Count: matches, Exists: true}
}

func browserNodeRoleLabel(node *browserSnapshotNode) (role, label string) {
	if node == nil || len(node.rows) == 0 {
		return "", ""
	}
	line := strings.TrimSpace(node.rows[0])
	line = strings.TrimPrefix(line, "- ")
	role, rest, _ := strings.Cut(line, " ")
	role = strings.TrimSuffix(role, ":")
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, `"`) {
		label, _ = quotedPrefix(rest)
	}
	return role, label
}

func browserNodeValue(node *browserSnapshotNode) string {
	if node == nil {
		return ""
	}
	for _, row := range node.rows[1:] {
		text := strings.TrimSpace(row)
		if !strings.HasPrefix(text, "- text: ") {
			continue
		}
		value := strings.TrimPrefix(text, "- text: ")
		if strings.HasPrefix(value, `"`) {
			if unquoted, ok := quotedPrefix(value); ok {
				return unquoted
			}
		}
		return value
	}
	line := strings.TrimSpace(node.rows[0])
	if index := strings.LastIndex(line, ": "); index >= 0 {
		return line[index+2:]
	}
	return ""
}

func (s *browserSnapshot) findNode(ref string) *browserSnapshotNode {
	if s == nil {
		return nil
	}
	if node := s.nodes[ref]; node != nil {
		return node
	}
	var find func(*browserSnapshotNode) *browserSnapshotNode
	find = func(node *browserSnapshotNode) *browserSnapshotNode {
		if node.ref == ref {
			return node
		}
		for i := range node.children {
			if found := find(&node.children[i]); found != nil {
				return found
			}
		}
		return nil
	}
	return find(&s.root)
}

func readBrowserDiff(check browserCheck, diff browserObservationDiff) (browserCheckActual, error) {
	switch check.Path {
	case "meaningful":
		return browserCheckActual{Value: diff.meaningful(), Text: strconv.FormatBool(diff.meaningful()), Count: boolCount(diff.meaningful()), Exists: diff.meaningful()}, nil
	case "page.changed":
		return browserCheckActual{Value: diff.pageChanged, Text: strconv.FormatBool(diff.pageChanged), Count: boolCount(diff.pageChanged), Exists: diff.pageChanged}, nil
	case "title.changed":
		return browserCheckActual{Value: diff.titleChanged, Text: strconv.FormatBool(diff.titleChanged), Count: boolCount(diff.titleChanged), Exists: diff.titleChanged}, nil
	case "status.changed":
		changed := diff.statusLine != ""
		return browserCheckActual{Value: changed, Text: strconv.FormatBool(changed), Count: boolCount(changed), Exists: changed}, nil
	case "console.changed":
		changed := diff.consoleLine != ""
		return browserCheckActual{Value: changed, Text: strconv.FormatBool(changed), Count: boolCount(changed), Exists: changed}, nil
	case "downloads":
		return browserListActual(diff.downloads), nil
	case "tabs.added":
		return browserListActual(diff.tabsAdded), nil
	case "tabs.removed":
		return browserListActual(diff.tabsRemoved), nil
	case "tabs.changed":
		return browserListActual(diff.tabsChanged), nil
	case "elements.added":
		return browserListActual(browserTreeChangeLines(diff.tree.added)), nil
	case "elements.changed":
		return browserListActual(browserTreeChangeLines(diff.tree.changed)), nil
	case "elements.removed":
		return browserListActual(browserTreeChangeLines(diff.tree.removed)), nil
	default:
		return browserCheckActual{}, fmt.Errorf("check %q has unknown diff path %q", check.Name, check.Path)
	}
}

func browserListActual(values []string) browserCheckActual {
	text := strings.Join(values, "\n")
	return browserCheckActual{Value: values, Text: text, List: values, Count: len(values), Exists: len(values) > 0}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func browserTreeChangeLines(changes []browserTreeChange) []string {
	var lines []string
	for _, change := range changes {
		lines = append(lines, change.rows...)
	}
	return lines
}

func compareBrowserCheck(check browserCheck, actual browserCheckActual) (bool, error) {
	switch check.Op {
	case "exists":
		return actual.Exists, nil
	case "not_exists":
		return !actual.Exists, nil
	case "count_eq", "count_gt", "count_gte", "gt", "lt", "gte", "lte":
		expected, ok := browserNumber(check.Expected)
		if !ok {
			return false, fmt.Errorf("check %q needs a numeric expected value", check.Name)
		}
		value := float64(actual.Count)
		switch check.Op {
		case "count_eq":
			return value == expected, nil
		case "count_gt":
			return value > expected, nil
		case "count_gte":
			return value >= expected, nil
		case "gt":
			return value > expected, nil
		case "lt":
			return value < expected, nil
		case "gte":
			return value >= expected, nil
		default:
			return value <= expected, nil
		}
	case "contains", "not_contains":
		expected := browserExpectedString(check.Expected)
		contains := false
		if len(actual.List) > 0 {
			for _, item := range actual.List {
				if strings.Contains(item, expected) {
					contains = true
					break
				}
			}
		} else {
			contains = strings.Contains(actual.Text, expected)
		}
		if check.Op == "not_contains" {
			return !contains, nil
		}
		return contains, nil
	case "regex":
		pattern, ok := check.Expected.(string)
		if !ok {
			return false, fmt.Errorf("check %q needs a string regex", check.Name)
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return false, err
		}
		return compiled.MatchString(actual.Text), nil
	case "eq":
		return browserValuesEqual(actual.Value, check.Expected), nil
	case "neq":
		return !browserValuesEqual(actual.Value, check.Expected), nil
	default:
		return false, fmt.Errorf("check %q has unknown op %q", check.Name, check.Op)
	}
}

func (actual browserCheckActual) resultValue(check browserCheck) any {
	switch check.Op {
	case "exists", "not_exists":
		return actual.Exists
	case "count_eq", "count_gt", "count_gte", "gt", "lt", "gte", "lte":
		return actual.Count
	case "contains", "not_contains":
		if len(actual.List) > 0 {
			return actual.List
		}
		return actual.Text
	default:
		return actual.Value
	}
}

func evaluateBrowserChecks(checks []browserCheck, observation browserObservation, diff browserObservationDiff) (bool, bool, []browserCheckResult) {
	if len(checks) == 0 {
		return true, false, nil
	}
	results := make([]browserCheckResult, 0, len(checks))
	allPassed, hasError := true, false
	for _, check := range checks {
		result := browserCheckResult{Name: check.Name, Read: check.Read, Op: check.Op, Expected: check.Expected}
		actual, err := readBrowserCheck(check, observation, diff)
		if err != nil {
			result.Error = err.Error()
			allPassed, hasError = false, true
			results = append(results, result)
			continue
		}
		passed, err := compareBrowserCheck(check, actual)
		if err != nil {
			result.Error = err.Error()
			allPassed, hasError = false, true
		} else {
			result.Passed = passed
			if !passed {
				allPassed = false
			}
		}
		result.Actual = actual.resultValue(check)
		results = append(results, result)
	}
	return allPassed, hasError, results
}
