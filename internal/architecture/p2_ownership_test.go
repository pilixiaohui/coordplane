package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestP2LegacyCoordinationOwnersCannotReturn(t *testing.T) {
	root := repositoryRoot(t)
	for _, rel := range []string{
		"internal/coordination",
		"internal/delivery",
		"internal/queue",
	} {
		exists, err := containsGoFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("legacy coordination package contains Go sources: %s", rel)
		}
	}

	legacyTypes := map[string]bool{
		"WorkContract":    true,
		"Assignment":      true,
		"Lease":           true,
		"Attempt":         true,
		"QueueItem":       true,
		"Mailbox":         true,
		"MailboxItem":     true,
		"DeliveryAttempt": true,
		"Thread":          true,
	}
	legacyTables := []string{
		"work_contracts",
		"assignments",
		"leases",
		"queue_items",
		"mailbox_items",
		"delivery_attempts",
		"session_routes",
		"agent_communication_envelopes",
	}
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				named := specification.(*ast.TypeSpec)
				if legacyTypes[named.Name.Name] {
					offenders = append(offenders, rel+": type "+named.Name.Name)
				}
			}
		}
		for _, table := range legacyTables {
			if strings.Contains(string(raw), table) {
				offenders = append(offenders, rel+": "+table)
			}
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("legacy coordination owners were reintroduced:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestP2ScriptedAdaptersAreExcludedFromProductionBuilds(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			selected, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			if !selected {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
			if err != nil {
				return err
			}
			found := false
			ast.Inspect(parsed, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.Ident:
					found = found || strings.Contains(strings.ToLower(typed.Name), "scripted")
				case *ast.BasicLit:
					if typed.Kind == token.STRING {
						value, unquoteErr := strconv.Unquote(typed.Value)
						found = found || (unquoteErr == nil && strings.Contains(strings.ToLower(value), "scripted"))
					}
				}
				return !found
			})
			if found {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				offenders = append(offenders, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("scripted adapter code is selectable in a production build: %s", strings.Join(offenders, ", "))
	}
}
