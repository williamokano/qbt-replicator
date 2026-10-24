package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Starter is written when no configuration file exists. It is valid as-is
// ("setup mode": no instances, nothing happens) and meant to be edited from
// the web UI's config editor or by hand.
const Starter = `# qbt-replicator configuration (generated starter file).
# Edit it from the web UI (Config page) or by hand; see config.example.yaml in
# the repository for every option. ${VAR} in values is read from the environment.

log:
  level: info

server:
  listen: ":8080"

daemon:
  interval: 30s
  dry_run: false

# Add at least two qBittorrent WebUIs that share the same storage paths, and at
# least one rule: uncomment the two blocks below (remove the leading "# ") and edit.
#
# instances:
#   - name: qbt-1
#     url: http://gluetun-1:8080
#     username: admin
#     password: ${QBT1_PASSWORD}
#   - name: qbt-2
#     url: http://gluetun-2:8080
#     username: admin
#     password: ${QBT2_PASSWORD}
#
# # Replicate torrents completed on qbt-1 to every other instance.
# rules:
#   - name: main
#     sources: [qbt-1]
#     targets: ["*"]
`

// WriteStarter creates path (and its directory) with the starter config.
// It never overwrites an existing file.
func WriteStarter(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating starter config: %w", err)
	}
	if _, err := f.WriteString(Starter); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// SaveFile atomically replaces path with raw, keeping the previous version as path+".bak".
func SaveFile(path string, raw []byte) error {
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("writing backup: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing config: %w", err)
	}
	return nil
}

// Mask is shown instead of literal secrets in the config editor.
const Mask = "********"

func isSecretKey(k string) bool {
	return k == "password" || k == "api_key" || k == "bot_token" || k == "token"
}

// secretPaths walks a YAML document and calls fn for every secret value,
// identified by a stable path (sequence items with a "name" use it).
func secretPaths(doc *yaml.Node, fn func(path string, n *yaml.Node)) {
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				p := path + "." + k.Value
				if isSecretKey(k.Value) && v.Kind == yaml.ScalarNode {
					fn(p, v)
					continue
				}
				walk(v, p)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				id := strconv.Itoa(i)
				if c.Kind == yaml.MappingNode {
					for j := 0; j+1 < len(c.Content); j += 2 {
						if c.Content[j].Value == "name" {
							id = "name=" + c.Content[j+1].Value
						}
					}
				}
				walk(c, path+"["+id+"]")
			}
		}
	}
	walk(doc, "")
}

func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MaskSecrets replaces literal password values with Mask. References such as
// ${QBT1_PASSWORD} are left visible since they contain no secret.
func MaskSecrets(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := unmarshalNode(raw, &doc); err != nil {
		return nil, err
	}
	changed := false
	secretPaths(&doc, func(_ string, n *yaml.Node) {
		if n.Value != "" && !strings.Contains(n.Value, "$") {
			n.Value, n.Style, changed = Mask, 0, true
		}
	})
	if !changed {
		return raw, nil
	}
	return encode(&doc)
}

// ErrUnknownMasked is returned when a masked secret has no previous value to restore.
var ErrUnknownMasked = errors.New("a password is still " + Mask + " but has no saved value to keep; type the real password")

// UnmaskSecrets restores every Mask value in edited from the same path in previous.
func UnmaskSecrets(edited, previous []byte) ([]byte, error) {
	var doc yaml.Node
	if err := unmarshalNode(edited, &doc); err != nil {
		return nil, err
	}
	if !bytes.Contains(edited, []byte(Mask)) {
		return edited, nil
	}
	old := map[string]string{}
	var prev yaml.Node
	if err := yaml.Unmarshal(previous, &prev); err == nil {
		secretPaths(&prev, func(p string, n *yaml.Node) { old[p] = n.Value })
	}
	var missing error
	secretPaths(&doc, func(p string, n *yaml.Node) {
		if n.Value != Mask {
			return
		}
		v, ok := old[p]
		if !ok || v == Mask {
			missing = fmt.Errorf("%s: %w", strings.TrimPrefix(p, "."), ErrUnknownMasked)
			return
		}
		n.Value, n.Style = v, 0
	})
	if missing != nil {
		return nil, missing
	}
	return encode(&doc)
}

// EnvOverrides lists QBTR_* environment variables that override file values.
func EnvOverrides() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, EnvPrefix+"_") && k != EnvPrefix+"_CONFIG" {
			out = append(out, k)
		}
	}
	return out
}
