package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"charm.land/huh/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/aphronio/dorf/internal/brand"
	"github.com/aphronio/dorf/internal/core"
)

const (
	setupForest      = "#30452B"
	setupMossTeal    = "#335446"
	setupLeafMoss    = "#678C28"
	setupWarmLichen  = "#D5C592"
	setupPathCream   = "#F5E5C7"
	setupMutedSage   = "#8F9D8D"
	setupHearthAmber = "#C47D1F"
	setupFormText    = "#E7E6E1"
	setupMutedText   = "#92948F"
)

type setupPresenter struct {
	output      io.Writer
	interactive bool
	color       bool
	accessible  bool
}

type setupConnectionMode string

const (
	setupConnectionChatGPT setupConnectionMode = "chatgpt"
	setupConnectionOpenAI  setupConnectionMode = "openai"
)

type setupGatewayMode string

const (
	setupGatewayCloudflare setupGatewayMode = "cloudflare"
	setupGatewayExisting   setupGatewayMode = "existing"
)

func newSetupPresenter(output io.Writer) setupPresenter {
	file, isFile := output.(*os.File)
	interactive := isFile && isTerminal(os.Stdin) && isTerminal(file)
	color := interactive && strings.TrimSpace(os.Getenv("NO_COLOR")) == "" && strings.TrimSpace(os.Getenv("TERM")) != "dumb"
	return setupPresenter{
		output: output, interactive: interactive, color: color,
		accessible: strings.TrimSpace(os.Getenv("ACCESSIBLE")) != "",
	}
}

func (p setupPresenter) Welcome() {
	if !p.interactive {
		return
	}
	fmt.Fprintln(p.output, brand.SetupBanner(p.color))
	fmt.Fprintln(p.output)
	p.Section("Foundation")
}

func (p setupPresenter) Section(title string) {
	if !p.interactive {
		return
	}
	style := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	if p.color {
		style = style.Foreground(lipgloss.Color(setupPathCream)).Background(lipgloss.Color(setupForest))
	}
	fmt.Fprintln(p.output, style.Render(strings.ToUpper(title)))
}

func (p setupPresenter) Ready(label, detail string) {
	if !p.interactive {
		fmt.Fprintf(p.output, "%s ready: %s\n", label, detail)
		return
	}
	check := lipgloss.NewStyle().Bold(true)
	name := lipgloss.NewStyle().Bold(true)
	value := lipgloss.NewStyle()
	if p.color {
		check = check.Foreground(lipgloss.Color(setupLeafMoss))
		value = value.Foreground(lipgloss.Color(setupMutedSage))
	}
	fmt.Fprintf(p.output, "  %s %s %s\n", check.Render("✓"), name.Render(fmt.Sprintf("%-18s", label)), value.Render(detail))
}

func (p setupPresenter) Note(label, detail string) {
	if !p.interactive {
		fmt.Fprintf(p.output, "%s: %s\n", label, detail)
		return
	}
	marker := lipgloss.NewStyle()
	name := lipgloss.NewStyle().Bold(true)
	value := lipgloss.NewStyle()
	if p.color {
		marker = marker.Foreground(lipgloss.Color(setupHearthAmber))
		name = name.Foreground(lipgloss.Color(setupWarmLichen))
		value = value.Foreground(lipgloss.Color(setupMutedSage))
	}
	fmt.Fprintf(p.output, "  %s %s %s\n", marker.Render("•"), name.Render(fmt.Sprintf("%-18s", label)), value.Render(detail))
}

func (p setupPresenter) Run(ctx context.Context, title string, action func(context.Context) error) error {
	if !p.interactive {
		return action(ctx)
	}
	theme := spinner.ThemeFunc(func(bool) *spinner.Styles {
		styles := &spinner.Styles{}
		if p.color {
			styles.Spinner = lipgloss.NewStyle().Foreground(lipgloss.Color(setupHearthAmber))
			styles.Title = lipgloss.NewStyle().Foreground(lipgloss.Color(setupMutedSage))
		}
		return styles
	})
	return spinner.New().
		Type(spinner.MiniDot).
		Title("  " + title).
		Context(ctx).
		ActionWithErr(action).
		WithOutput(p.output).
		WithInput(os.Stdin).
		WithAccessible(p.accessible).
		WithTheme(theme).
		Run()
}

func (p setupPresenter) ProviderGroup(selected *[]core.SandboxProvider, kvmAvailable bool) *huh.Group {
	options := []huh.Option[core.SandboxProvider]{}
	if kvmAvailable {
		options = append(options,
			huh.NewOption(p.option("Local · Incus", "Hardware-isolated Linux VMs on this machine · requires KVM"), core.SandboxProviderIncus),
		)
	}
	options = append(options,
		huh.NewOption(p.option("Cloud · E2B", "Managed Linux VMs"), core.SandboxProviderE2B),
	)
	fieldHeight := len(options) * 2
	if !kvmAvailable {
		fieldHeight += 3
	}
	field := huh.NewMultiSelect[core.SandboxProvider]().
		Options(options...).
		Value(selected).
		Filterable(false).
		Limit(len(options)).
		Height(fieldHeight)
	if !kvmAvailable {
		field.Title("  [—] Local · Incus").
			Description("      Unavailable on this machine · KVM not detected")
	}
	return huh.NewGroup(field).
		Title("Choose one or more locations now").
		Description("Each agent gets an isolated Sandbox. You can add more options later.")
}

func (p setupPresenter) HarnessGroup(selected *string) *huh.Group {
	return huh.NewGroup(
		huh.NewSelect[string]().
			Options(
				huh.NewOption(p.option("Codex", "OpenAI Codex Harness"), "codex"),
				huh.NewOption(p.option("Pi", "Pi coding-agent Harness"), "pi"),
			).
			Value(selected).
			Height(4),
	).Title("Which Harness should agents use?").
		Description("The selected Harness is verified inside every configured Sandbox profile.")
}

func (p setupPresenter) ConnectionGroup(selected *setupConnectionMode) *huh.Group {
	return huh.NewGroup(
		huh.NewSelect[setupConnectionMode]().
			Options(
				huh.NewOption(p.option("ChatGPT subscription", "Sign in with device confirmation"), setupConnectionChatGPT),
				huh.NewOption(p.option("OpenAI API key", "Usage is billed to your OpenAI API account"), setupConnectionOpenAI),
			).
			Value(selected).
			Height(4),
	).Title("How should your agents access OpenAI?").
		Description("The upstream credential stays on this host and never enters a Sandbox.")
}

func (p setupPresenter) CloudflareGatewayGroup(selected *setupGatewayMode, zone string) *huh.Group {
	return huh.NewGroup(
		huh.NewSelect[setupGatewayMode]().
			Options(
				huh.NewOption(p.option("Guided Cloudflare Tunnel", "Create and run a stable outbound-only route"), setupGatewayCloudflare),
				huh.NewOption(p.option("Existing HTTPS ingress", "Use routing infrastructure you already operate"), setupGatewayExisting),
			).
			Value(selected).
			Height(4),
	).Title("Cloudflare DNS detected for " + zone).
		Description("Choose how cloud Sandboxes should reach Dorf at this hostname.")
}

func (p setupPresenter) SecretGroup(title, description string, value *string) *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			EchoMode(huh.EchoModePassword).
			Value(value).
			Validate(func(raw string) error {
				if strings.TrimSpace(raw) == "" {
					return fmt.Errorf("a value is required")
				}
				return nil
			}),
	).Title(title).Description(description)
}

func (p setupPresenter) TextGroup(title, description, placeholder string, value *string, validate func(string) error) *huh.Group {
	field := huh.NewInput().Value(value).Placeholder(placeholder)
	if validate != nil {
		field.Validate(validate)
	}
	return huh.NewGroup(field).Title(title).Description(description)
}

func (p setupPresenter) ConfirmGroup(title, description string, confirmed *bool) *huh.Group {
	return huh.NewGroup(huh.NewConfirm().Affirmative("Continue").Negative("Cancel").Value(confirmed)).
		Title(title).Description(description)
}

func (p setupPresenter) RunForm(ctx context.Context, groups ...*huh.Group) error {
	output, ok := p.output.(*os.File)
	if !ok || !p.interactive {
		return fmt.Errorf("interactive setup requires a terminal")
	}
	form := huh.NewForm(groups...).
		WithInput(os.Stdin).
		WithOutput(output).
		WithKeyMap(setupKeyMap()).
		WithTheme(huh.ThemeFunc(setupTheme)).
		WithAccessible(p.accessible)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return errSetupCancelled
		}
		return err
	}
	return nil
}

func (p setupPresenter) option(title, description string) string {
	if !p.color {
		return title + "\n" + description
	}
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(setupMutedText))
	return title + "\n" + muted.Render(description)
}

func setupKeyMap() *huh.KeyMap {
	keymap := huh.NewDefaultKeyMap()
	keymap.MultiSelect.Toggle.SetKeys("space")
	keymap.MultiSelect.Toggle.SetHelp("space", "select")
	keymap.MultiSelect.Submit.SetHelp("enter", "continue")
	keymap.MultiSelect.Next.SetHelp("enter", "continue")
	return keymap
}

func setupTheme(isDark bool) *huh.Styles {
	styles := huh.ThemeBase(isDark)
	muted := lipgloss.Color(setupMutedText)

	styles.Group.Title = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(setupFormText))
	styles.Group.Description = lipgloss.NewStyle().Foreground(muted).MarginTop(1).MarginBottom(1)
	styles.Focused.Base = styles.Focused.Base.BorderForeground(lipgloss.Color(setupMossTeal))
	styles.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(setupLeafMoss)).SetString("› ")
	styles.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color(setupLeafMoss)).SetString("[✓] ")
	styles.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(muted).SetString("[ ] ")
	styles.Focused.SelectedOption = lipgloss.NewStyle().Bold(true)
	styles.Focused.UnselectedOption = lipgloss.NewStyle()
	styles.Focused.Title = lipgloss.NewStyle().Foreground(muted)
	styles.Focused.Description = lipgloss.NewStyle().Foreground(muted).MarginBottom(1)
	styles.Focused.ErrorIndicator = styles.Focused.ErrorIndicator.Foreground(lipgloss.Color(setupHearthAmber))
	styles.Focused.ErrorMessage = styles.Focused.ErrorMessage.Foreground(lipgloss.Color(setupHearthAmber))
	styles.Blurred = styles.Focused
	styles.Blurred.Base = styles.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	styles.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
	styles.Help.ShortKey = lipgloss.NewStyle().Foreground(lipgloss.Color(setupFormText))
	styles.Help.ShortDesc = lipgloss.NewStyle().Foreground(muted)
	styles.Help.ShortSeparator = lipgloss.NewStyle().Foreground(lipgloss.Color(setupForest))
	return styles
}
