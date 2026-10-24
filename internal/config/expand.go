package config

import (
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// expandEnv replaces ${VAR} / $VAR references inside YAML scalar values only.
// Expanding after parsing keeps values such as passwords containing quotes,
// '#', ':' or '$' intact instead of letting them break the YAML syntax.
func expandEnv(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := unmarshalNode(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 { // empty document
		return raw, nil
	}
	changed := false
	var walk func(n *yaml.Node, isKey bool)
	walk = func(n *yaml.Node, isKey bool) {
		switch n.Kind {
		case yaml.ScalarNode:
			if !isKey && strings.Contains(n.Value, "$") {
				n.Value = os.ExpandEnv(n.Value)
				// Keep it a string; the encoder quotes it as needed and viper's
				// weak decoding still turns "30s" or "2" into durations/ints.
				n.Tag, n.Style = "!!str", 0
				changed = true
			}
		case yaml.MappingNode:
			for i, c := range n.Content {
				walk(c, i%2 == 0)
			}
		default:
			for _, c := range n.Content {
				walk(c, false)
			}
		}
	}
	walk(&doc, false)
	if !changed {
		return raw, nil
	}
	return yaml.Marshal(&doc)
}
