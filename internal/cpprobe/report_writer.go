package cpprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ReportArtifacts struct {
	ManualTrace   ManualTrace
	Inspect       RedactedInspect
	GitSummary    GitOperationSummary
	FailureMatrix FailureMatrix
	Conclusion    ConclusionReport
}

func WriteReportArtifacts(dir string, artifacts ReportArtifacts) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("cp-probe report writer: output dir is required")
	}
	if err := artifacts.ManualTrace.Validate(); err != nil {
		return err
	}
	if err := artifacts.Inspect.Validate(); err != nil {
		return err
	}
	if err := artifacts.GitSummary.Validate(); err != nil {
		return err
	}
	if err := artifacts.FailureMatrix.Validate(); err != nil {
		return err
	}
	if err := artifacts.Conclusion.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cp-probe report dir: %w", err)
	}
	writes := map[string][]byte{
		ManualTraceArtifact:         []byte(renderManualTrace(artifacts.ManualTrace)),
		FailureMatrixArtifact:       []byte(renderFailureMatrix(artifacts.FailureMatrix)),
		ConclusionReportArtifact:    []byte(renderConclusion(artifacts.Conclusion)),
		InspectRedactedArtifact:     mustJSON(artifacts.Inspect),
		GitOperationSummaryArtifact: mustJSON(artifacts.GitSummary),
	}
	for name, body := range writes {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

func renderManualTrace(trace ManualTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s Manual Trace\n\n", trace.Scenario)
	b.WriteString("| # | actor | entrypoint | capability | status | error_code | next_actions |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for i, step := range trace.Steps {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n",
			i+1,
			mdCell(step.Actor),
			mdCell(step.EntryPoint),
			mdCell(step.Capability),
			mdCell(step.Status),
			mdCell(step.ErrorCode),
			mdCell(strings.Join(step.NextActions, ", ")),
		)
	}
	return b.String()
}

func renderFailureMatrix(matrix FailureMatrix) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s Failure Matrix\n\n", matrix.Scenario)
	b.WriteString("| id | status | capability | expected_error_codes | actual_error_code | state_assertion | next_step |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, item := range matrix.Items {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			mdCell(item.ID),
			mdCell(item.Status),
			mdCell(item.Capability),
			mdCell(strings.Join(item.ExpectedErrorCodes, ", ")),
			mdCell(item.ActualErrorCode),
			mdCell(item.StateAssertion),
			mdCell(item.NextStep),
		)
	}
	return b.String()
}

func renderConclusion(report ConclusionReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s Conclusion\n\n", report.Scenario)
	fmt.Fprintf(&b, "Status: %s\n\n", report.Status)
	fmt.Fprintf(&b, "- Manual trace: %s\n", report.ManualTraceRef)
	fmt.Fprintf(&b, "- Inspect: %s\n", report.InspectRef)
	fmt.Fprintf(&b, "- Git summary: %s\n", report.GitSummaryRef)
	fmt.Fprintf(&b, "- Failure matrix: %s\n\n", report.FailureMatrixRef)
	b.WriteString("## Covered\n\n")
	for _, covered := range report.Covered {
		fmt.Fprintf(&b, "- %s\n", covered)
	}
	if len(report.NotCovered) > 0 {
		b.WriteString("\n## Not Covered\n\n")
		for _, item := range report.NotCovered {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	if len(report.NextSteps) > 0 {
		b.WriteString("\n## Next Steps\n\n")
		for _, item := range report.NextSteps {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return b.String()
}

func mustJSON(value any) []byte {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func mdCell(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	return strings.TrimSpace(value)
}
