package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/telegram"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (a *App) runConfigure(args []string) error {
	if len(args) != 0 {
		return errors.New("configure takes no arguments")
	}
	if !isInteractiveTerminal() {
		return errors.New("configure requires an interactive terminal")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	updated, canceled, err := runConfigureTUI(cfg)
	if err != nil {
		return err
	}
	if canceled {
		return errors.New("configuration cancelled")
	}
	if err := telegram.Init(context.Background(), updated.Telegram.BotToken); err != nil {
		return fmt.Errorf("telegram setup failed: %w", err)
	}
	if err := config.Save(updated); err != nil {
		return err
	}
	fmt.Println("Configuration saved.")
	fmt.Printf("owner_chat_id=%d owner_user_id=%d default_model=%s\n", updated.Telegram.OwnerChatID, updated.Telegram.OwnerUserID, updated.DefaultModel)
	fmt.Printf("github_webhook_enabled=%t github_webhook_listen_addr=%s github_owner_login=%s\n", updated.GitHubWebhook.Enabled, strings.TrimSpace(updated.GitHubWebhook.ListenAddr), strings.TrimSpace(updated.GitHubWebhook.OwnerLogin))
	return nil
}

type configureField struct {
	Key         string
	Label       string
	Placeholder string
	Value       string
	Secret      bool
}

type configureModel struct {
	fields    []configureField
	inputs    []textinput.Model
	index     int
	errText   string
	submitted bool
	canceled  bool
	values    map[string]string
}

func newConfigureModel(cfg config.Config) configureModel {
	ownerID := preferredOwnerID(cfg.Telegram.OwnerChatID, cfg.Telegram.OwnerUserID)
	fields := []configureField{
		{
			Key:         "ollama_host",
			Label:       "Ollama Host",
			Placeholder: "http://localhost:11434",
			Value:       strings.TrimSpace(cfg.OllamaHost),
		},
		{
			Key:         "default_model",
			Label:       "Default Model",
			Placeholder: "kimi-k2.5:cloud",
			Value:       strings.TrimSpace(cfg.DefaultModel),
		},
		{
			Key:         "log_path",
			Label:       "Log Path",
			Placeholder: "~/.ollamaclaw/ollamaclaw.log",
			Value:       strings.TrimSpace(cfg.LogPath),
		},
		{
			Key:         "bot_token",
			Label:       "Telegram Bot Token",
			Placeholder: "123456:ABCDEF...",
			Value:       strings.TrimSpace(cfg.Telegram.BotToken),
			Secret:      true,
		},
		{
			Key:         "owner_id",
			Label:       "Telegram Owner ID",
			Placeholder: "123456789",
			Value:       ownerID,
		},
		{
			Key:         "github_owner_login",
			Label:       "GitHub Owner Login",
			Placeholder: "parth",
			Value:       strings.TrimSpace(cfg.GitHubWebhook.OwnerLogin),
		},
		{
			Key:         "github_webhook_secret",
			Label:       "GitHub Webhook Secret",
			Placeholder: "random-shared-secret",
			Value:       strings.TrimSpace(cfg.GitHubWebhook.Secret),
			Secret:      true,
		},
		{
			Key:         "github_webhook_listen_addr",
			Label:       "GitHub Webhook Listen Addr",
			Placeholder: "127.0.0.1:8787",
			Value:       strings.TrimSpace(cfg.GitHubWebhook.ListenAddr),
		},
		{
			Key:         "github_repo_allowlist",
			Label:       "GitHub Repo Allowlist",
			Placeholder: "owner/repo, owner/repo2 (optional)",
			Value:       strings.Join(cfg.GitHubWebhook.RepoAllowlist, ", "),
		},
	}

	inputs := make([]textinput.Model, 0, len(fields))
	for i, f := range fields {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 4096
		in.Width = 72
		in.Placeholder = f.Placeholder
		in.SetValue(f.Value)
		if i == 0 {
			in.Focus()
		} else {
			in.Blur()
		}
		if f.Secret {
			in.EchoMode = textinput.EchoPassword
			in.EchoCharacter = '*'
		}
		inputs = append(inputs, in)
	}

	return configureModel{
		fields: fields,
		inputs: inputs,
		index:  0,
	}
}

func (m configureModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m configureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.canceled = true
			return m, tea.Quit
		case "tab", "shift+tab", "up", "down":
			switch msg.String() {
			case "tab", "down":
				m.index = (m.index + 1) % len(m.inputs)
			default:
				m.index--
				if m.index < 0 {
					m.index = len(m.inputs) - 1
				}
			}
			for i := range m.inputs {
				if i == m.index {
					m.inputs[i].Focus()
				} else {
					m.inputs[i].Blur()
				}
			}
			return m, nil
		case "enter":
			values := m.readValues()
			if err := validateConfigureValues(values); err != nil {
				m.errText = err.Error()
				return m, nil
			}
			m.values = values
			m.submitted = true
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	for i := range m.inputs {
		if i == m.index {
			m.inputs[i], cmd = m.inputs[i].Update(msg)
			break
		}
	}
	return m, cmd
}

func (m configureModel) View() string {
	title := configureTitleStyle.Render("OllamaClaw Configure")
	help := configureHelpStyle.Render("Tab/Shift+Tab: move  Enter: save  Ctrl+C: cancel")
	lines := []string{title, help, ""}
	for i := range m.inputs {
		label := "Field"
		if i < len(m.fields) && strings.TrimSpace(m.fields[i].Label) != "" {
			label = m.fields[i].Label
		}
		if i == m.index {
			label = configureFocusLabelStyle.Render(label)
		} else {
			label = configureLabelStyle.Render(label)
		}
		lines = append(lines, label)
		lines = append(lines, m.inputs[i].View())
		lines = append(lines, "")
	}
	lines = append(lines, configureHintStyle.Render("Owner ID sets both owner_chat_id and owner_user_id."))
	lines = append(lines, configureHintStyle.Render("GitHub webhook activates when both Owner Login and Webhook Secret are set."))
	if strings.TrimSpace(m.errText) != "" {
		lines = append(lines, "", configureErrorStyle.Render(m.errText))
	}
	return configureContainerStyle.Render(strings.Join(lines, "\n"))
}

func (m configureModel) readValues() map[string]string {
	out := map[string]string{}
	for i, field := range m.fields {
		if i < len(m.inputs) {
			out[field.Key] = strings.TrimSpace(m.inputs[i].Value())
		}
	}
	return out
}

func runConfigureTUI(cfg config.Config) (config.Config, bool, error) {
	model := newConfigureModel(cfg)
	prog := tea.NewProgram(model)
	finalModel, err := prog.Run()
	if err != nil {
		return config.Config{}, false, err
	}
	m, ok := finalModel.(configureModel)
	if !ok {
		return config.Config{}, false, fmt.Errorf("configure ui internal error")
	}
	if m.canceled {
		return config.Config{}, true, nil
	}
	if !m.submitted {
		return config.Config{}, false, fmt.Errorf("configure ui did not submit")
	}
	values := m.values

	ownerID, err := strconv.ParseInt(values["owner_id"], 10, 64)
	if err != nil || ownerID <= 0 {
		return config.Config{}, false, fmt.Errorf("owner id must be a positive integer")
	}

	updated := cfg
	if v := strings.TrimSpace(values["ollama_host"]); v != "" {
		updated.OllamaHost = v
	}
	if v := strings.TrimSpace(values["default_model"]); v != "" {
		updated.DefaultModel = v
	}
	if v := strings.TrimSpace(values["log_path"]); v != "" {
		updated.LogPath = v
	}
	updated.Telegram.BotToken = strings.TrimSpace(values["bot_token"])
	updated.Telegram.OwnerChatID, updated.Telegram.OwnerUserID = normalizeOwnerIDs(ownerID, 0, 0)
	updated.GitHubWebhook.OwnerLogin = strings.TrimSpace(values["github_owner_login"])
	updated.GitHubWebhook.Secret = strings.TrimSpace(values["github_webhook_secret"])
	if v := strings.TrimSpace(values["github_webhook_listen_addr"]); v != "" {
		updated.GitHubWebhook.ListenAddr = v
	}
	updated.GitHubWebhook.RepoAllowlist = parseCSV(values["github_repo_allowlist"])
	updated.GitHubWebhook.Enabled = updated.GitHubWebhook.OwnerLogin != "" && updated.GitHubWebhook.Secret != ""
	return updated, false, nil
}

func validateConfigureValues(values map[string]string) error {
	if strings.TrimSpace(values["bot_token"]) == "" {
		return fmt.Errorf("telegram bot token is required")
	}
	ownerRaw := strings.TrimSpace(values["owner_id"])
	if ownerRaw == "" {
		return fmt.Errorf("telegram owner id is required")
	}
	ownerID, err := strconv.ParseInt(ownerRaw, 10, 64)
	if err != nil || ownerID <= 0 {
		return fmt.Errorf("telegram owner id must be a positive integer")
	}
	githubOwner := strings.TrimSpace(values["github_owner_login"])
	githubSecret := strings.TrimSpace(values["github_webhook_secret"])
	if githubOwner == "" && githubSecret != "" {
		return fmt.Errorf("github owner login is required when github webhook secret is set")
	}
	if githubOwner != "" && githubSecret == "" {
		return fmt.Errorf("github webhook secret is required when github owner login is set")
	}
	return nil
}

func preferredOwnerID(chatID, userID int64) string {
	switch {
	case chatID != 0 && userID != 0 && chatID == userID:
		return strconv.FormatInt(chatID, 10)
	case chatID != 0:
		return strconv.FormatInt(chatID, 10)
	case userID != 0:
		return strconv.FormatInt(userID, 10)
	default:
		return ""
	}
}

func parseCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	// Match Ollama's warm accent direction for a consistent first-run feel.
	configureAccent = lipgloss.AdaptiveColor{Light: "#F97316", Dark: "#FB923C"}
	configureMuted  = lipgloss.AdaptiveColor{Light: "#4B5563", Dark: "#9CA3AF"}
	configureError  = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}

	configureContainerStyle = lipgloss.NewStyle().
				Padding(1, 2).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(configureAccent)
	configureTitleStyle      = lipgloss.NewStyle().Bold(true).Foreground(configureAccent)
	configureHelpStyle       = lipgloss.NewStyle().Foreground(configureMuted)
	configureLabelStyle      = lipgloss.NewStyle().Bold(true)
	configureFocusLabelStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(configureAccent)
	configureHintStyle  = lipgloss.NewStyle().Foreground(configureMuted)
	configureErrorStyle = lipgloss.NewStyle().Foreground(configureError).Bold(true)
)
