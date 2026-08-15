package core

import (
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaximumInstructionsBytes is the shared file/text prompt size limit. Both
// sources use the same 1 MiB / SHA-256 semantics so a Run can switch sources
// without changing the instructions hash rules.
const MaximumInstructionsBytes = 1 << 20

const maximumOptionalTokenBytes = 256
const maximumBaseURLBytes = 2048

var configTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// AdapterDescriptor is the read-only adapter metadata core needs for
// write-time Agent validation and GET /v1/adapters. It is injected by the
// runtime owner from the immutable production registry and never constructed
// from user input. The JSON shape intentionally contains no executable, argv
// template, host path, or secret.
type AdapterDescriptor struct {
	ID             string   `json:"-"`
	Name           string   `json:"name"`
	ExecutionModel string   `json:"execution_model"`
	SupportsResume bool     `json:"supports_resume"`
	SupportsInject bool     `json:"supports_inject"`
	AllowedEfforts []string `json:"allowed_efforts"`
}

// agentConfig is the normalized, validated config-domain projection shared by
// AddAgent and UpdateAgent.
type agentConfig struct {
	DisplayName      string
	AdapterID        string
	Image            string
	InstructionsFile string
	InstructionsText string
	Model            string
	SubagentModel    string
	BaseURL          string
	Effort           string
}

func (s *Service) validateAgentConfig(input AgentConfigInput) (agentConfig, error) {
	displayName, err := requireText("display_name", input.DisplayName)
	if err != nil {
		return agentConfig{}, err
	}
	adapterID, err := requireText("adapter_id", input.AdapterID)
	if err != nil {
		return agentConfig{}, err
	}
	if _, registered := s.adapters[adapterID]; !registered {
		return agentConfig{}, NewError(CodeInvalidArgument, "adapter_id is not registered", false)
	}
	image, err := requireText("image", input.Image)
	if err != nil {
		return agentConfig{}, err
	}

	instructionsFile, instructionsText, err := validateInstructionsSource(input.InstructionsFile, input.InstructionsText)
	if err != nil {
		return agentConfig{}, err
	}
	model, err := optionalConfigToken("model", input.Model)
	if err != nil {
		return agentConfig{}, err
	}
	subagentModel, err := optionalConfigToken("subagent_model", input.SubagentModel)
	if err != nil {
		return agentConfig{}, err
	}
	baseURL, err := optionalBaseURL(input.BaseURL)
	if err != nil {
		return agentConfig{}, err
	}
	effort, err := s.validatedEffort(adapterID, input.Effort)
	if err != nil {
		return agentConfig{}, err
	}
	return agentConfig{
		DisplayName: displayName, AdapterID: adapterID, Image: image,
		InstructionsFile: instructionsFile, InstructionsText: instructionsText,
		Model: model, SubagentModel: subagentModel, BaseURL: baseURL, Effort: effort,
	}, nil
}

func validateInstructionsSource(file, text string) (string, string, error) {
	file = strings.TrimSpace(file)
	fileProvided := file != ""
	textProvided := text != ""
	switch {
	case fileProvided && textProvided:
		return "", "", NewError(CodeInvalidArgument, "instructions_file and instructions_text are mutually exclusive", false)
	case !fileProvided && !textProvided:
		return "", "", NewError(CodeInvalidArgument, "instructions_file or instructions_text is required", false)
	case fileProvided:
		if _, err := requireText("instructions_file", file); err != nil {
			return "", "", err
		}
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			return "", "", NewError(CodeInvalidArgument, "instructions_file must be a canonical daemon-host absolute path", false)
		}
		return file, "", nil
	default:
		if !utf8.ValidString(text) {
			return "", "", NewError(CodeInvalidArgument, "instructions_text must be valid UTF-8", false)
		}
		if len(text) > MaximumInstructionsBytes {
			return "", "", NewError(CodeInvalidArgument, "instructions_text must not exceed 1 MiB", false)
		}
		return "", text, nil
	}
}

func optionalConfigToken(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maximumOptionalTokenBytes || !configTokenPattern.MatchString(value) {
		return "", NewError(CodeInvalidArgument, name+" must be a safe token matching [A-Za-z0-9._:/-]", false)
	}
	return value, nil
}

func optionalBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maximumBaseURLBytes {
		return "", NewError(CodeInvalidArgument, "base_url must not exceed 2048 bytes", false)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return "", NewError(CodeInvalidArgument, "base_url must be an https URL without userinfo, query, or fragment", false)
	}
	normalized := parsed.String()
	if normalized == "" {
		return "", NewError(CodeInvalidArgument, "base_url normalization produced an empty value", false)
	}
	return normalized, nil
}

func (s *Service) validatedEffort(adapterID, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	descriptor, registered := s.adapters[adapterID]
	if !registered {
		return "", NewError(CodeInvalidArgument, "adapter_id is not registered", false)
	}
	if len(descriptor.AllowedEfforts) == 0 {
		return "", NewError(CodeInvalidArgument, "adapter does not allow effort overrides", false)
	}
	for _, allowed := range descriptor.AllowedEfforts {
		if value == allowed {
			return value, nil
		}
	}
	return "", NewError(CodeInvalidArgument, "effort is not allowed for adapter_id", false)
}

// agentChangedFieldNames returns the sorted config field names that differ
// between an existing Agent and a validated replacement. Only names enter the
// Event; config values and prompt text are never persisted to the log.
func agentChangedFieldNames(before Agent, next agentConfig) []string {
	changed := make([]string, 0, 9)
	for name, different := range map[string]bool{
		"display_name":      before.DisplayName != next.DisplayName,
		"adapter_id":        before.AdapterID != next.AdapterID,
		"image":             before.Image != next.Image,
		"instructions_file": before.InstructionsFile != next.InstructionsFile,
		"instructions_text": before.InstructionsText != next.InstructionsText,
		"model":             before.Model != next.Model,
		"subagent_model":    before.SubagentModel != next.SubagentModel,
		"base_url":          before.BaseURL != next.BaseURL,
		"effort":            before.Effort != next.Effort,
	} {
		if different {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}

func agentUpdatedEventPayload(version int64, fields []string) string {
	return eventPayload(map[string]any{"version": version, "changed_fields": fields})
}
