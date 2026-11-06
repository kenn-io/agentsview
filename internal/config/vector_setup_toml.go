package config

import (
	"bytes"
	"errors"
	"os"
	"slices"

	"github.com/BurntSushi/toml"
	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	"github.com/creachadair/tomledit/transform"
)

// writeVectorSetup retains the user's comments and key order while applying the
// validated vector settings to section, dotted-key, and inline-table syntax.
func (c *Config) writeVectorSetup(settings map[string]any) error {
	original, err := os.ReadFile(c.configPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	doc, err := tomledit.Parse(bytes.NewReader(original))
	if err != nil {
		return err
	}
	encoded, err := toml.Marshal(settings)
	if err != nil {
		return err
	}
	replacement, err := tomledit.Parse(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	var removed []*tomledit.Entry
	doc.Scan(func(key parser.Key, entry *tomledit.Entry) bool {
		if key[0] == "vector" && entry.IsMapping() && replacement.First(key...) == nil {
			removed = append(removed, entry)
		}
		return true
	})
	for _, entry := range removed {
		comments := slices.Clone(entry.Block)
		if entry.Value.Trailer != "" {
			comments = append(comments, entry.Value.Trailer)
		}
		entry.Remove()
		if len(comments) > 0 {
			entry.Items = append(entry.Items, comments)
		}
	}
	replacement.Scan(func(key parser.Key, entry *tomledit.Entry) bool {
		if key[0] != "vector" || !entry.IsMapping() {
			return true
		}
		if current := doc.First(key...); current != nil {
			current.Value.X = entry.Value.X
			return true
		}
		field := *entry.KeyValue
		for n := len(key) - 1; n > 0; n-- {
			if parent := doc.First(key[:n]...); parent != nil && parent.IsMapping() {
				if inline, ok := parent.Value.X.(parser.Inline); ok {
					field.Name = slices.Clone(key[n:])
					inline = append(inline, &field)
					parent.Value.X = inline
					return true
				}
			}
			if parent := transform.FindTable(doc, key[:n]...); parent != nil {
				field.Name = slices.Clone(key[n:])
				transform.InsertMapping(parent.Section, &field, false)
				return true
			}
		}
		doc.Sections = append(doc.Sections, &tomledit.Section{
			Heading: &parser.Heading{Name: slices.Clone(key[:len(key)-1])},
			Items:   []parser.Item{&field},
		})
		return true
	})
	var out bytes.Buffer
	if err := tomledit.Format(&out, doc); err != nil {
		return err
	}
	return os.WriteFile(c.configPath(), out.Bytes(), 0o600)
}
