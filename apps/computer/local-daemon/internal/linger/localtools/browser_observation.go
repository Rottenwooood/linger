package localtools

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// browserObservation is the baseline of the latest full page snapshot and
// the container state around it, so one action can report what it changed
// instead of repeating the whole accessibility tree.
type browserObservation struct {
	hasSnapshot bool
	url         string
	title       string
	status      string
	console     string
	// tabs maps a tab's list index to the line Playwright rendered for it,
	// without the current marker; two tabs on one address stay distinct.
	tabs map[string]string
	// activeTab identifies the current tab by index and address, without
	// its title, so a same-page title change is not a tab switch.
	activeTab string
	// downloads holds the completed files already reported for this page.
	downloads map[string]bool
	// artifacts holds the files Playwright wrote into the output directory,
	// so a directory scan reports only user downloads.
	artifacts map[string]bool
	// refs maps an element reference to its snapshot line, and lines counts
	// every non-empty snapshot line, with or without a reference.
	refs  map[string]string
	lines map[string]int
}

// browserObservationDiff is what one action changed across the three layers
// the Playwright MCP result exposes: the browser context (tabs and
// downloads), the page host (address, title, HTTP status, console), and the
// accessibility tree.
type browserObservationDiff struct {
	urlChanged   bool
	titleChanged bool
	fromTitle    string
	toTitle      string
	statusLine   string
	consoleLine  string
	tabsAdded    []string
	tabsRemoved  []string
	tabsChanged  []string
	activeTab    string
	downloads    []string
	added        []string
	changed      []string
	removed      []string
	removedMore  int
}

// meaningful reports whether any observable layer changed. The compact report
// says no_change only when no layer moved.
func (d browserObservationDiff) meaningful() bool {
	return d.urlChanged || d.titleChanged || d.statusLine != "" || d.consoleLine != "" ||
		len(d.tabsAdded) > 0 || len(d.tabsRemoved) > 0 || len(d.tabsChanged) > 0 ||
		d.activeTab != "" || len(d.downloads) > 0 ||
		len(d.added) > 0 || len(d.changed) > 0 || len(d.removed) > 0
}

// maxDiffElements bounds the lines one compact report carries. A larger
// change is a new page shape, and the full tree is worth its context.
const maxDiffElements = 40

// maxRemovedSamples keeps the disappearance report short: a removed
// reference cannot be used again, so only the count matters beyond a sample.
const maxRemovedSamples = 10

var (
	pageHTTPStatus = regexp.MustCompile(`(?m)^- HTTP status: (.+)$`)
	pageConsole    = regexp.MustCompile(`(?m)^- Console: (.+)$`)
	openTabsBlock  = regexp.MustCompile(`(?ms)^### Open tabs\n(.+?)(?:\n### |\z)`)
	resultBlock    = regexp.MustCompile(`(?ms)^### Result\n(.+?)(?:\n### |\z)`)
	openTabLine    = regexp.MustCompile(`^- (\d+): (?:\(current\) )?\[(.*)\]\((.*)\)(?: \[crashed\])?$`)
	snapshotBlock  = regexp.MustCompile("(?s)### Snapshot\\n```yaml\\n(.*?)\\n```")
	downloadEvent  = regexp.MustCompile(`(?m)^- Downloaded file .+ to "([^"\n]+)"\r?$`)
	markdownLink   = regexp.MustCompile(`\]\(([^)\n]+)\)`)
	consoleLog     = regexp.MustCompile(`^console-.*\.log$`)
)

// observeAction records one Playwright result and, for a same-page
// interaction, replaces the repeated full snapshot with what changed. A
// first visit, a navigation, browser_snapshot, or a change too large to
// summarize keeps the full tree. inlineSnapshot already inlined the tree.
func (p *browserPages) observeAction(name string, result json.RawMessage, output string) json.RawMessage {
	p.observe(result)

	texts := resultTexts(result)
	snapshots := snapshotTexts(texts)
	if len(snapshots) == 0 {
		p.observeTabs(name, texts)
		p.observeArtifacts(texts, output)
		return result
	}

	current := observationFromText(texts, snapshots)
	p.mu.Lock()
	before := p.observation
	p.mu.Unlock()

	mergeArtifacts(&current, before)
	current.downloads = setOf(scanDownloads(output, texts, current.artifacts))
	if current.tabs == nil {
		if current.url == "" {
			// Without a page address the result cannot name the tab; keep
			// the previous one rather than report every tab as closed.
			current.tabs, current.activeTab = before.tabs, before.activeTab
		} else {
			// A snapshot-bearing result lists Open tabs whenever more than
			// one tab is open, so a missing section means one tab.
			current.tabs, current.activeTab = singleTab(current.url, current.title)
		}
	}

	p.mu.Lock()
	p.observation = current
	if !before.hasSnapshot || before.url != current.url {
		// A new page starts a new baseline; old downloads and references
		// belong to the page the Agent left.
		p.mu.Unlock()
		return result
	}
	p.mu.Unlock()

	diff := compareObservation(before, current)

	if !shouldCompact(name, diff) {
		return result
	}
	return compactSnapshot(result, diff)
}

// observeTabs records the tab list of a result that carries no snapshot,
// such as browser_tabs, so the next action does not report these tabs again.
func (p *browserPages) observeTabs(name string, texts []string) {
	tabs, active := parseTabs(texts)
	if tabs == nil && name == "browser_tabs" {
		// browser_tabs prints its list in the Result section; a later page
		// action would re-report every tab without this baseline.
		tabs, active = parseResultTabs(texts)
	}
	if tabs == nil {
		return
	}
	p.mu.Lock()
	if p.observation.activeTab != active {
		// Selecting another tab changes the page even when the address is
		// unchanged; its tree belongs to a new baseline.
		p.observation.hasSnapshot = false
	}
	p.observation.tabs, p.observation.activeTab = tabs, active
	p.mu.Unlock()
}

// shouldCompact reports whether one action's result can replace the full
// accessibility tree with its diff.
func shouldCompact(name string, diff browserObservationDiff) bool {
	switch name {
	case "browser_snapshot", "browser_navigate", "browser_navigate_back", "browser_tabs":
		return false
	}
	if diff.urlChanged || diff.activeTab != "" {
		// A tab switch changes which page the refs belong to, even when the
		// address is the same.
		return false
	}
	return len(diff.added)+len(diff.changed) <= maxDiffElements
}

// compareObservation diffs the current baseline against the previous one.
func compareObservation(before, current browserObservation) browserObservationDiff {
	var diff browserObservationDiff
	diff.urlChanged = before.url != current.url
	if before.title != current.title {
		diff.titleChanged, diff.fromTitle, diff.toTitle = true, before.title, current.title
	}
	if before.status != current.status {
		diff.statusLine = changedLine(before.status, current.status)
	}
	if before.console != current.console {
		diff.consoleLine = changedLine(before.console, current.console)
	}
	if current.activeTab != "" && current.activeTab != before.activeTab {
		diff.activeTab = current.activeTab
	}
	for address := range current.downloads {
		if !before.downloads[address] {
			diff.downloads = append(diff.downloads, address)
		}
	}
	// Tabs are keyed by their list index so two tabs on one address stay
	// distinct; the current marker is not part of the stored line.
	for index, line := range current.tabs {
		old, present := before.tabs[index]
		switch {
		case !present:
			diff.tabsAdded = append(diff.tabsAdded, line)
		case old != line:
			diff.tabsChanged = append(diff.tabsChanged, line)
		}
	}
	for index, line := range before.tabs {
		if _, present := current.tabs[index]; !present {
			diff.tabsRemoved = append(diff.tabsRemoved, line)
		}
	}
	sort.Strings(diff.tabsAdded)
	sort.Strings(diff.tabsRemoved)
	sort.Strings(diff.tabsChanged)
	sort.Strings(diff.downloads)
	compareTree(before, current, &diff)
	return diff
}

// compareTree reports changed references and the lines whose count changed.
// A reference whose line changed is reported once, as changed, and excluded
// from the added and removed line lists.
func compareTree(before, current browserObservation, diff *browserObservationDiff) {
	for ref, line := range current.refs {
		if old, present := before.refs[ref]; present && old != line {
			diff.changed = append(diff.changed, line)
		}
	}
	for ref, line := range current.refs {
		if _, present := before.refs[ref]; !present {
			diff.added = append(diff.added, line)
		}
	}
	removed := make([]string, 0)
	for ref, line := range before.refs {
		if _, present := current.refs[ref]; !present {
			removed = append(removed, line)
		}
	}
	for line, count := range current.lines {
		if count <= before.lines[line] {
			continue
		}
		if !strings.Contains(line, "[ref=") {
			diff.added = append(diff.added, line)
		}
	}
	for line, count := range before.lines {
		if count <= current.lines[line] {
			continue
		}
		// Element lines are the refs map's responsibility; lines holds only
		// text lines, so a ref is never reported as both changed and text.
		if !strings.Contains(line, "[ref=") {
			removed = append(removed, line)
		}
	}
	sort.Strings(diff.added)
	sort.Strings(diff.changed)
	sort.Strings(removed)
	removed = uniqueStrings(removed)
	diff.removed = removed
	if len(removed) > maxRemovedSamples {
		diff.removed, diff.removedMore = removed[:maxRemovedSamples], len(removed)-maxRemovedSamples
	}
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

// indentOf counts the leading spaces of one snapshot line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// observationFromText reads the page header, tabs and full snapshot of one
// result into a baseline.
func observationFromText(texts, snapshots []string) browserObservation {
	observation := browserObservation{
		hasSnapshot: true,
		refs:        map[string]string{},
		lines:       map[string]int{},
		downloads:   map[string]bool{},
		artifacts:   map[string]bool{},
	}
	for _, text := range texts {
		if match := pageURL.FindStringSubmatch(text); match != nil {
			observation.url = strings.TrimSpace(match[1])
		}
		if match := pageTitle.FindStringSubmatch(text); match != nil {
			observation.title = strings.TrimSpace(match[1])
		}
		if match := pageHTTPStatus.FindStringSubmatch(text); match != nil {
			observation.status = strings.TrimSpace(match[1])
		}
		if match := pageConsole.FindStringSubmatch(text); match != nil {
			observation.console = strings.TrimSpace(match[1])
		}
	}
	observation.tabs, observation.activeTab = parseTabs(texts)
	for _, snapshot := range snapshots {
		lines := strings.Split(snapshot, "\n")
		skip := map[int]bool{}
		for index, line := range lines {
			if skip[index] {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			observation.lines[trimmed]++
			match := pageRef.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			value := trimmed
			if strings.HasSuffix(trimmed, ":") {
				parentIndent := indentOf(line)
				for nextIndex := index + 1; nextIndex < len(lines); nextIndex++ {
					next := lines[nextIndex]
					nextTrimmed := strings.TrimSpace(next)
					if nextTrimmed == "" {
						continue
					}
					nextIndent := indentOf(next)
					if nextIndent <= parentIndent {
						break
					}
					if nextIndent == parentIndent+2 && strings.HasPrefix(nextTrimmed, "- /") {
						continue
					}
					if nextIndent == parentIndent+2 && strings.HasPrefix(nextTrimmed, "- text: ") {
						value = strings.TrimSuffix(trimmed, ":") + ": " + strings.TrimPrefix(nextTrimmed, "- text: ")
						skip[nextIndex] = true
					}
					break
				}
			}
			observation.refs[match[2]] = value
		}
	}
	return observation
}

// parseTabs reads the Open tabs section Playwright prints when the tab list
// or one of its headers changed. A missing section means the result did not
// report tabs, not that no tab is open.
func parseTabs(texts []string) (map[string]string, string) {
	var tabs map[string]string
	var active string
	for _, text := range texts {
		block := openTabsBlock.FindStringSubmatch(text)
		if block == nil {
			continue
		}
		tabs, active = parseTabLines(block[1])
	}
	return tabs, active
}

// parseResultTabs reads the tab list browser_tabs prints in the Result
// section when the call carries no snapshot.
func parseResultTabs(texts []string) (map[string]string, string) {
	for _, text := range texts {
		block := resultBlock.FindStringSubmatch(text)
		if block == nil {
			continue
		}
		if tabs, active := parseTabLines(block[1]); tabs != nil {
			return tabs, active
		}
	}
	return nil, ""
}

// parseTabLines keys tabs by their list index, so two tabs on one address
// stay distinct, and stores the line without the current marker.
func parseTabLines(block string) (map[string]string, string) {
	if strings.HasPrefix(strings.TrimSpace(block), "No open tabs.") {
		return map[string]string{}, ""
	}
	tabs := map[string]string{}
	var active string
	for _, raw := range strings.Split(strings.TrimSpace(block), "\n") {
		line := strings.TrimSpace(raw)
		match := openTabLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		normalized := normalizeTabLine(line)
		tabs[match[1]] = normalized
		if strings.Contains(line, ": (current) [") {
			active = match[1] + ": " + match[3]
		}
	}
	if len(tabs) == 0 {
		return nil, ""
	}
	return tabs, active
}

func normalizeTabLine(line string) string {
	return strings.Replace(line, ": (current) [", ": [", 1)
}

// singleTab is the only tab a snapshot-bearing result has when it prints no
// Open tabs section.
func singleTab(url, title string) (map[string]string, string) {
	if url == "" {
		return nil, ""
	}
	line := "- 0: [" + title + "](" + url + ")"
	return map[string]string{"0": line}, "0: " + url
}

// changedLine renders one value that appeared, disappeared or became other.
func changedLine(before, after string) string {
	switch {
	case before == "":
		return after
	case after == "":
		return "none (was " + before + ")"
	default:
		return before + " -> " + after
	}
}

// render returns the compact report that replaces the full tree.
func (d browserObservationDiff) render() string {
	var out strings.Builder
	out.WriteString("### Snapshot diff\n")
	if !d.meaningful() {
		out.WriteString("no_change: the page did not change; existing refs remain valid.\n")
		return out.String()
	}
	if d.titleChanged {
		fmt.Fprintf(&out, "- title: %s -> %s\n", d.fromTitle, d.toTitle)
	}
	if d.statusLine != "" {
		fmt.Fprintf(&out, "- http status: %s\n", d.statusLine)
	}
	if d.consoleLine != "" {
		fmt.Fprintf(&out, "- console: %s\n", d.consoleLine)
	}
	if d.activeTab != "" {
		fmt.Fprintf(&out, "- active tab: %s\n", strings.TrimPrefix(d.activeTab, "- "))
	}
	for _, tab := range d.tabsAdded {
		fmt.Fprintf(&out, "- tabs added: %s\n", tab)
	}
	for _, tab := range d.tabsChanged {
		fmt.Fprintf(&out, "- tabs changed: %s\n", tab)
	}
	for _, tab := range d.tabsRemoved {
		fmt.Fprintf(&out, "- tabs removed: %s\n", tab)
	}
	for _, name := range d.downloads {
		fmt.Fprintf(&out, "- download: %s\n", name)
	}
	if len(d.changed) > 0 {
		out.WriteString("- elements changed:\n")
		for _, line := range d.changed {
			fmt.Fprintf(&out, "  ~ %s\n", line)
		}
	}
	if len(d.added) > 0 {
		out.WriteString("- elements added:\n")
		for _, line := range d.added {
			fmt.Fprintf(&out, "  + %s\n", line)
		}
	}
	if len(d.removed) > 0 {
		out.WriteString("- elements removed:\n")
		for _, line := range d.removed {
			fmt.Fprintf(&out, "  - %s\n", line)
		}
		if d.removedMore > 0 {
			fmt.Fprintf(&out, "  ... %d more removed\n", d.removedMore)
		}
	}
	if len(d.added)+len(d.changed)+len(d.removed) > 0 {
		out.WriteString("Unchanged refs keep their identifiers; call browser_snapshot for the full tree.\n")
	}
	return out.String()
}

// compactSnapshot replaces every inlined snapshot with the compact report.
func compactSnapshot(result json.RawMessage, diff browserObservationDiff) json.RawMessage {
	var reply map[string]any
	if json.Unmarshal(result, &reply) != nil {
		return result
	}
	content, _ := reply["content"].([]any)
	report := diff.render()
	changed := false
	for _, item := range content {
		part, _ := item.(map[string]any)
		text, ok := part["text"].(string)
		if !ok || !strings.Contains(text, "### Snapshot\n```yaml") {
			continue
		}
		part["text"] = snapshotBlock.ReplaceAllLiteralString(text, report)
		changed = true
	}
	if !changed {
		return result
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return result
	}
	return encoded
}

// resultTexts returns the text parts of one MCP tool result.
func resultTexts(result json.RawMessage) []string {
	var reply struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &reply) != nil {
		return nil
	}
	texts := make([]string, 0, len(reply.Content))
	for _, item := range reply.Content {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// snapshotTexts returns every inlined accessibility tree in the result.
func snapshotTexts(texts []string) []string {
	var snapshots []string
	for _, text := range texts {
		for _, match := range snapshotBlock.FindAllStringSubmatch(text, -1) {
			snapshots = append(snapshots, match[1])
		}
	}
	return snapshots
}

// mergeArtifacts keeps every file Playwright wrote in the output directory
// marked as an artifact across pages, so a directory scan never reports one
// as a user download.
func mergeArtifacts(current *browserObservation, before browserObservation) {
	if len(before.artifacts) == 0 {
		return
	}
	if current.artifacts == nil {
		current.artifacts = map[string]bool{}
	}
	for name := range before.artifacts {
		current.artifacts[name] = true
	}
}

// observeArtifacts records links of a result that carries no snapshot, so a
// later directory scan does not report a screenshot or log as a download.
func (p *browserPages) observeArtifacts(texts []string, output string) {
	artifacts := map[string]bool{}
	collectArtifacts(texts, output, artifacts)
	if len(artifacts) == 0 {
		return
	}
	p.mu.Lock()
	if p.observation.artifacts == nil {
		p.observation.artifacts = map[string]bool{}
	}
	for name := range artifacts {
		p.observation.artifacts[name] = true
	}
	p.mu.Unlock()
}

// collectArtifacts adds every link to a file inside this Operation's output
// directory, including screenshots and console or network logs.
func collectArtifacts(texts []string, output string, artifacts map[string]bool) {
	for _, text := range texts {
		for _, match := range screenshotLink.FindAllStringSubmatch(text, -1) {
			artifacts[path.Base(match[1])] = true
		}
		for _, match := range markdownLink.FindAllStringSubmatch(text, -1) {
			if name := outputArtifact(output, match[1]); name != "" {
				artifacts[name] = true
			}
		}
	}
}

// scanDownloads lists the files a user downloaded. The Events section names
// completed downloads; the output directory catches files whose event arrived
// after the action. Links to files inside this Operation's output directory
// are Playwright artifacts, not downloads, and the console log is written
// without a link.
func scanDownloads(output string, texts []string, artifacts map[string]bool) []string {
	if artifacts == nil {
		artifacts = map[string]bool{}
	}
	found := map[string]bool{}
	for _, text := range texts {
		for _, match := range downloadEvent.FindAllStringSubmatch(text, -1) {
			found[path.Base(filepath.ToSlash(match[1]))] = true
		}
	}
	collectArtifacts(texts, output, artifacts)
	if output != "" {
		entries, err := os.ReadDir(output)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				lower := strings.ToLower(name)
				switch {
				case strings.HasSuffix(lower, ".yml"), strings.HasSuffix(lower, ".yaml"),
					strings.HasSuffix(lower, ".crdownload"), strings.HasSuffix(lower, ".tmp"),
					consoleLog.MatchString(lower), artifacts[name]:
					continue
				}
				if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
					found[name] = true
				}
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// outputArtifact returns the file name of a link that points inside this
// Operation's output directory, or empty for a page or remote link.
func outputArtifact(output, target string) string {
	if output == "" || strings.Contains(target, "://") {
		return ""
	}
	marker := path.Base(filepath.ToSlash(output)) + "/"
	slash := filepath.ToSlash(target)
	index := strings.Index(slash, marker)
	if index < 0 {
		return ""
	}
	rest := slash[index+len(marker):]
	if hash := strings.IndexByte(rest, '#'); hash >= 0 {
		rest = rest[:hash]
	}
	if rest == "" || strings.HasSuffix(rest, "/") {
		return ""
	}
	return path.Base(rest)
}

func setOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
