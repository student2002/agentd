// wizard_test.go covers the wizard's pure logic: choice application,
// connection validation, and the summary/hint rendering.
package main

import (
	"strings"
	"testing"
)

func TestWizardChoicesApplyTo(t *testing.T) {
	choices := wizardChoices{
		ServerURL:  "http://127.0.0.1:9",
		BaseBranch: "main",
		Tools: map[string]string{
			"claude":   "claude",
			"openclaw": "/usr/local/bin/openclaw",
		},
		Connections: []wizardConnection{{Name: "team-a", Token: "td_x"}},
	}
	cfg := defaultGlobalConfig()
	choices.applyTo(cfg)

	if cfg.Server.URL != "http://127.0.0.1:9" {
		t.Fatalf("server.url: %q", cfg.Server.URL)
	}
	if cfg.Git.BaseBranch != "main" {
		t.Fatalf("base_branch: %q", cfg.Git.BaseBranch)
	}
	if cfg.Tools.Claude.Path != "claude" || cfg.Tools.OpenClaw.Path != "/usr/local/bin/openclaw" {
		t.Fatalf("tool paths: %+v", cfg.Tools)
	}
	if cfg.Tools.OpenCode.Path != "" {
		t.Fatalf("unselected tool should stay empty: %+v", cfg.Tools)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Name != "team-a" || cfg.Workspaces[0].Token != "td_x" {
		t.Fatalf("workspaces: %+v", cfg.Workspaces)
	}
}

func TestWizardChoicesApplyToEmptyChangesNothing(t *testing.T) {
	cfg := defaultGlobalConfig()
	wizardChoices{}.applyTo(cfg)
	if cfg.Server.URL != "" || cfg.Git.BaseBranch != "" || len(cfg.Workspaces) != 0 {
		t.Fatalf("empty choices should leave the skeleton untouched: %+v", cfg)
	}
}

func TestValidateConnection(t *testing.T) {
	existing := []wizardConnection{{Name: "team-a", Token: "td_a"}}
	if err := validateConnection("team-a", "td_b", existing); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected duplicate alias error, got %v", err)
	}
	if err := validateConnection("team-b", "not-a-token", existing); err == nil || !strings.Contains(err.Error(), "td_") {
		t.Fatalf("expected token prefix error, got %v", err)
	}
	if err := validateConnection("team-b", "td_b", existing); err != nil {
		t.Fatalf("valid connection rejected: %v", err)
	}
}

func TestRenderSummaryAndPendingHints(t *testing.T) {
	empty := wizardChoices{}
	summary := renderSummary(empty)
	for _, want := range []string{"console-only mode", "(skipped)", "(none selected)", "(none added)"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("empty summary missing %q:\n%s", want, summary)
		}
	}
	if hints := pendingHints(empty); len(hints) != 4 {
		t.Fatalf("empty choices should yield 4 hints, got %d: %v", len(hints), hints)
	}

	full := wizardChoices{
		ServerURL:  "http://127.0.0.1:9",
		BaseBranch: "master",
		Tools:      map[string]string{"claude": "claude"},
		Connections: []wizardConnection{
			{Name: "team-a", Token: "td_secret_secret"},
		},
	}
	summary = renderSummary(full)
	for _, want := range []string{"http://127.0.0.1:9", "master", "tools.claude.path", "team-a"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("full summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "td_secret_secret") {
		t.Fatal("summary must mask the token")
	}
	if hints := pendingHints(full); len(hints) != 0 {
		t.Fatalf("full choices should yield no hints, got %v", hints)
	}
}
