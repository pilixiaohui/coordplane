package releasehealth

import (
	"strings"

	"coordplane/internal/claudeenv"
)

const (
	ScenarioCPAccept001 = "cp-accept-001"
	ScenarioCPProbe001  = "cp-probe-001"

	DefaultTeamID      = "cp-accept-001-three-agent-docker-claude"
	DefaultTeamVersion = 1

	CPProbeDefaultTeamID      = "cp-probe-001-manual-service"
	CPProbeDefaultTeamVersion = 1
	CPProbeDockerTeamID       = "cp-probe-001-docker-claude"
	CPProbeDockerTeamVersion  = 1

	DefaultWorkDir   = ".coordplane-release-health"
	DefaultImage     = "coordplane/claude-runtime:release-health"
	DefaultNetwork   = "coordplane-release-health"
	DefaultListen    = "0.0.0.0:18080"
	DefaultPublicURL = "http://127.0.0.1:18080"

	DockerfilePath       = "docker/claude-runtime/Dockerfile"
	TeamConfigPath       = "team_config/fixtures/cp_accept_001_three_agent_docker_claude.yaml"
	OperationManualPath  = "testing_acceptance/release_health_cp_accept_001.md"
	ReleaseHealthScript  = "scripts/release-health-cp-accept-001.sh"
	ReleaseHealthMakeArg = "release-health-cp-accept-001"

	CPProbeTeamConfigPath       = "team_config/fixtures/cp_probe_001_manual_service.yaml"
	CPProbeDockerTeamConfigPath = "team_config/fixtures/cp_probe_001_docker_claude.yaml"
	CPProbeReleaseHealthScript  = "scripts/release-health-cp-probe-001.sh"
	CPProbeReleaseHealthMakeArg = "release-health-cp-probe-001"
)

var InspectLeakDenylist = []string{
	"coordplane.db",
	"ANTHROPIC_AUTH_TOKEN=",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY=",
	"ANTHROPIC_API_KEY",
	"CLAUDE_AUTH_TOKEN=",
	"CLAUDE_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN=",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"sk-",
	"Bearer ",
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/.claude",
	"/.config",
	"/.ssh",
}

func ClaudeEnvCSV() string {
	return strings.Join(claudeenv.RuntimeKeys, ",")
}
