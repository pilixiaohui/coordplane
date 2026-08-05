package core

import "testing"

func TestStableErrorCarriesConflictState(t *testing.T) {
	err := Conflict(CodeVersionConflict, "task changed", string(TaskRunning), 7)
	if err.Code != CodeVersionConflict || err.Retryable != true {
		t.Fatalf("error = %#v", err)
	}
	if err.State != string(TaskRunning) || err.Version != 7 {
		t.Fatalf("conflict context = %#v", err)
	}
}
