package grafana

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

func baseInput() Input {
	return Input{
		Service:       "checkout",
		Namespace:     "shop",
		Team:          "payments",
		SLOs:          []api.SLO{{Name: "availability", Type: api.SLOAvailability, Objective: 99.9}},
		PrometheusUID: "prom-main",
		LokiUID:       "loki-main",
		TempoUID:      "tempo-main",
		LogsEnabled:   true,
		TracesEnabled: true,
		Labels:        map[string]string{"lantern.dev/service": "checkout"},
	}
}

func dashboardData(t *testing.T, obj interface{ YAML() string }) map[string]any {
	t.Helper()
	// The object under test is a *kube.Object; extract the embedded JSON by
	// round-tripping through the package's own toGenericJSON path isn't
	// available on the object directly, so tests instead call Build and
	// inspect the ConfigMap's rendered YAML for the embedded JSON blob.
	yaml := obj.YAML()
	start := strings.Index(yaml, "{\n")
	if start == -1 {
		t.Fatalf("no JSON blob found in ConfigMap YAML:\n%s", yaml)
	}
	// Strip the 4-space YAML block-scalar indent from every line so the
	// remainder parses as JSON.
	var b strings.Builder
	for _, line := range strings.Split(yaml[start:], "\n") {
		b.WriteString(strings.TrimPrefix(line, "    "))
		b.WriteString("\n")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("embedded dashboard is not valid JSON: %v\n%s", err, b.String())
	}
	return doc
}

func TestBuildProducesValidDashboardWithAllPanelGroups(t *testing.T) {
	obj, diags, err := Build(baseInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if obj == nil {
		t.Fatal("expected a dashboard object")
	}
	for _, d := range diags {
		if d.Level == "warn" || d.Level == "error" {
			t.Errorf("unexpected diagnostic: %+v", d)
		}
	}
	doc := dashboardData(t, obj)
	panels := doc["panels"].([]any)
	// 1 SLO -> 2 panels (timeseries + stat), plus logs + traces = 4.
	if len(panels) != 4 {
		t.Fatalf("got %d panels, want 4: %+v", len(panels), panels)
	}
	var types []string
	for _, p := range panels {
		types = append(types, p.(map[string]any)["type"].(string))
	}
	want := []string{"timeseries", "stat", "logs", "traces"}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("panel %d type = %q, want %q (all types: %v)", i, types[i], w, types)
		}
	}
}

func TestBuildSkipsPanelGroupsWithNoDatasource(t *testing.T) {
	in := baseInput()
	in.LokiUID = ""
	in.TempoUID = ""
	obj, diags, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc := dashboardData(t, obj)
	panels := doc["panels"].([]any)
	if len(panels) != 2 {
		t.Fatalf("got %d panels, want 2 (SLO panels only): %+v", len(panels), panels)
	}
	foundInfo := 0
	for _, d := range diags {
		if d.Level == "info" {
			foundInfo++
		}
	}
	if foundInfo < 2 {
		t.Errorf("expected an info diagnostic for each skipped datasource, got %d info diagnostics: %+v", foundInfo, diags)
	}
}

func TestBuildReturnsNilWhenNothingToShow(t *testing.T) {
	in := Input{Service: "empty", Namespace: "ns"}
	obj, diags, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if obj != nil {
		t.Fatalf("expected nil object when there are no SLOs and no datasources, got %+v", obj)
	}
	if len(diags) == 0 {
		t.Error("expected at least one diagnostic explaining why nothing was emitted")
	}
}

func TestBuildRespectsSignalToggles(t *testing.T) {
	in := baseInput()
	in.LogsEnabled = false
	in.TracesEnabled = false
	obj, _, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc := dashboardData(t, obj)
	panels := doc["panels"].([]any)
	if len(panels) != 2 {
		t.Fatalf("got %d panels, want 2 (SLO panels only, logs/traces disabled): %+v", len(panels), panels)
	}
}

func TestBuildIncludesExtraPanels(t *testing.T) {
	in := baseInput()
	in.ExtraPanels = []api.Panel{
		{Title: "Custom queue depth", Query: "sum(my_queue_depth)", Viz: "stat", Unit: "short"},
	}
	obj, _, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc := dashboardData(t, obj)
	panels := doc["panels"].([]any)
	last := panels[len(panels)-1].(map[string]any)
	if last["title"] != "Custom queue depth" {
		t.Errorf("last panel = %+v, want the extra panel last", last)
	}
	if last["type"] != "stat" {
		t.Errorf("extra panel type = %v, want %q from Viz", last["type"], "stat")
	}
}

// --- RFC 6902 patch application ---------------------------------------------

func TestApplyPatchAddReplaceRemove(t *testing.T) {
	doc, _ := toGenericJSON(map[string]any{"title": "orig", "tags": []any{"a"}})

	doc, err := applyPatch(doc, api.Patch{Op: "replace", Path: "/title", Value: "new"})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if doc.(map[string]any)["title"] != "new" {
		t.Errorf("title = %v, want %q", doc.(map[string]any)["title"], "new")
	}

	doc, err = applyPatch(doc, api.Patch{Op: "add", Path: "/tags/-", Value: "b"})
	if err != nil {
		t.Fatalf("add append: %v", err)
	}
	tags := doc.(map[string]any)["tags"].([]any)
	if len(tags) != 2 || tags[1] != "b" {
		t.Errorf("tags = %v, want [a b]", tags)
	}

	doc, err = applyPatch(doc, api.Patch{Op: "add", Path: "/owner", Value: "team-x"})
	if err != nil {
		t.Fatalf("add new key: %v", err)
	}
	if doc.(map[string]any)["owner"] != "team-x" {
		t.Errorf("owner = %v, want %q", doc.(map[string]any)["owner"], "team-x")
	}

	doc, err = applyPatch(doc, api.Patch{Op: "remove", Path: "/owner"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := doc.(map[string]any)["owner"]; ok {
		t.Error("owner should have been removed")
	}
}

func TestApplyPatchAddAtNumericIndexEqualToArrayLength(t *testing.T) {
	// RFC 6902 §4.1: an "add" target index equal to the array's current
	// length means append — the same as the "-" marker. Regression test for
	// a bug found on the real UAT bastion (round 3): arrayIndex rejected
	// idx == length unconditionally, so a literal numeric index (rather than
	// "-") could never be used to append.
	doc, _ := toGenericJSON(map[string]any{"tags": []any{"a", "b"}})

	doc, err := applyPatch(doc, api.Patch{Op: "add", Path: "/tags/2", Value: "c"})
	if err != nil {
		t.Fatalf("add at index == length should succeed (RFC 6902 append semantics): %v", err)
	}
	tags := doc.(map[string]any)["tags"].([]any)
	if len(tags) != 3 || tags[2] != "c" {
		t.Errorf("tags = %v, want [a b c]", tags)
	}

	// One past the length is still invalid — only == length is legal.
	if _, err := applyPatch(doc, api.Patch{Op: "add", Path: "/tags/10", Value: "d"}); err == nil {
		t.Error("add at an index beyond length+1 should still error")
	}

	// replace/remove must NOT gain the same == length leniency: there is no
	// element at that index to replace or remove.
	if _, err := applyPatch(doc, api.Patch{Op: "replace", Path: "/tags/3", Value: "x"}); err == nil {
		t.Error("replace at index == length should still error — nothing exists there")
	}
	if _, err := applyPatch(doc, api.Patch{Op: "remove", Path: "/tags/3"}); err == nil {
		t.Error("remove at index == length should still error — nothing exists there")
	}
}

func TestApplyPatchTest(t *testing.T) {
	doc, _ := toGenericJSON(map[string]any{"title": "orig"})

	if _, err := applyPatch(doc, api.Patch{Op: "test", Path: "/title", Value: "orig"}); err != nil {
		t.Errorf("test op should pass when value matches: %v", err)
	}
	if _, err := applyPatch(doc, api.Patch{Op: "test", Path: "/title", Value: "wrong"}); err == nil {
		t.Error("test op should fail when value does not match")
	}
}

func TestApplyPatchRejectsUnsupportedOps(t *testing.T) {
	doc, _ := toGenericJSON(map[string]any{"title": "orig"})
	if _, err := applyPatch(doc, api.Patch{Op: "move", Path: "/title", From: "/other"}); err == nil {
		t.Error("move should be rejected loudly, not silently ignored")
	}
	if _, err := applyPatch(doc, api.Patch{Op: "copy", Path: "/title", From: "/other"}); err == nil {
		t.Error("copy should be rejected loudly, not silently ignored")
	}
}

func TestApplyPatchErrorsOnMissingPath(t *testing.T) {
	doc, _ := toGenericJSON(map[string]any{"title": "orig"})
	if _, err := applyPatch(doc, api.Patch{Op: "replace", Path: "/nonexistent", Value: "x"}); err == nil {
		t.Error("replace on a missing key should error, not silently create it")
	}
	if _, err := applyPatch(doc, api.Patch{Op: "remove", Path: "/nonexistent"}); err == nil {
		t.Error("remove on a missing key should error")
	}
}

func TestBuildPatchesApplyToFinalDashboard(t *testing.T) {
	in := baseInput()
	in.Patches = []api.Patch{
		{Op: "replace", Path: "/title", Value: "Custom Title"},
		{Op: "add", Path: "/tags/-", Value: "custom"},
	}
	obj, _, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc := dashboardData(t, obj)
	if doc["title"] != "Custom Title" {
		t.Errorf("title = %v, want %q", doc["title"], "Custom Title")
	}
	tags := doc["tags"].([]any)
	if tags[len(tags)-1] != "custom" {
		t.Errorf("tags = %v, want last element %q", tags, "custom")
	}
}
