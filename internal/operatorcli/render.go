package operatorcli

import (
	"encoding/json"
	"io"
	"strings"
)

// Human output is the indented form of the same public result used by JSON
// clients, avoiding a second object-specific rendering surface.
func render(writer io.Writer, mode string, value any) error {
	encoder := json.NewEncoder(writer)
	if !strings.EqualFold(strings.TrimSpace(mode), "json") {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}
