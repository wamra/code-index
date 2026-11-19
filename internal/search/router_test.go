package search

import "testing"

func TestLooksLikeIdentifier(t *testing.T) {
	yes := []string{
		"Get-Widget", "get_widget", "getWidget", "Widgets/Widgets.psm1", "./lib.ps1", "--force",
		"Foo.Bar", "std::vector", "MAX_RETRIES", "widget", "Invoke-Helper -Name", "deploy_release",
	}
	no := []string{
		"function that returns a widget by name", "how are widgets created", "where is the cache invalidated",
		"returns widget", "two words", "",
	}
	for _, q := range yes {
		if !LooksLikeIdentifier(q) {
			t.Errorf("%q should look like an identifier", q)
		}
	}
	for _, q := range no {
		if LooksLikeIdentifier(q) {
			t.Errorf("%q should not look like an identifier", q)
		}
	}
}

func TestFTSTokens(t *testing.T) {
	got := ftsTokens("The Get-Widget function that returns a widget_by_name, twice: widget")
	want := []string{"get", "widget", "function", "returns", "by", "name", "twice"}
	// "by" is a stopword here: expect it dropped
	want = []string{"get", "widget", "function", "returns", "name", "twice"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %v, want %v", got, want)
		}
	}
}
