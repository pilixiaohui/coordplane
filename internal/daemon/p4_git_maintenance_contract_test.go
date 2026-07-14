//go:build contract

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestGT07PublicSourceCreateAndCheckoutSerializeWithRealTaskRefGC(t *testing.T) {
	for _, operation := range []string{"task_create_source", "task_checkout"} {
		t.Run(operation, func(t *testing.T) {
			h := newRealP4Harness(t)
			source := completeP4Task(t, h, "gt07-maintenance-"+operation)
			ready, release := filepath.Join(h.root, "task-ref-ready"), filepath.Join(h.root, "task-ref-release")
			t.Setenv("COORDPLANE_CONTRACT_TASK_REF_USE", source.TaskRef)
			t.Setenv("COORDPLANE_CONTRACT_TASK_REF_READY", ready)
			t.Setenv("COORDPLANE_CONTRACT_TASK_REF_RELEASE", release)

			type operationResult struct {
				task core.Task
				fact core.GitCheckoutFact
				err  error
			}
			operationDone := make(chan operationResult, 1)
			destination := filepath.Join(h.root, "checkout")
			go func() {
				if operation == "task_create_source" {
					task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
						ProjectID: h.project.ID, AssigneeAgentID: h.integrator.ID,
						Title: "durable source consumer", SourceTaskID: source.ID, RequestID: "gt07-public-source-create",
					})
					operationDone <- operationResult{task: task, err: err}
					return
				}
				fact, err := h.service.CheckoutTask(context.Background(), core.TaskCheckoutInput{
					TaskID: source.ID, Destination: destination,
				})
				operationDone <- operationResult{fact: fact, err: err}
			}()
			waitP4File(t, ready)
			gcDone := make(chan error, 1)
			go func() { gcDone <- h.service.ReconcileGitGC(context.Background(), source.ClosedAt) }()
			select {
			case err := <-gcDone:
				t.Fatalf("GC escaped the task-ref maintenance barrier: %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			assertP4TaskRef(t, h, source.TaskRef, source.HeadSHA)
			if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result := <-operationDone
			if result.err != nil {
				t.Fatal(result.err)
			}
			if err := <-gcDone; err != nil {
				t.Fatal(err)
			}
			if operation == "task_create_source" {
				if result.task.SourceTaskRef != source.TaskRef || result.task.SourceHeadSHA != source.HeadSHA {
					t.Fatalf("source-backed task = %#v", result.task)
				}
				assertP4TaskRef(t, h, source.TaskRef, source.HeadSHA)
			} else {
				if result.fact.Destination != destination || result.fact.HeadSHA != source.HeadSHA {
					t.Fatalf("public checkout = %#v", result.fact)
				}
				if got := strings.TrimSpace(gitIn(t, destination, "rev-parse", "HEAD^{commit}")); got != source.HeadSHA {
					t.Fatalf("checkout HEAD = %s, want %s", got, source.HeadSHA)
				}
				command := exec.Command("git", "--git-dir="+h.project.ControlRepoPath, "show-ref", "--verify", "--quiet", source.TaskRef)
				if err := command.Run(); err == nil {
					t.Fatalf("eligible task ref %s survived serialized checkout", source.TaskRef)
				}
			}
			gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
		})
	}
}
