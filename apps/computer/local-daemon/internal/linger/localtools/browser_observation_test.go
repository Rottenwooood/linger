package localtools

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func browserResult(t *testing.T, text string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func browserResultText(t *testing.T, result json.RawMessage) string {
	t.Helper()
	texts := resultTexts(result)
	if len(texts) != 1 {
		t.Fatalf("result text parts: %d", len(texts))
	}
	return texts[0]
}

func snapshotPage(url, title, snapshot string) string {
	return "### Page\n- Page URL: " + url + "\n- Page Title: " + title + "\n### Snapshot\n```yaml\n" + snapshot + "\n```"
}

func TestBrowserFirstNavigationKeepsFullSnapshot(t *testing.T) {
	var pages browserPages
	result := pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart",
		"- main [ref=e1]:\n  - button \"Pay\" [ref=e2]",
	)), t.TempDir())
	text := browserResultText(t, result)
	if !strings.Contains(text, "### Snapshot\n```yaml\n- main [ref=e1]:") {
		t.Fatalf("first navigation lost its full snapshot: %q", text)
	}
	if strings.Contains(text, "### Snapshot diff") {
		t.Fatalf("first navigation reported a diff: %q", text)
	}
}

func TestBrowserSamePageActionEmitsMultiLayerDiff(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart",
		"- main [ref=e1]:\n  - button \"Pay\" [ref=e2]\n  - text \"Total 10\"",
	)), output)
	if err := os.WriteFile(filepath.Join(output, "report.pdf"), []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "photo.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "shot.jpeg"), []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	click := "### Open tabs\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Receipt](https://shop.example/receipt)\n" +
		"![Screenshot](.linger/browser-output/abc/shot.jpeg)\n" +
		snapshotPage("https://shop.example/cart", "Cart",
			"- main [ref=e1]:\n  - button \"Pay now\" [ref=e2]\n  - dialog \"Confirm\" [ref=e3]:\n    - text \"Are you sure?\"")
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, click), output))
	for _, want := range []string{
		"### Snapshot diff",
		"+ - dialog \"Confirm\" [ref=e3]:",
		"~ - button \"Pay now\" [ref=e2]",
		"- - text \"Total 10\"",
		"- download: report.pdf",
		"- download: photo.png",
		"- tabs added: - 1: [Receipt](https://shop.example/receipt)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff lacks %q: %q", want, text)
		}
	}
	if strings.Contains(text, "download: shot.jpeg") {
		t.Fatalf("screenshot was reported as a download: %q", text)
	}
	if strings.Contains(text, "- main [ref=e1]:") {
		t.Fatalf("unchanged element repeated in the diff: %q", text)
	}
	if role, label, ok := pages.element("e1"); !ok || role != "main" {
		t.Fatalf("unchanged ref lost from permission evidence: %v %q %q", ok, role, label)
	}
	if role, label, ok := pages.element("e3"); !ok || role != "dialog" || label != "Confirm" {
		t.Fatalf("added ref missing from permission evidence: %v %q %q", ok, role, label)
	}
}

func TestBrowserDiffKeepsDollarSigns(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:\n  - text \"Price $100\"",
	)), output))
	if !strings.Contains(text, "+ - text \"Price $100\"") {
		t.Fatalf("diff rewrote a dollar sign: %q", text)
	}
}

func TestBrowserSamePageActionWithoutChangeSaysSo(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	page := "### Page\n- Page URL: https://shop.example/cart\n- Page Title: Cart\n- Console: 0 errors, 0 warnings\n### Snapshot\n```yaml\n- main [ref=e1]:\n  - button \"Pay\" [ref=e2]\n```"
	pages.observeAction("browser_navigate", browserResult(t, page), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, page), output))
	if !strings.Contains(text, "### Snapshot diff\nno_change") {
		t.Fatalf("unchanged page did not report no_change: %q", text)
	}
}

func TestBrowserLargeChangeKeepsFullSnapshot(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/list", "List", "- main [ref=e1]:",
	)), output)
	var snapshot strings.Builder
	snapshot.WriteString("- main [ref=e1]:\n")
	for index := 0; index < maxDiffElements+1; index++ {
		snapshot.WriteString(fmt.Sprintf("  - button \"Item\" [ref=e%d]\n", index))
	}
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, snapshotPage(
		"https://shop.example/list", "List", snapshot.String(),
	)), output))
	if !strings.Contains(text, "### Snapshot\n```yaml\n- main [ref=e1]:") {
		t.Fatalf("large change was compacted: %q", text)
	}
}

func TestBrowserNavigationChangeKeepsFullSnapshot(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, snapshotPage(
		"https://shop.example/checkout", "Checkout", "- main [ref=e9]:\n  - button \"Place order\" [ref=e10]",
	)), output))
	if !strings.Contains(text, "### Snapshot\n```yaml\n- main [ref=e9]:") {
		t.Fatalf("navigation change was compacted: %q", text)
	}
}

func TestBrowserSamePageActionReportsPageHostChanges(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t,
		"### Page\n- Page URL: https://shop.example/cart\n- Page Title: Cart\n- Console: 0 errors, 0 warnings\n"+
			"### Snapshot\n```yaml\n- main [ref=e1]:\n```"), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t,
		"### Page\n- Page URL: https://shop.example/cart\n- Page Title: Cart (2)\n- HTTP status: 500 Server Error\n- Console: 2 errors, 0 warnings\n"+
			"### Snapshot\n```yaml\n- main [ref=e1]:\n```"), output))
	for _, want := range []string{
		"### Snapshot diff",
		"- title: Cart -> Cart (2)",
		"- http status: 500 Server Error",
		"- console: 0 errors, 0 warnings -> 2 errors, 0 warnings",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("page host diff lacks %q: %q", want, text)
		}
	}
}

func TestBrowserTabsResultUpdatesTheTabBaseline(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	tabs := "### Open tabs\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Receipt](https://shop.example/receipt)"
	pages.observeAction("browser_tabs", browserResult(t, tabs), output)
	click := tabs + "\n" + snapshotPage("https://shop.example/cart", "Cart", "- main [ref=e1]:")
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, click), output))
	if strings.Contains(text, "tabs added") || strings.Contains(text, "tabs removed") {
		t.Fatalf("tabs already reported by browser_tabs were reported again: %q", text)
	}
}

func TestBrowserTabsResultFromResultSection(t *testing.T) {
	var pages browserPages
	pages.observeTabs("browser_tabs", []string{
		"### Result\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Receipt](https://shop.example/receipt)",
	})
	if len(pages.observation.tabs) != 2 {
		t.Fatalf("Result tab list parsed as %d tabs: %v", len(pages.observation.tabs), pages.observation.tabs)
	}
	if pages.observation.activeTab == "" {
		t.Fatal("Result tab list lost the current tab")
	}
}

func TestBrowserTabsCloseToOneTabUpdatesTheBaseline(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	pages.observeAction("browser_tabs", browserResult(t,
		"### Open tabs\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Receipt](https://shop.example/receipt)"), output)
	pages.observeAction("browser_tabs", browserResult(t,
		"### Result\n- 0: (current) [Cart](https://shop.example/cart)"), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output))
	if strings.Contains(text, "tabs added") || strings.Contains(text, "tabs removed") {
		t.Fatalf("closed tab remained in the baseline: %q", text)
	}
}

func TestBrowserTabsCloseLastTabClearsTheBaseline(t *testing.T) {
	var pages browserPages
	pages.observeTabs("browser_tabs", []string{
		"### Result\n- 0: (current) [Cart](https://shop.example/cart)",
	})
	pages.observeTabs("browser_tabs", []string{
		"### Result\nNo open tabs. Navigate to a URL to create one.",
	})
	if len(pages.observation.tabs) != 0 {
		t.Fatalf("closed last tab remained in the baseline: %v", pages.observation.tabs)
	}
}

func TestBrowserTabSwitchKeepsTabLines(t *testing.T) {
	before, beforeActive := parseTabs([]string{
		"### Open tabs\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Receipt](https://shop.example/receipt)",
	})
	current, currentActive := parseTabs([]string{
		"### Open tabs\n- 0: [Cart](https://shop.example/cart)\n- 1: (current) [Receipt](https://shop.example/receipt)",
	})
	diff := compareObservation(
		browserObservation{hasSnapshot: true, tabs: before, activeTab: beforeActive},
		browserObservation{hasSnapshot: true, tabs: current, activeTab: currentActive},
	)
	if len(diff.tabsChanged) != 0 {
		t.Fatalf("tab switch reported unchanged tabs: %v", diff.tabsChanged)
	}
	if diff.activeTab == "" {
		t.Fatal("tab switch did not change the active tab")
	}
	if shouldCompact("browser_click", diff) {
		t.Fatal("a page from another tab was compacted into the previous tree")
	}
}

func TestBrowserTabKeyKeepsDuplicateURLs(t *testing.T) {
	tabs, _ := parseTabs([]string{
		"### Open tabs\n- 0: (current) [Cart](https://shop.example/cart)\n- 1: [Cart](https://shop.example/cart)",
	})
	if len(tabs) != 2 {
		t.Fatalf("duplicate tab addresses collapsed into %d tabs: %v", len(tabs), tabs)
	}
}

func TestBrowserFormValueChangeKeepsItsRef(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	page := func(value string) string {
		return snapshotPage("https://shop.example/checkout", "Checkout",
			"- main [ref=e1]:\n  - textbox \"Email\" [ref=e2]:\n    - /placeholder: Enter email\n    - text: "+value)
	}
	pages.observeAction("browser_navigate", browserResult(t, page("a@b.com")), output)
	text := browserResultText(t, pages.observeAction("browser_type", browserResult(t, page("c@d.com")), output))
	if !strings.Contains(text, "~ - textbox \"Email\" [ref=e2]: c@d.com") {
		t.Fatalf("form value change lost its ref: %q", text)
	}
	if strings.Contains(text, "+ - text: c@d.com") {
		t.Fatalf("form value change repeated as an orphan text line: %q", text)
	}
}

func TestBrowserDiffIgnoresPlaywrightArtifacts(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	for _, name := range []string{"console-2026.log", "page-1.png", "report.pdf", "photo.png"} {
		if err := os.WriteFile(filepath.Join(output, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := ".linger/browser-output/" + path.Base(filepath.ToSlash(output)) + "/"
	click := "### Events\n- Downloaded file report.pdf to \"" + link + "report.pdf\"\n" +
		"- [Screenshot of viewport](" + link + "page-1.png)\n" +
		"- [Console](" + link + "console-2026.log)\n" +
		snapshotPage("https://shop.example/cart", "Cart", "- main [ref=e1]:")
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, click), output))
	for _, want := range []string{"- download: report.pdf", "- download: photo.png"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff lacks %q: %q", want, text)
		}
	}
	for _, unwanted := range []string{"- download: console-2026.log", "- download: page-1.png"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("Playwright artifact reported as a download: %q", text)
		}
	}
}

func TestBrowserScreenshotArtifactSurvivesNoSnapshotResult(t *testing.T) {
	var pages browserPages
	output := t.TempDir()
	pages.observeAction("browser_navigate", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output)
	if err := os.WriteFile(filepath.Join(output, "page-1.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := ".linger/browser-output/" + path.Base(filepath.ToSlash(output)) + "/page-1.png"
	pages.observeAction("browser_take_screenshot", browserResult(t, "- [Screenshot of viewport]("+link+")"), output)
	text := browserResultText(t, pages.observeAction("browser_click", browserResult(t, snapshotPage(
		"https://shop.example/cart", "Cart", "- main [ref=e1]:",
	)), output))
	if strings.Contains(text, "- download: page-1.png") {
		t.Fatalf("screenshot reported as a download: %q", text)
	}
}
