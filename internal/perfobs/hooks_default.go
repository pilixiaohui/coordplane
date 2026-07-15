//go:build !perf

package perfobs

import "context"

func Start(context.Context) error         { return nil }
func Stop() error                         { return nil }
func Received(string, Fields, string)     {}
func FailedReceived(string, Fields)       {}
func Point(string, Fields, string)        {}
func StartStage(string, string, Fields)   {}
func EndStage(string, string, string)     {}
func ClientLine([]byte, Fields)           {}
func Fault(context.Context, string) error { return nil }
