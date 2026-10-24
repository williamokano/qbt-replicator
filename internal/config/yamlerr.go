package config

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// explainYAMLError makes YAML syntax errors point at the offending line.
// yaml.v3 often reports where the enclosing mapping started (e.g. "line 4")
// instead of the bad line, so we find the first line at which the document
// stops parsing and quote it, with a hint for common mistakes.
func explainYAMLError(raw []byte, err error) error {
	if err == nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	bad := -1
	for i := range lines {
		var n yaml.Node
		if yaml.Unmarshal([]byte(strings.Join(lines[:i+1], "\n")), &n) != nil {
			bad = i
			break
		}
	}
	if bad < 0 {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	// Drop yaml.v3's own (often wrong) "yaml: line N:" prefix.
	reason := lineRef.ReplaceAllString(err.Error(), "")
	msg := fmt.Sprintf("invalid YAML at line %d: %q: %s", bad+1, strings.TrimSpace(lines[bad]), reason)
	if hint := yamlHint(lines, bad); hint != "" {
		msg += "\nhint: " + hint
	}
	return fmt.Errorf("%s", msg)
}

func yamlHint(lines []string, bad int) string {
	cur := strings.TrimSpace(lines[bad])
	prev := ""
	for i := bad - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			prev = t
			break
		}
	}
	switch {
	case strings.HasPrefix(cur, "-") && (strings.HasSuffix(prev, "[]") || strings.HasSuffix(prev, "{}")):
		key := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(prev, "[]"), "{}"))
		return fmt.Sprintf("%q declares an empty list; remove the \"[]\" so it reads %q when list items follow", prev, key)
	case strings.Contains(lines[bad], "\t"):
		return "YAML does not allow tabs for indentation; use spaces"
	}
	return ""
}

var lineRef = regexp.MustCompile(`^yaml: (line \d+: )?`)

// unmarshalNode parses raw into a node with line-accurate errors.
func unmarshalNode(raw []byte, n *yaml.Node) error {
	if err := yaml.Unmarshal(raw, n); err != nil {
		return explainYAMLError(bytes.TrimRight(raw, "\n"), err)
	}
	return nil
}
