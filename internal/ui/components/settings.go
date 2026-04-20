// Package components — SettingsModel renders an interactive settings
// screen (huh form) for the banya-cli. Currently exposes one category:
// subagent/critic model configuration. On submit the form writes the
// values to ~/.config/banya/config.yaml; the caller is responsible for
// relaunching the sidecar so env-var propagation picks up the changes.
package components

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/cascadecodes/banya-cli/internal/config"
)

// SettingsResult is returned from a SettingsModel when the form closes.
// Saved=true means the user pressed submit and the config was written.
type SettingsResult struct {
	Saved    bool
	Subagent config.SubagentConfig
	Language string // ko | en — default language written to config
	// LLMPresetID is non-empty when the user switched the main model
	// preset. The UI layer applies it via Model.ApplyLLMPreset so the
	// running ProcessClient's LLMBackend swaps without restart.
	LLMPresetID string
	Err         error
}

// SettingsClosedMsg is emitted by the Settings model (via tea.Cmd) to
// let the parent Model know the form is done and transition back to
// StateReady.
type SettingsClosedMsg struct {
	Result SettingsResult
}

// SettingsModel wraps a huh.Form that edits SubagentConfig + the global
// language preference.
type SettingsModel struct {
	form        *huh.Form
	provider    string
	model       string
	apiKey      string
	endpoint    string
	language    string // ko | en
	llmPresetID string // "" means keep current; otherwise one of LLMPresets.ID
	done        bool
	result      SettingsResult
	width       int
	height      int
}

// NewSettingsModel seeds the form with current config values.
func NewSettingsModel(current config.SubagentConfig, currentLang string, currentLLM config.LLMServerConfig) SettingsModel {
	if currentLang == "" {
		currentLang = config.LanguageKorean
	}
	llmID := ""
	if p := config.MatchPresetFromConfig(currentLLM); p != nil {
		llmID = p.ID
	}
	s := SettingsModel{
		provider:    current.Provider,
		model:       current.Model,
		apiKey:      current.APIKey,
		endpoint:    current.Endpoint,
		language:    currentLang,
		llmPresetID: llmID,
	}
	s.form = buildForm(&s)
	return s
}

func buildForm(s *SettingsModel) *huh.Form {
	// Main-model options derived from config.LLMPresets.
	mainOpts := []huh.Option[string]{huh.NewOption("(keep current)", "")}
	for _, p := range config.LLMPresets {
		label := p.Label
		if p.Beta {
			label += " [beta]"
		}
		mainOpts = append(mainOpts, huh.NewOption(label, p.ID))
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Main Agent Model").
				Description("Primary LLM the agent uses for reasoning + tool calls. Switching here hot-swaps the backend on submit; the API key must be exported via the preset's env var (shown in /model)."),
			huh.NewSelect[string]().
				Title("Main model preset").
				Options(mainOpts...).
				Value(&s.llmPresetID),
		),
		huh.NewGroup(
			huh.NewNote().
				Title("Language / 언어").
				Description("Default response language. The agent still matches the language of each user message at runtime — this is only the fallback."),
			huh.NewSelect[string]().
				Title("Default language").
				Options(
					huh.NewOption("한국어 (Korean)", config.LanguageKorean),
					huh.NewOption("English", config.LanguageEnglish),
				).
				Value(&s.language),
		),
		huh.NewGroup(
			huh.NewNote().
				Title("Subagent / Critic Model").
				Description("보조 LLM 설정. banya-core 가 spawn_agent / compactor / critic 용도로 사용. 설정을 비우면 메인 LLM 으로 폴백."),
			huh.NewSelect[string]().
				Title("Provider").
				Options(
					huh.NewOption("(disabled)", ""),
					huh.NewOption("Gemini (Google)", "gemini"),
					huh.NewOption("Anthropic (Claude)", "anthropic"),
					huh.NewOption("OpenAI-compat (Qwen / vLLM 등)", "openai-compat"),
				).
				Value(&s.provider),
			huh.NewInput().
				Title("Model").
				Placeholder("예: gemini-3-flash-preview / claude-opus-4-5 / qwen3.5-coder-32b").
				Value(&s.model),
			huh.NewInput().
				Title("API Key").
				Placeholder("sk-... / AI... / 등").
				EchoMode(huh.EchoModePassword).
				Value(&s.apiKey),
			huh.NewInput().
				Title("Endpoint (선택)").
				Placeholder("기본값 쓰려면 빈 칸. 예: https://generativelanguage.googleapis.com/v1beta").
				Value(&s.endpoint),
		),
	).WithTheme(huh.ThemeBase16()).WithShowHelp(true)
}

// Init delegates to the inner form.
func (m SettingsModel) Init() tea.Cmd { return m.form.Init() }

// Update forwards messages to the form and detects completion.
// ESC → cancel (Saved=false). Enter on last field → submit + save.
func (m SettingsModel) Update(msg tea.Msg) (SettingsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
			m.done = true
			m.result = SettingsResult{Saved: false}
			return m, func() tea.Msg { return SettingsClosedMsg{Result: m.result} }
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	formModel, cmd := m.form.Update(msg)
	if f, ok := formModel.(*huh.Form); ok {
		m.form = f
	}

	if m.form.State == huh.StateCompleted {
		sub := config.SubagentConfig{
			Provider: m.provider,
			Model:    m.model,
			APIKey:   m.apiKey,
			Endpoint: m.endpoint,
		}
		err := config.SaveSubagent(sub)
		if err == nil {
			err = config.SaveLanguage(m.language)
		}
		// Note: main-model preset change is applied by the caller via
		// Model.ApplyLLMPreset when LLMPresetID is non-empty — it does
		// both config persistence and ProcessClient backend swap.
		m.done = true
		m.result = SettingsResult{
			Saved:       err == nil,
			Subagent:    sub,
			Language:    m.language,
			LLMPresetID: m.llmPresetID,
			Err:         err,
		}
		return m, func() tea.Msg { return SettingsClosedMsg{Result: m.result} }
	}
	return m, cmd
}

// View renders the form inside a bordered box.
func (m SettingsModel) View() string {
	if m.done {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#00FF41")).
		Padding(1, 2).
		Width(max(60, m.width-8)).
		Render(m.form.View())
	hint := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#888888")).
		Render("Tab/Shift+Tab 으로 필드 이동, Enter 로 다음 필드, 마지막에 Submit, Esc 취소")
	return lipgloss.JoinVertical(lipgloss.Left, box, "", hint)
}

// Done reports whether the form has closed (either submit or cancel).
func (m SettingsModel) Done() bool { return m.done }

// Result returns the final SettingsResult (valid after Done()).
func (m SettingsModel) Result() SettingsResult { return m.result }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
