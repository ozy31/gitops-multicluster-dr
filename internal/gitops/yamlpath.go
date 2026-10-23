package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// GetString returns the scalar at the dotted key path inside a YAML document.
func GetString(doc []byte, keyPath string) (string, error) {
	n, err := lookup(doc, keyPath)
	if err != nil {
		return "", err
	}
	return n.Value, nil
}

// SetString replaces the scalar at keyPath with value and returns the new
// document. Only the bytes of that scalar are touched, so comments, key
// order and formatting of the rest of the file are preserved exactly.
func SetString(doc []byte, keyPath, value string) ([]byte, error) {
	n, err := lookup(doc, keyPath)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(value, "\n:#'\"") {
		return nil, fmt.Errorf("refusing to write value %q: unsupported characters", value)
	}

	lines := bytes.SplitAfter(doc, []byte("\n"))
	if n.Line < 1 || n.Line > len(lines) {
		return nil, fmt.Errorf("key %q: line %d out of range", keyPath, n.Line)
	}
	line := lines[n.Line-1]
	start := n.Column - 1

	var oldRaw, newRaw string
	switch n.Style {
	case 0:
		oldRaw, newRaw = n.Value, value
	case yaml.DoubleQuotedStyle:
		oldRaw, newRaw = `"`+n.Value+`"`, `"`+value+`"`
	case yaml.SingleQuotedStyle:
		oldRaw, newRaw = `'`+n.Value+`'`, `'`+value+`'`
	default:
		return nil, fmt.Errorf("key %q: unsupported scalar style", keyPath)
	}
	end := start + len(oldRaw)
	if start < 0 || end > len(line) || string(line[start:end]) != oldRaw {
		return nil, fmt.Errorf("key %q: could not locate scalar in source", keyPath)
	}

	var out bytes.Buffer
	out.Grow(len(doc) + len(newRaw))
	for i, l := range lines {
		if i == n.Line-1 {
			out.Write(l[:start])
			out.WriteString(newRaw)
			out.Write(l[end:])
			continue
		}
		out.Write(l)
	}
	return out.Bytes(), nil
}

func lookup(doc []byte, keyPath string) (*yaml.Node, error) {
	if keyPath == "" {
		return nil, errors.New("empty key path")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("empty yaml document")
	}
	cur := root.Content[0]
	for _, key := range strings.Split(keyPath, ".") {
		if cur.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("key %q: %q is not a mapping", keyPath, key)
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == key {
				next = cur.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("key %q not found", keyPath)
		}
		cur = next
	}
	if cur.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("key %q is not a scalar", keyPath)
	}
	return cur, nil
}
