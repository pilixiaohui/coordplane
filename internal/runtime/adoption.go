package runtime

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ValidateAdoption proves that an existing container still has the isolation
// contract expected by the durable Run before it may be started or monitored.
func ValidateAdoption(expected ContainerSpec, actual LiveState) error {
	if err := ValidateOwnership(expected.Ref, actual.Ref); err != nil {
		return err
	}
	if actual.Image != expected.Image {
		return isolationMismatch("image")
	}
	if !equalStrings(actual.Entrypoint, []string{expected.Command.Executable}) ||
		!equalStrings(actual.CommandArgs, expected.Command.Args) {
		return isolationMismatch("command")
	}
	if err := validateAdoptionEnvironment(expected, actual.Environment); err != nil {
		return err
	}
	if actual.WorkingDir != expected.WorkingDir {
		return isolationMismatch("working directory")
	}
	if err := validateNonRootUser(actual.User); err != nil {
		return err
	}
	if actual.User != expected.User {
		return isolationMismatch("user")
	}
	if !equalStringsAsSet(actual.GroupAdd, expected.GroupAdd) {
		return isolationMismatch("supplementary groups")
	}
	if actual.Network != expected.Network {
		return isolationMismatch("network")
	}
	if actual.AutoRemove || actual.RestartPolicy != "no" {
		return isolationMismatch("lifecycle policy")
	}
	if actual.Privileged || len(actual.CapAdd) != 0 || len(actual.CapDrop) != 1 || !containsFold(actual.CapDrop, "ALL") {
		return isolationMismatch("Linux privileges")
	}
	if !actual.ReadonlyRootfs || !expected.ReadOnlyRoot {
		return isolationMismatch("read-only root filesystem")
	}
	if len(actual.SecurityOpt) != 1 || !containsSecurityOption(actual.SecurityOpt, "no-new-privileges") {
		return isolationMismatch("no-new-privileges")
	}
	if actual.PublishedPorts != 0 {
		return isolationMismatch("published ports")
	}
	if actual.PIDsLimit != expected.Limits.PIDs || actual.MemoryBytes != expected.Limits.MemoryBytes ||
		actual.NanoCPUs != expected.Limits.NanoCPUs {
		return isolationMismatch("resource limits")
	}
	if !actual.Init {
		return isolationMismatch("init process")
	}
	if err := validateAdoptionTmpfs(expected.Limits.TmpfsBytes, actual.Tmpfs); err != nil {
		return err
	}
	if err := validateAdoptionMounts(expected.Mounts, actual.Mounts); err != nil {
		return err
	}
	return nil
}

func validateAdoptionEnvironment(expected ContainerSpec, actual []EnvironmentFact) error {
	sensitive := make(map[string]struct{}, len(expected.SensitiveEnvKeys))
	for _, name := range expected.SensitiveEnvKeys {
		if _, exists := sensitive[name]; exists {
			return isolationMismatch("environment")
		}
		sensitive[name] = struct{}{}
	}
	actualByName := make(map[string]string, len(actual))
	for _, fact := range actual {
		if fact.Name == "" || fact.ValueDigest == "" {
			return isolationMismatch("environment")
		}
		if _, exists := actualByName[fact.Name]; exists {
			return isolationMismatch("environment")
		}
		actualByName[fact.Name] = fact.ValueDigest
	}
	for name, value := range expected.Command.Env {
		actualDigest, exists := actualByName[name]
		if !exists {
			return isolationMismatch("environment")
		}
		if _, secret := sensitive[name]; !secret && actualDigest != environmentValueDigest(value) {
			return isolationMismatch("environment")
		}
	}
	return nil
}

func validateAdoptionTmpfs(size int64, actual map[string]string) error {
	if size <= 0 || len(actual) != 1 {
		return isolationMismatch("tmpfs")
	}
	options, exists := actual["/tmp"]
	if !exists {
		return isolationMismatch("tmpfs")
	}
	want := map[string]struct{}{
		"rw": {}, "nosuid": {}, "nodev": {}, "size=" + strconv.FormatInt(size, 10): {},
	}
	got := make(map[string]struct{})
	for _, option := range strings.Split(options, ",") {
		option = strings.TrimSpace(option)
		if option == "" {
			return isolationMismatch("tmpfs")
		}
		if _, duplicate := got[option]; duplicate {
			return isolationMismatch("tmpfs")
		}
		got[option] = struct{}{}
	}
	if len(got) != len(want) {
		return isolationMismatch("tmpfs")
	}
	for option := range want {
		if _, exists := got[option]; !exists {
			return isolationMismatch("tmpfs")
		}
	}
	return nil
}

// ValidateOwnership compares every durable container identity fence. An empty
// expected ContainerID is allowed only for create-before-ID-persistence recovery.
func ValidateOwnership(expected, actual RuntimeRef) error {
	if actual.ContainerName != expected.ContainerName || actual.ProjectID != expected.ProjectID ||
		actual.TaskID != expected.TaskID || actual.AgentID != expected.AgentID || actual.RunID != expected.RunID ||
		actual.Generation != expected.Generation || actual.LaunchNonce != expected.LaunchNonce ||
		(expected.ContainerID != "" && actual.ContainerID != expected.ContainerID) {
		return isolationMismatch("runtime identity")
	}
	return nil
}

func validateNonRootUser(user string) error {
	uid := strings.SplitN(strings.TrimSpace(user), ":", 2)[0]
	value, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || value == 0 {
		return isolationMismatch("nonroot user")
	}
	return nil
}

func validateAdoptionMounts(expected []Mount, actual []MountFact) error {
	if len(actual) != len(expected) {
		return isolationMismatch("mount set")
	}
	want := make(map[string]Mount, len(expected))
	for _, mount := range expected {
		target := filepath.Clean(mount.Target)
		if _, exists := want[target]; exists {
			return isolationMismatch("mount set")
		}
		mount.Source = filepath.Clean(mount.Source)
		want[target] = mount
	}
	for _, mount := range actual {
		target := filepath.Clean(mount.Destination)
		expectedMount, ok := want[target]
		if !ok || mount.Type != "bind" || filepath.Clean(mount.Source) != expectedMount.Source ||
			mount.ReadWrite == expectedMount.ReadOnly || mount.Propagation != "rprivate" {
			return isolationMismatch("mount set")
		}
		delete(want, target)
	}
	if len(want) != 0 {
		return isolationMismatch("mount set")
	}
	return nil
}

func equalStringsAsSet(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func containsSecurityOption(values []string, want string) bool {
	for _, value := range values {
		if value == want || value == want+":true" {
			return true
		}
	}
	return false
}

func isolationMismatch(field string) error {
	return fmt.Errorf("%w: container isolation %s mismatch", ErrOwnership, field)
}
