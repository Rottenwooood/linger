package localtools

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
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
	// tabs maps a tab's address to the line Playwright rendered for it.
	tabs      map[string]string
	activeTab string
	// downloads holds the completed files already reported for this page.
	downloads map[string]bool
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
	openTabLine    = regexp.MustCompile(`^- \d+: (?:\(current\) )?\[(.*)\]\((.*)\)(?: \[crashed\])?$`)
	snapshotBlock  = regexp.MustCompile("(?s)### Snapshot\\n```yaml\\n(.*?)\\n```")
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
		p.observeTabs(texts)
		return result
	}

	current := observationFromText(texts, snapshots)
	current.downloads = setOf(scanDownloads(output, texts))
	p.mu.Lock()
	before := p.observation
	switch {
	case current.tabs == nil && current.url == before.url && before.hasSnapshot:
		// A result without an Open tabs section does not mean the tabs
		// closed; it means the tab headers did not change.
		current.tabs, current.activeTab = before.tabs, before.activeTab
	case current.tabs == nil:
		current.tabs, current.activeTab = singleTab(current.url, current.title)
	}
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
func (p *browserPages) observeTabs(texts []string) {
	tabs, active := parseTabs(texts)
	if tabs == nil {
		return
	}
	p.mu.Lock()
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
	if diff.urlChanged {
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
	for address, line := range current.tabs {
		old, present := before.tabs[address]
		switch {
		case !present:
			diff.tabsAdded = append(diff.tabsAdded, line)
		case old != line:
			diff.tabsChanged = append(diff.tabsChanged, line)
		}
	}
	for address, line := range before.tabs {
		if _, present := current.tabs[address]; !present {
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
	changedRefs := map[string]bool{}
	for ref, line := range current.refs {
		if old, present := before.refs[ref]; present && old != line {
			diff.changed = append(diff.changed, line)
			changedRefs[ref] = true
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
			if changedRefs[ref] {
				continue
			}
			removed = append(removed, line)
		}
	}
	for line, count := range current.lines {
		if count <= before.lines[line] {
			continue
		}
		if containsRef(line, changedRefs) {
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
		if match := pageRef.FindStringSubmatch(line); match != nil && changedRefs[match[2]] {
			continue
		}
		removed = append(removed, line)
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

func containsRef(line string, refs map[string]bool) bool {
	match := pageRef.FindStringSubmatch(line)
	return match != nil && refs[match[2]]
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

// observationFromText reads the page header, tabs and full snapshot of one
// result into a baseline.
func observationFromText(texts, snapshots []string) browserObservation {
	observation := browserObservation{
		hasSnapshot: true,
		refs:        map[string]string{},
		lines:       map[string]int{},
		downloads:   map[string]bool{},
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
		for _, line := range strings.Split(snapshot, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			observation.lines[trimmed]++
			if match := pageRef.FindStringSubmatch(line); match != nil {
				observation.refs[match[2]] = trimmed
			}
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
		tabs = map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(block[1]), "\n") {
			match := openTabLine.FindStringSubmatch(strings.TrimSpace(line))
			if match == nil {
				continue
			}
			address := match[2]
			tabs[address] = strings.TrimSpace(line)
			if strings.Contains(line, "(current)") {
				active = address
			}
		}
	}
	return tabs, active
}

// singleTab is the tab list of a page whose result did not print one: the
// current page is the only tab Playwright reported.
func singleTab(url, title string) (map[string]string, string) {
	if url == "" {
		return nil, ""
	}
	return map[string]string{url: "- 0: (current) [" + title + "](" + url + ")"}, url
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
		fmt.Fprintf(&out, "- active tab: %s\n", d.activeTab)
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
	for _, item := range content {
		part, _ := item.(map[string]any)
		text, ok := part["text"].(string)
		if !ok || !strings.Contains(text, "### Snapshot\n```yaml") {
			continue
		}
		part["text"] = snapshotBlock.ReplaceAllLiteralString(text, report)
	}
	encoded, _ := json.Marshal(reply)
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

// scanDownloads lists the completed files in the Operation's browser output
// directory. Snapshots, screenshots, and unfinished downloads are not user
// downloads. A screenshot is identified by the link Playwright printed for it,
// so a downloaded image with the same extension is still a download.
func scanDownloads(output string, texts []string) []string {
	if output == "" {
		return nil
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return nil
	}
	screenshots := map[string]bool{}
	for _, text := range texts {
		for _, match := range screenshotLink.FindAllStringSubmatch(text, -1) {
			screenshots[path.Base(match[1])] = true
		}
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		switch {
		case strings.HasSuffix(lower, ".yml"), strings.HasSuffix(lower, ".yaml"),
			strings.HasSuffix(lower, ".crdownload"), strings.HasSuffix(lower, ".tmp"),
			screenshots[name]:
			continue
		}
		if info, err := entry.Info(); err != nil || !info.Mode().IsRegular() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func setOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
