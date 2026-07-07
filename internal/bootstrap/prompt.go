package bootstrap

import (
	"fmt"
	"sort"
	"strings"

	"coordplane/internal/skills"
	"coordplane/internal/teamconfig"
)

type Context struct {
	Agent             teamconfig.AgentConfig
	SkillSummaries    []skills.Summary
	AssignmentSummary string
}

func Compose(ctx Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent: %s\n", ctx.Agent.ID)
	if ctx.Agent.RolePrompt != "" {
		fmt.Fprintf(&b, "Role prompt (quoted TeamConfig text):\n%s\n", quoteConfigText(ctx.Agent.RolePrompt))
	}
	if ctx.AssignmentSummary != "" {
		fmt.Fprintf(&b, "Current assignment:\n%s\n", strings.TrimSpace(ctx.AssignmentSummary))
	}

	summaries := append([]skills.Summary(nil), ctx.SkillSummaries...)
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})
	b.WriteString("Available skills:\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "- %s@v%d: %s\n  Read with: coordlink skill read %s\n",
			summary.Name, summary.Version, summary.Summary, summary.Name)
	}
	b.WriteString("Use coordlink capability calls for backend state. Read a relevant skill before acting when workflow details are needed.\n")
	b.WriteString("Do not infer hidden backend state, inspect other agents' private data, or expect full capability schemas in this prompt.\n")
	return b.String()
}

func quoteConfigText(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		lines[i] = "  | " + neutralizePromptMarker(strings.TrimRight(line, " \t"))
	}
	return strings.Join(lines, "\n")
}

func neutralizePromptMarker(line string) string {
	replacements := map[string]string{
		"Available skills:": "Available skills (quoted text):",
		"Read with:":        "Read with (quoted text):",
		"input_schema":      "schema marker omitted",
		"output_schema":     "schema marker omitted",
		"rejected_schema":   "schema marker omitted",
	}
	for old, replacement := range replacements {
		line = strings.ReplaceAll(line, old, replacement)
	}
	return line
}
