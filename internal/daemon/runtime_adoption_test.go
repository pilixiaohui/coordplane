package daemon

import "testing"

func TestRuntimeContainerUIDMustDifferFromDaemon(t *testing.T) {
	if err := validateRuntimeContainerUID(runtimeContainerUID); err == nil {
		t.Fatal("daemon/container UID collision was accepted")
	}
	if err := validateRuntimeContainerUID(runtimeContainerUID - 1); err != nil {
		t.Fatalf("distinct daemon/container UIDs rejected: %v", err)
	}
}
