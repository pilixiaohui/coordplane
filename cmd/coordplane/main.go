package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"coordplane/internal/backend"
	"coordplane/internal/releasehealth"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError("missing subcommand")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "release-health":
		return runReleaseHealth(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return usageError("unknown subcommand " + args[0])
	}
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("coordplane serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var cfg backend.Config
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path")
	flags.StringVar(&cfg.ListenAddr, "listen", ":8080", "HTTP listen address")
	flags.StringVar(&cfg.TeamConfigPath, "teamconfig", "", "optional TeamConfig YAML to load at startup")
	flags.StringVar(&cfg.TeamID, "team-id", "", "TeamConfig team_id to load when --teamconfig is omitted")
	flags.StringVar(&cfg.RuntimeWorkspaceRoot, "runtime-workspace-root", "", "external runtime workspace root")
	flags.StringVar(&cfg.RuntimeHomeRoot, "runtime-home-root", "", "external runtime home root")
	flags.StringVar(&cfg.DockerNetwork, "docker-network", "", "Docker network for managed docker runtimes")
	flags.StringVar(&cfg.BackendURL, "backend-url", "", "backend URL injected into runner sessions")
	flags.StringVar(&cfg.CoordlinkPath, "coordlink", "", "coordlink binary path for docker runtime injection")
	flags.StringVar(&cfg.ClaudeBinary, "claude-bin", "", "container path for the claude CLI profile")
	var claudeEnv string
	flags.StringVar(&claudeEnv, "claude-env", "", "comma-separated allowlisted host env keys to inject into Claude CLI runtime; defaults to the built-in Claude CLI env contract")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg.ClaudeEnvKeys = splitCSV(claudeEnv)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "coordplane serve listening on %s using db %s\n", firstNonEmpty(cfg.ListenAddr, ":8080"), cfg.DBPath)
	return backend.RunServe(ctx, cfg)
}

func runReleaseHealth(args []string) error {
	if len(args) == 0 {
		return usageError("release-health requires a scenario")
	}
	switch args[0] {
	case releasehealth.ScenarioCPAccept001:
		return runReleaseHealthCPAccept001(args[1:])
	case releasehealth.ScenarioCPProbe001:
		return runReleaseHealthCPProbe001(args[1:])
	default:
		return usageError("unknown release-health scenario " + args[0])
	}
}

func runReleaseHealthCPAccept001(args []string) error {
	flags := flag.NewFlagSet("coordplane release-health cp-accept-001", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var dbPath, rootContractID, teamID, teamConfig, runLabel, createdBy string
	var listenAddr, backendURL, coordlinkPath, dockerNetwork, claudeBinary, claudeEnv, workDir, inspectOut string
	var teamVersion int
	flags.StringVar(&dbPath, "db", filepath.Join(releasehealth.DefaultWorkDir, "coordplane.db"), "SQLite database path for durable release evidence")
	flags.StringVar(&rootContractID, "root-contract", "", "optional existing root contract id to evaluate instead of driving a new workflow")
	flags.StringVar(&teamID, "team-id", releasehealth.DefaultTeamID, "TeamConfig team_id expected to own the root contract and evidence")
	flags.IntVar(&teamVersion, "team-version", releasehealth.DefaultTeamVersion, "TeamConfig version expected to own the root contract and evidence")
	flags.StringVar(&teamConfig, "teamconfig", "", "TeamConfig YAML for the formal Docker/Claude release-health workflow")
	flags.StringVar(&listenAddr, "listen", releasehealth.DefaultListen, "HTTP listen address used while driving the workflow")
	flags.StringVar(&backendURL, "backend-url", releasehealth.DefaultPublicURL, "backend URL injected into Docker runtime coordlink sessions")
	flags.StringVar(&coordlinkPath, "coordlink", filepath.Join(releasehealth.DefaultWorkDir, "bin", "coordlink"), "coordlink binary path mounted into Docker runtime sessions")
	flags.StringVar(&dockerNetwork, "docker-network", releasehealth.DefaultNetwork, "Docker network for managed runtime containers")
	flags.StringVar(&claudeBinary, "claude-bin", "/usr/local/bin/claude", "container path for the Claude CLI profile")
	flags.StringVar(&claudeEnv, "claude-env", releasehealth.ClaudeEnvCSV(), "comma-separated allowlisted host env keys to inject into Claude CLI runtime")
	flags.StringVar(&workDir, "workdir", releasehealth.DefaultWorkDir, "release-health work directory for repository, DB, binaries, and evidence")
	flags.StringVar(&inspectOut, "inspect-out", "", "optional path to write inspect JSON after evaluation")
	flags.StringVar(&runLabel, "run-label", "cp-accept-001-release-health", "idempotency label for this release-health evaluation")
	flags.StringVar(&createdBy, "created-by", "release-health", "operator/internal actor recorded on release_acceptances")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rootContractID == "" {
		rootContractID = strings.TrimSpace(os.Getenv("COORDPLANE_ROOT_CONTRACT_ID"))
	}
	if dbPath == "" {
		return fmt.Errorf("coordplane release-health: --db is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	result, err := releasehealth.RunCPAccept001(ctx, releasehealth.CPAccept001Config{
		DBPath:        dbPath,
		RootContract:  rootContractID,
		TeamID:        teamID,
		TeamVersion:   teamVersion,
		TeamConfig:    teamConfig,
		ListenAddr:    listenAddr,
		BackendURL:    backendURL,
		CoordlinkPath: coordlinkPath,
		DockerNetwork: dockerNetwork,
		ClaudeBinary:  claudeBinary,
		ClaudeEnvKeys: splitCSV(claudeEnv),
		WorkDir:       workDir,
		RunLabel:      runLabel,
		CreatedBy:     createdBy,
	})
	acceptance := result.Acceptance
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if acceptance.ID != "" {
		if encodeErr := encoder.Encode(acceptance); encodeErr != nil {
			return encodeErr
		}
	}
	if inspectOut != "" && result.Inspect != nil {
		if writeErr := writeJSONFile(inspectOut, result.Inspect); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func runReleaseHealthCPProbe001(args []string) error {
	flags := flag.NewFlagSet("coordplane release-health cp-probe-001", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var dbPath, teamID, teamConfig, workDir, artifactDir, listenAddr, backendURL string
	var dockerTeamID, dockerTeamConfig, coordlinkPath, dockerNetwork, claudeBinary, claudeEnv string
	var runtimeWorkspaceRoot, runtimeHomeRoot, environmentBlocker string
	var teamVersion, dockerTeamVersion int
	flags.StringVar(&dbPath, "db", filepath.Join(releasehealth.DefaultWorkDir, "coordplane.db"), "SQLite database path for durable CP-PROBE release evidence")
	flags.StringVar(&teamID, "team-id", releasehealth.CPProbeDefaultTeamID, "TeamConfig team_id expected for CP-PROBE evidence")
	flags.IntVar(&teamVersion, "team-version", releasehealth.CPProbeDefaultTeamVersion, "TeamConfig version expected for CP-PROBE evidence")
	flags.StringVar(&teamConfig, "teamconfig", releasehealth.CPProbeTeamConfigPath, "TeamConfig YAML for the CP-PROBE release-health workflow")
	flags.StringVar(&dockerTeamID, "docker-team-id", releasehealth.CPProbeDockerTeamID, "Docker/Claude TeamConfig team_id expected for CP-PROBE replay evidence")
	flags.IntVar(&dockerTeamVersion, "docker-team-version", releasehealth.CPProbeDockerTeamVersion, "Docker/Claude TeamConfig version expected for CP-PROBE replay evidence")
	flags.StringVar(&dockerTeamConfig, "docker-teamconfig", releasehealth.CPProbeDockerTeamConfigPath, "TeamConfig YAML for the CP-PROBE Docker/Claude replay stage")
	flags.StringVar(&coordlinkPath, "coordlink", filepath.Join(releasehealth.DefaultWorkDir, "bin", "coordlink"), "coordlink binary path mounted into Docker runtime sessions")
	flags.StringVar(&dockerNetwork, "docker-network", releasehealth.DefaultNetwork, "Docker network for managed CP-PROBE runtime containers")
	flags.StringVar(&claudeBinary, "claude-bin", "/usr/local/bin/claude", "container path for the Claude CLI profile")
	flags.StringVar(&claudeEnv, "claude-env", releasehealth.ClaudeEnvCSV(), "comma-separated allowlisted host env keys to inject into Claude CLI runtime")
	flags.StringVar(&workDir, "workdir", releasehealth.DefaultWorkDir, "release-health work directory for CP-PROBE DB, fixtures, and artifacts")
	flags.StringVar(&artifactDir, "artifact-dir", "", "directory for CP-PROBE artifact files; defaults to --workdir")
	flags.StringVar(&listenAddr, "listen", releasehealth.DefaultListen, "backend listen address recorded for CP-PROBE backend")
	flags.StringVar(&backendURL, "backend-url", releasehealth.DefaultPublicURL, "backend URL injected into non-Docker runtime sessions")
	flags.StringVar(&runtimeWorkspaceRoot, "runtime-workspace-root", "", "external runtime workspace root; defaults under --workdir")
	flags.StringVar(&runtimeHomeRoot, "runtime-home-root", "", "external runtime home root; defaults under --workdir")
	flags.StringVar(&environmentBlocker, "environment-blocker", "", "blocker recorded in the CP-PROBE conclusion when Docker/Claude replay is unavailable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if dbPath == "" {
		return fmt.Errorf("coordplane release-health: --db is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	result, err := releasehealth.RunCPProbe001(ctx, releasehealth.CPProbe001Config{
		DBPath:               dbPath,
		TeamID:               teamID,
		TeamVersion:          teamVersion,
		TeamConfig:           teamConfig,
		DockerTeamID:         dockerTeamID,
		DockerTeamVersion:    dockerTeamVersion,
		DockerTeamConfig:     dockerTeamConfig,
		ListenAddr:           listenAddr,
		BackendURL:           backendURL,
		CoordlinkPath:        coordlinkPath,
		DockerNetwork:        dockerNetwork,
		ClaudeBinary:         claudeBinary,
		ClaudeEnvKeys:        splitCSV(claudeEnv),
		WorkDir:              workDir,
		ArtifactDir:          artifactDir,
		RuntimeWorkspaceRoot: runtimeWorkspaceRoot,
		RuntimeHomeRoot:      runtimeHomeRoot,
		EnvironmentBlocker:   environmentBlocker,
	})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		return encodeErr
	}
	return err
}

func usageError(message string) error {
	printUsage()
	return fmt.Errorf("coordplane: %s", message)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  coordplane serve --db /path/to/coordplane.db --listen :8080 [--teamconfig team.yaml] [--coordlink /path/to/coordlink] [--docker-network coordplane-release-health] [--claude-bin /usr/local/bin/claude] [--claude-env ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL]")
	fmt.Fprintln(os.Stderr, "  coordplane release-health cp-accept-001 [--db .coordplane-release-health/coordplane.db] [--coordlink .coordplane-release-health/bin/coordlink] [--root-contract ctr_root]")
	fmt.Fprintln(os.Stderr, "  coordplane release-health cp-probe-001 [--db .coordplane-release-health/coordplane.db] [--workdir .coordplane-release-health] [--docker-teamconfig team_config/fixtures/cp_probe_001_docker_claude.yaml] [--coordlink .coordplane-release-health/bin/coordlink]")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
