package daemon

import (
	"testing"

	containerruntime "coordplane/internal/runtime"
)

// TestIsDaemonHelperRefNarrowFingerprint locks the orphan-isolation exclusion
// boundary (COD-64 Part 2): only the daemon's own deterministic helper
// container fingerprint — name prefix coordplane-git-(capture|inspect)- with a
// 12-hex digest suffix, AgentID git-helper, generation 1 and the same digest
// as LaunchNonce — is exempt from fail-closed orphan detection. Every other
// shape (run containers, mismatched nonce/agent/generation, malformed digest)
// must keep matching so a running container without a Run row still fails
// closed (acceptance.md RT-05: no-Run-row orphan containers are never guessed
// or deleted).
func TestIsDaemonHelperRefNarrowFingerprint(t *testing.T) {
	digest := "5185ed5d780f"
	opDigest := "0123456789abcdef0123456789abcdef"
	makeRef := func(name, agent, nonce string, generation int64) containerruntime.RuntimeRef {
		return containerruntime.RuntimeRef{
			ContainerName: name, ProjectID: "prj_test", TaskID: "tsk_test",
			AgentID: agent, RunID: opDigest, Generation: generation, LaunchNonce: nonce,
		}
	}
	for _, tc := range []struct {
		name string
		ref  containerruntime.RuntimeRef
		want bool
	}{
		{
			name: "inspect helper fingerprint matches",
			ref:  makeRef("coordplane-git-inspect-"+digest, "git-helper", digest, 1), want: true,
		},
		{
			name: "capture helper fingerprint matches",
			ref:  makeRef("coordplane-git-capture-"+digest, "git-helper", digest, 1), want: true,
		},
		{
			name: "run container never matches",
			ref:  makeRef("coordplane-run-run_abcdef0123456789abcdef0123456789", "agt_test", "launch-nonce", 1), want: false,
		},
		{
			name: "run container with git-helper agent still never matches",
			ref:  makeRef("coordplane-run-run_abcdef0123456789abcdef0123456789", "git-helper", "launch-nonce", 1), want: false,
		},
		{
			name: "helper name with nonce mismatch does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest, "git-helper", "ffffffffffff", 1), want: false,
		},
		{
			name: "helper name with foreign agent does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest, "agt_test", digest, 1), want: false,
		},
		{
			name: "helper name with generation 2 does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest, "git-helper", digest, 2), want: false,
		},
		{
			name: "inspect name with short digest does not match",
			ref:  makeRef("coordplane-git-inspect-5185ed5d780", "git-helper", "5185ed5d780", 1), want: false,
		},
		{
			name: "inspect name with long digest does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest+"00", "git-helper", digest+"00", 1), want: false,
		},
		{
			name: "inspect name with non-hex digest does not match",
			ref:  makeRef("coordplane-git-inspect-zzz5ed5d780f", "git-helper", "zzz5ed5d780f", 1), want: false,
		},
		{
			name: "bare helper prefix does not match",
			ref:  makeRef("coordplane-git-inspect-", "git-helper", "", 1), want: false,
		},
		{
			name: "unknown name shape does not match",
			ref:  makeRef("coordplane-other-"+digest, "git-helper", digest, 1), want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDaemonHelperRef(tc.ref); got != tc.want {
				t.Fatalf("isDaemonHelperRef(%#v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
