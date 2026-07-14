//go:build docker

package daemon

import (
	"fmt"
	"os"
	"testing"
	"time"

	"coordplane/tests/testsupport"
)

func TestMain(m *testing.M) {
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "internal/daemon", 3*time.Minute)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := release(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
