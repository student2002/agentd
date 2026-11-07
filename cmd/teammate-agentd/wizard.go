// wizard.go provides the interactive `config init` wizard (TTY only). The
// flow follows the probe-then-pick pattern: machine facts are detected up
// front, every step offers a default and a skip, and nothing is written
// until the summary is confirmed.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/teammate/agentd/internal/agent"
)

// ---------- choice model (unit-tested without a terminal) ----------

// wizardConnection is one workspace connection collected by the wizard.
type wizardConnection struct {
	Name  string
	Token string
}

// wizardChoices carries everything the wizard collected; empty fields mean
// "skipped, configure later".
type wizardChoices struct {
	ServerURL   string
	BaseBranch  string
	Tools       map[string]string // provider -> executable path
	Connections []wizardConnection
}

// applyTo fills the wizard's choices into a daemon config.
func (c wizardChoices) applyTo(cfg *agent.GlobalConfig) {
	cfg.Server.URL = c.ServerURL
	cfg.Git.BaseBranch = c.BaseBranch
	for provider, path := range c.Tools {
		switch provider {
		case "claude":
			cfg.Tools.Claude.Path = path
		case "openclaw":
			cfg.Tools.OpenClaw.Path = path
		case "opencode":
			cfg.Tools.OpenCode.Path = path
		case "atomcode":
			cfg.Tools.AtomCode.Path = path
		case "mimocode":
			cfg.Tools.MiMoCode.Path = path
		}
	}
	for _, conn := range c.Connections {
		cfg.Workspaces = append(cfg.Workspaces, agent.WorkspaceEntry{Name: conn.Name, Token: conn.Token})
	}
}

// validateConnection applies the same rules as `workspace add` and the
// runtime connection add: td_ prefix and a unique alias.
func validateConnection(name, token string, existing []wizardConnection) error {
	if !strings.HasPrefix(token, "td_") {
		return fmt.Errorf("the daemon token must start with td_ (copy it from the workspace web UI)")
	}
	for _, conn := range existing {
		if conn.Name == name {
			return fmt.Errorf("alias %q is already used by an earlier connection in this session", name)
		}
	}
	return nil
}

// renderSummary lines up what the wizard collected, marking skipped items.
func renderSummary(c wizardChoices) string {
	var b strings.Builder
	b.WriteString("Configuration to write:\n")
	serverURL := c.ServerURL
	if serverURL == "" {
		serverURL = "(skipped — console-only mode)"
	}
	fmt.Fprintf(&b, "  server.url:       %s\n", serverURL)
	baseBranch := c.BaseBranch
	if baseBranch == "" {
		baseBranch = "(skipped)"
	}
	fmt.Fprintf(&b, "  git.base_branch:  %s\n", baseBranch)
	if len(c.Tools) == 0 {
		b.WriteString("  tools:            (none selected)\n")
	}
	for _, provider := range agent.SupportedProviders {
		if path, ok := c.Tools[provider]; ok {
			fmt.Fprintf(&b, "  tools.%s.path:  %s\n", provider, path)
		}
	}
	if len(c.Connections) == 0 {
		b.WriteString("  workspaces:       (none added)\n")
	}
	for _, conn := range c.Connections {
		fmt.Fprintf(&b, "  workspace %q:     token %s\n", conn.Name, maskToken(conn.Token))
	}
	return b.String()
}

// pendingHints lists the skipped items with the exact commands that fill
// them later.
func pendingHints(c wizardChoices) []string {
	var hints []string
	if c.ServerURL == "" {
		hints = append(hints, "teammate-agentd config set server.url <http://your-server:8080>   # or switch it in the local console")
	}
	if c.BaseBranch == "" {
		hints = append(hints, "teammate-agentd config set git.base_branch <main|master>")
	}
	if len(c.Tools) == 0 {
		hints = append(hints, "teammate-agentd config set tools.<provider>.path <cli>          # one per coding tool you use")
	}
	if len(c.Connections) == 0 {
		hints = append(hints, "teammate-agentd workspace add <td_...> --name <alias>           # or add it in the local console")
	}
	return hints
}

// installedProviders probes PATH for every supported coding tool by its bare
// command name.
func installedProviders() []string {
	probe := agent.NewProbeManager(agent.ToolsConfig{
		Claude:   agent.ToolConfig{Path: "claude"},
		OpenClaw: agent.ToolConfig{Path: "openclaw"},
		OpenCode: agent.ToolConfig{Path: "opencode"},
		AtomCode: agent.ToolConfig{Path: "atomcode"},
		MiMoCode: agent.ToolConfig{Path: "mimocode"},
	})
	installed := make([]string, 0, len(agent.SupportedProviders))
	for _, info := range probe.Snapshot() {
		if info.Installed {
			installed = append(installed, info.Provider)
		}
	}
	return installed
}

// interactiveTerminal reports whether both stdin and stdout are character
// devices; the wizard renders ANSI UI and reads keys, so redirects and
// containers get the skeleton path instead.
func interactiveTerminal() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

// ---------- terminal UI ----------

type wizardStep int

const (
	stepServerURL wizardStep = iota
	stepBaseBranch
	stepBaseBranchCustom
	stepTools
	stepConnectConfirm
	stepConnectName
	stepConnectToken
	stepSummary
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	hintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

type wizardModel struct {
	choices wizardChoices
	step    wizardStep

	options []string // current selector / confirm choices
	cursor  int

	checked map[string]bool // provider -> checked (checkbox step)

	input  textinput.Model
	errMsg string

	pendingName   string // alias typed for the connection being added
	pendingToken  string // token stashed across the alias-retry loop
	connectAsked  bool   // first ask vs "add another?"
	installedList []string

	aborted bool
	written bool
}

func newWizardModel(installed []string) wizardModel {
	m := wizardModel{
		choices:       wizardChoices{Tools: map[string]string{}},
		step:          stepServerURL,
		input:         newWizardInput("http://your-server:8080"),
		checked:       map[string]bool{},
		installedList: installed,
	}
	for _, provider := range installed {
		m.checked[provider] = true
	}
	return m
}

func newWizardInput(placeholder string) textinput.Model {
	input := textinput.New()
	input.Placeholder = placeholder
	input.Prompt = "> "
	input.Focus()
	input.Width = 48
	return input
}

func (m wizardModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m wizardModel) abort() tea.Model {
	m.aborted = true
	return m
}

func (m *wizardModel) enterBaseBranch() {
	m.step = stepBaseBranch
	m.cursor = 0
	m.options = []string{"master", "main", "custom…", "skip for now"}
	m.errMsg = ""
}

func (m *wizardModel) enterTools() {
	m.errMsg = ""
	if len(m.installedList) == 0 {
		m.enterConnectConfirm()
		return
	}
	m.step = stepTools
	m.cursor = 0
}

func (m *wizardModel) enterConnectConfirm() {
	m.step = stepConnectConfirm
	m.cursor = 0
	m.options = []string{"no — add later", "yes"}
	m.errMsg = ""
	m.pendingToken = ""
}

func (m *wizardModel) enterSummary() {
	m.step = stepSummary
	m.cursor = 0
	m.options = []string{"write config", "cancel"}
	m.errMsg = ""
}

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, isKey := msg.(tea.KeyMsg)
	if !isKey {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	if key.Type == tea.KeyCtrlC {
		return m.abort(), tea.Quit
	}

	switch m.step {
	case stepServerURL:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		if key.Type == tea.KeyEnter {
			value := strings.TrimSpace(m.input.Value())
			if value == "" {
				m.choices.ServerURL = ""
				m.enterBaseBranch()
				return m, nil
			}
			normalized, err := agent.NormalizeServerURL(value)
			if err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
			m.choices.ServerURL = normalized
			m.enterBaseBranch()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case stepBaseBranch:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		switch key.Type {
		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case tea.KeyEnter:
			switch m.cursor {
			case 0:
				m.choices.BaseBranch = "master"
				m.enterTools()
			case 1:
				m.choices.BaseBranch = "main"
				m.enterTools()
			case 2:
				m.step = stepBaseBranchCustom
				m.input = newWizardInput("branch name")
				m.errMsg = ""
				return m, textinput.Blink
			case 3:
				m.choices.BaseBranch = ""
				m.enterTools()
			}
		}

	case stepBaseBranchCustom:
		if key.Type == tea.KeyEsc {
			m.enterBaseBranch()
			return m, nil
		}
		if key.Type == tea.KeyEnter {
			value := strings.TrimSpace(m.input.Value())
			if value == "" {
				m.errMsg = "branch name cannot be empty — type a name, or press Esc to go back to the presets"
				return m, nil
			}
			m.choices.BaseBranch = value
			m.enterTools()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case stepTools:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		// Space arrives as KeyRunes{' '} on the Windows console driver and
		// KeySpace elsewhere; String() normalizes both.
		if key.String() == " " {
			provider := m.installedList[m.cursor]
			m.checked[provider] = !m.checked[provider]
			return m, nil
		}
		switch key.Type {
		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.installedList)-1 {
				m.cursor++
			}
		case tea.KeyEnter:
			for _, provider := range m.installedList {
				if m.checked[provider] {
					m.choices.Tools[provider] = provider
				}
			}
			m.enterConnectConfirm()
		}

	case stepConnectConfirm:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		switch key.Type {
		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case tea.KeyEnter:
			if m.cursor == 1 {
				m.step = stepConnectName
				m.input = newWizardInput("alias (empty = derived from the token)")
				m.pendingName = ""
				m.errMsg = ""
				m.connectAsked = true
				return m, textinput.Blink
			}
			m.enterSummary()
		}

	case stepConnectName:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		if key.Type == tea.KeyEnter {
			m.pendingName = strings.TrimSpace(m.input.Value())
			m.step = stepConnectToken
			m.input = newWizardInput("td_…")
			if m.pendingToken != "" {
				m.input.SetValue(m.pendingToken)
			}
			m.errMsg = ""
			return m, textinput.Blink
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case stepConnectToken:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		if key.Type == tea.KeyEnter {
			token := strings.TrimSpace(m.input.Value())
			if token == "" {
				m.errMsg = "token cannot be empty — paste the td_… token from the workspace web UI"
				return m, nil
			}
			name := m.pendingName
			if name == "" {
				name = agent.DeriveWorkspaceName(token)
			}
			if !strings.HasPrefix(token, "td_") {
				m.errMsg = "the daemon token must start with td_ (copy it from the workspace web UI)"
				return m, nil
			}
			if err := validateConnection(name, token, m.choices.Connections); err != nil {
				// A duplicate alias is an alias problem: go back to the alias
				// step with both fields prefilled.
				m.errMsg = err.Error()
				m.pendingToken = token
				m.step = stepConnectName
				m.input = newWizardInput("alias (empty = derived from the token)")
				m.input.SetValue(m.pendingName)
				return m, textinput.Blink
			}
			m.choices.Connections = append(m.choices.Connections, wizardConnection{Name: name, Token: token})
			m.enterConnectConfirm()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case stepSummary:
		if key.Type == tea.KeyEsc {
			return m.abort(), tea.Quit
		}
		switch key.Type {
		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case tea.KeyEnter:
			if m.cursor == 0 {
				m.written = true
				return m, tea.Quit
			}
			return m.abort(), tea.Quit
		}
	}
	return m, nil
}

func (m wizardModel) View() string {
	var b strings.Builder
	switch m.step {
	case stepServerURL:
		b.WriteString(titleStyle.Render("Server URL") + "\n\n")
		b.WriteString("The teammate server this daemon registers against.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(hintStyle.Render("Enter empty to skip — the daemon runs console-only and the URL\ncan be switched in the local console at any time. · Esc: abort") + "\n")
		if m.errMsg != "" {
			b.WriteString(errorStyle.Render(m.errMsg) + "\n")
		}

	case stepBaseBranch:
		b.WriteString(titleStyle.Render("Git base branch") + "\n\n")
		b.WriteString("The branch task branches are cut from (the repository's main line).\n\n")
		for i, option := range m.options {
			if i == m.cursor {
				b.WriteString(selectedStyle.Render("> "+option) + "\n")
			} else {
				b.WriteString("  " + option + "\n")
			}
		}
		b.WriteString(hintStyle.Render("↑/↓: move · Enter: choose · Esc: abort"))

	case stepBaseBranchCustom:
		b.WriteString(titleStyle.Render("Git base branch — custom") + "\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(hintStyle.Render("Enter: use this branch name · Esc: back to the presets · Ctrl-C: abort") + "\n")
		if m.errMsg != "" {
			b.WriteString(errorStyle.Render(m.errMsg) + "\n")
		}

	case stepTools:
		b.WriteString(titleStyle.Render("Coding tools") + "\n\n")
		b.WriteString("Detected on this machine (probed by bare command name):\n\n")
		for i, provider := range m.installedList {
			box := "[ ]"
			if m.checked[provider] {
				box = "[x]"
			}
			line := "  " + box + " " + provider
			if i == m.cursor {
				b.WriteString(selectedStyle.Render("> "+line) + "\n")
			} else {
				b.WriteString(line + "\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("space: toggle · Enter: confirm selection · Esc: abort") + "\n")
		b.WriteString(hintStyle.Render("Not listed = not installed; add later with config set tools.<provider>.path"))

	case stepConnectConfirm:
		question := "Add a workspace connection now?"
		if m.connectAsked {
			question = "Add another workspace connection?"
		}
		b.WriteString(titleStyle.Render("Workspace connections") + "\n\n")
		b.WriteString(question + " A connection is one td_ daemon token\nfrom that workspace's web UI.\n\n")
		for i, option := range m.options {
			if i == m.cursor {
				b.WriteString(selectedStyle.Render("> "+option) + "\n")
			} else {
				b.WriteString("  " + option + "\n")
			}
		}
		b.WriteString(hintStyle.Render("↑/↓: move · Enter: choose · Esc: abort"))

	case stepConnectName:
		b.WriteString(titleStyle.Render("Workspace connection — alias") + "\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(hintStyle.Render("Enter: continue (empty alias is derived from the token) · Esc: abort") + "\n")
		if m.errMsg != "" {
			b.WriteString(errorStyle.Render(m.errMsg) + "\n")
		}

	case stepConnectToken:
		b.WriteString(titleStyle.Render("Workspace connection — daemon token") + "\n\n")
		b.WriteString("Paste the td_… daemon token from the workspace web UI's daemon onboarding.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(hintStyle.Render("Enter: add connection · Esc: abort") + "\n")
		if m.errMsg != "" {
			b.WriteString(errorStyle.Render(m.errMsg) + "\n")
		}

	case stepSummary:
		b.WriteString(titleStyle.Render("Summary") + "\n\n")
		b.WriteString(renderSummary(m.choices))
		b.WriteString("\n")
		if hints := pendingHints(m.choices); len(hints) > 0 {
			b.WriteString("Still empty (fill any time):\n")
			for _, hint := range hints {
				b.WriteString("  " + hint + "\n")
			}
			b.WriteString("\n")
		}
		for i, option := range m.options {
			if i == m.cursor {
				b.WriteString(selectedStyle.Render("> "+option) + "\n")
			} else {
				b.WriteString("  " + option + "\n")
			}
		}
		b.WriteString(hintStyle.Render("↑/↓: move · Enter: choose · Esc: abort"))
	}
	return b.String()
}

// runInitWizard runs the interactive wizard and, when the summary is
// confirmed, writes the resulting config to path. Aborts write nothing.
func runInitWizard(path string) error {
	model := newWizardModel(installedProviders())
	final, err := tea.NewProgram(model).Run()
	if err != nil {
		return fmt.Errorf("run wizard: %w", err)
	}
	w, ok := final.(wizardModel)
	if !ok || w.aborted || !w.written {
		fmt.Println("Initialization aborted — nothing was written.")
		return nil
	}
	cfg := defaultGlobalConfig()
	w.choices.applyTo(cfg)
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		return fmt.Errorf("save agentd config: %w", err)
	}
	fmt.Println("Agent daemon config written to", path)
	fmt.Println()
	fmt.Println(renderSummary(w.choices))
	fmt.Println("Start the daemon with `teammate-agentd`; the local console URL is printed in its logs.")
	return nil
}
