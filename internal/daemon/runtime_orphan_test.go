package daemon

import (
	"testing"

	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
)

// TestIsDaemonHelperRefNarrowFingerprint locks the orphan-isolation exclusion
// boundary (COD-64 Part 2, COD-65 rework): only the daemon's own deterministic
// helper container fingerprint — name prefix coordplane-git-(capture|inspect)-
// with a 24-hex digest suffix (hex.EncodeToString of the 12-byte SHA-256
// prefix, the shape captureRuntimeRef/inspectRuntimeRef actually produce),
// AgentID git-helper, generation 1 and the same digest as LaunchNonce — is
// exempt from fail-closed orphan detection. Every other shape (run containers,
// mismatched nonce/agent/generation, malformed or wrongly-sized digest) must
// keep matching so a running container without a Run row still fails closed
// (acceptance.md RT-05: no-Run-row orphan containers are never guessed or
// deleted). The pre-rework table locked 11/12/14-char digests — shapes
// production never generates — so the red-green evidence never exercised the
// real 24-hex name; the boundary cases below pin 12 (pre-rework buggy shape),
// 23, 24 and 25 chars.
func TestIsDaemonHelperRefNarrowFingerprint(t *testing.T) {
	digest := "a3b4c5d6e7f80123456789ab"
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
			ref:  makeRef("coordplane-git-inspect-"+digest, "git-helper", "ffffffffffffffffffffffff", 1), want: false,
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
			name: "inspect name with pre-rework 12-hex digest does not match",
			ref:  makeRef("coordplane-git-inspect-5185ed5d780f", "git-helper", "5185ed5d780f", 1), want: false,
		},
		{
			name: "inspect name with 23-hex digest does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest[:23], "git-helper", digest[:23], 1), want: false,
		},
		{
			name: "inspect name with 25-hex digest does not match",
			ref:  makeRef("coordplane-git-inspect-"+digest+"0", "git-helper", digest+"0", 1), want: false,
		},
		{
			name: "inspect name with non-hex digest does not match",
			ref:  makeRef("coordplane-git-inspect-zzz4c5d6e7f80123456789ab", "git-helper", "zzz4c5d6e7f80123456789ab", 1), want: false,
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

// TestIsDaemonHelperRefMatchesGeneratorDerivedRefs locks the derivation
// contract between the helper name generator (captureRuntimeRef /
// inspectRuntimeRef in git_capture_helper.go) and the orphan-isolation matcher
// (isDaemonHelperRef): any ref the generator produces for a real capture /
// inspect operation must be recognized as a daemon helper ref. COD-65 root
// cause was matcher/generator drift — helperRefDigest accepted 12-hex while
// hex.EncodeToString(digest[:12]) emits 24 hex chars — and only this
// generator-derived shape of test can catch it: a table test with hand-picked
// digests can always be made to agree with whichever shape the matcher
// happens to accept.
func TestIsDaemonHelperRefMatchesGeneratorDerivedRefs(t *testing.T) {
	capture := captureRuntimeRef(gitrepo.CaptureHelperRequest{
		ProjectID: "prj_contract", TaskID: "tsk_contract", RunID: "run_contract",
	})
	if !isDaemonHelperRef(capture) {
		t.Fatalf("isDaemonHelperRef(captureRuntimeRef(...)) = false, want true: %#v", capture)
	}
	inspect := inspectRuntimeRef(gitrepo.WorkspaceInspectRequest{
		ProjectID: "prj_contract", TaskID: "tsk_contract", Workspace: "/workspace/contract",
	}, "op_contract")
	if !isDaemonHelperRef(inspect) {
		t.Fatalf("isDaemonHelperRef(inspectRuntimeRef(...)) = false, want true: %#v", inspect)
	}
}
