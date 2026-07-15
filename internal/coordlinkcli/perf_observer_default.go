//go:build !perf

package coordlinkcli

import "io"

func withPerfObserver(client jsonClient, _ io.Writer) jsonClient { return client }
