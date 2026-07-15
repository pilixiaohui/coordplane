package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestP2LegacyCoordinationOwnersCannotReturn(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"coordination", "delivery", "queue"} {
		if found, err := containsGoFile(filepath.Join(root, "internal", name)); err != nil {
			t.Fatal(err)
		} else if found {
			t.Errorf("legacy coordination package contains Go sources: internal/%s", name)
		}
	}
	legacyTypes := stringSet(
		"WorkContract", "Assignment", "Lease", "Attempt", "QueueItem",
		"Mailbox", "MailboxItem", "DeliveryAttempt", "Thread",
	)
	legacyTables := stringSet(
		"work_contracts", "assignments", "leases", "queue_items", "mailbox_items",
		"delivery_attempts", "session_routes", "agent_communication_envelopes",
	)
	var offenders []string
	for _, source := range productionSources(t) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.TypeSpec:
				if legacyTypes[typed.Name.Name] {
					offenders = append(offenders, source.rel+": type "+typed.Name.Name)
				}
			case *ast.BasicLit:
				if typed.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(typed.Value)
				if err != nil {
					break
				}
				for table := range legacyTables {
					if strings.Contains(value, table) {
						offenders = append(offenders, source.rel+": "+table)
					}
				}
			}
			return true
		})
	}
	failOffenders(t, "legacy coordination owners were reintroduced", offenders)
}

func TestP2ScriptedAdaptersAreExcludedFromProductionBuilds(t *testing.T) {
	var offenders []string
	for _, source := range productionSources(t) {
		found := false
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				found = found || strings.Contains(strings.ToLower(typed.Name), "scripted")
			case *ast.BasicLit:
				value, err := strconv.Unquote(typed.Value)
				found = found || (typed.Kind == token.STRING && err == nil && strings.Contains(strings.ToLower(value), "scripted"))
			}
			return !found
		})
		if found {
			offenders = append(offenders, source.rel)
		}
	}
	failOffenders(t, "scripted adapter code is selectable in a production build", offenders)
}
