package coordlinkcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpAdvertisesP2OutcomeCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, nil, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, command := range []string{"task wait", "task submit", "task fail"} {
		if !strings.Contains(stdout.String(), command) {
			t.Fatalf("help omits P2 command %q:\n%s", command, stdout.String())
		}
	}
}
