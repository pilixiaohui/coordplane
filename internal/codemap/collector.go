package codemap

import "context"

const (
	CollectionModeFull        = "full"
	CollectionModeIncremental = "incremental_request"
)

type Collector interface {
	Name() string
	Version() string
	Collect(context.Context, CollectContext) (Collection, error)
}

type CollectContext struct {
	Root         string
	ModulePath   string
	Mode         string
	ChangedFiles []string
}

type Collection struct {
	Nodes       []Node
	Edges       []Edge
	Diagnostics []Diagnostic
	InputFiles  []InputFile
}

type InputFile struct {
	Path   string
	Digest string
}

func DefaultCollectors() []Collector {
	return []Collector{
		DocsCollector{},
		GoCollector{},
		TestCollector{},
		ConfigCollector{},
		ScriptCollector{},
	}
}
