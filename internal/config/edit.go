package config

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Errors of the YAML edit helpers.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
)

// The edit helpers change one part of a YAML config through its node tree,
// so comments and formatting elsewhere in the file are kept. The admin
// API's partial edits use them with Store.Update.

// SetItem replaces the item of the top-level sequence seqKey whose idField
// equals id. With create set, a missing item is appended instead of
// reported as ErrNotFound; with create and !replace, an existing item is
// ErrAlreadyExists.
func SetItem(data []byte, seqKey, idField, id string, item any, create, replace bool) ([]byte, error) {
	return editRoot(data, func(root *yaml.Node) error {
		var n yaml.Node
		if err := n.Encode(item); err != nil {
			return err
		}
		seq := ensureSeq(root, seqKey)
		if i := findItem(seq, idField, id); i >= 0 {
			if !replace {
				return fmt.Errorf("%s %q: %w", seqKey, id, ErrAlreadyExists)
			}
			keepComments(seq.Content[i], &n)
			seq.Content[i] = &n
			return nil
		}
		if !create {
			return fmt.Errorf("%s %q: %w", seqKey, id, ErrNotFound)
		}
		seq.Content = append(seq.Content, &n)
		return nil
	})
}

// DeleteItem removes the item of seqKey whose idField equals id.
func DeleteItem(data []byte, seqKey, idField, id string) ([]byte, error) {
	return editRoot(data, func(root *yaml.Node) error {
		seq := mapValue(root, seqKey)
		i := -1
		if seq != nil && seq.Kind == yaml.SequenceNode {
			i = findItem(seq, idField, id)
		}
		if i < 0 {
			return fmt.Errorf("%s %q: %w", seqKey, id, ErrNotFound)
		}
		seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
		return nil
	})
}

// SetField sets field of every item of seqKey whose idField is a key of
// values. Every id must exist.
func SetField(data []byte, seqKey, idField, field string, values map[string]any) ([]byte, error) {
	return editRoot(data, func(root *yaml.Node) error {
		seq := mapValue(root, seqKey)
		for id, v := range values {
			i := -1
			if seq != nil && seq.Kind == yaml.SequenceNode {
				i = findItem(seq, idField, id)
			}
			if i < 0 {
				return fmt.Errorf("%s %q: %w", seqKey, id, ErrNotFound)
			}
			var vn yaml.Node
			if err := vn.Encode(v); err != nil {
				return err
			}
			item := seq.Content[i]
			if cur := mapValue(item, field); cur != nil {
				cur.Kind, cur.Tag, cur.Value, cur.Content = vn.Kind, vn.Tag, vn.Value, vn.Content
			} else {
				item.Content = append(item.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, &vn)
			}
		}
		return nil
	})
}

func editRoot(data []byte, fn func(root *yaml.Node) error) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config root is not a mapping")
	}
	if err := fn(doc.Content[0]); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func ensureSeq(root *yaml.Node, key string) *yaml.Node {
	if v := mapValue(root, key); v != nil {
		if v.Kind != yaml.SequenceNode {
			*v = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		}
		return v
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func findItem(seq *yaml.Node, idField, id string) int {
	for i, item := range seq.Content {
		if v := mapValue(item, idField); v != nil && v.Value == id {
			return i
		}
	}
	return -1
}

func keepComments(from, to *yaml.Node) {
	to.HeadComment, to.LineComment, to.FootComment = from.HeadComment, from.LineComment, from.FootComment
}
