package testsupport

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAcquireSerialResourceBlocksConcurrentPackageOwner(t *testing.T) {
	resource := fmt.Sprintf("contract-%d", os.Getpid())
	t.Cleanup(func() { _ = os.Remove(serialResourcePath(resource)) })
	release, err := AcquireSerialResource(resource, "first-package", time.Second)
	RequireNoError(t, err)
	t.Cleanup(func() { _ = release() })

	if _, err := AcquireSerialResource(resource, "competing-package", 75*time.Millisecond); err == nil || !strings.Contains(err.Error(), "first-package") {
		t.Fatalf("competing acquisition error = %v, want current owner diagnostic", err)
	}
	RequireNoError(t, release())
	reacquired, err := AcquireSerialResource(resource, "next-package", time.Second)
	if err != nil {
		t.Fatalf("reacquire released resource: %v", err)
	}
	RequireNoError(t, reacquired())
}
