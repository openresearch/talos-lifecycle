package configrender

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"go.yaml.in/yaml/v3"
)

// Some fields Talos reads only when a service starts or a volume unlocks.
// Applied hot they have no effect yet, so verification would prove nothing:
// they keep the node's value and are reported as skipped.

// nil: the whole document
var rebootOnly = map[string][][]string{
	"v1alpha1": {
		{"cluster", "etcd"},
		{"machine", "env"},
		{"machine", "kernel"},
		{"machine", "systemDiskEncryption"},
	},
	"EnvironmentConfig":     nil,
	"KernelModuleConfig":    nil,
	"SecurityProfileConfig": {{"workloadIsolation"}},
	"VolumeConfig":          {{"encryption"}},
	"UserVolumeConfig":      {{"encryption"}},
	"RawVolumeConfig":       {{"encryption"}},
	"SwapVolumeConfig":      {{"encryption"}},
}

func keepRebootOnly(onto, cfg config.Provider) (config.Provider, []string, error) {
	oldDocs, _, err := documents(onto)
	if err != nil {
		return nil, nil, err
	}

	newDocs, order, err := documents(cfg)
	if err != nil {
		return nil, nil, err
	}

	var skipped []string

	for _, id := range unionKeys(oldDocs, newDocs) {
		paths, managed := rebootOnly[kindOf(oldDocs[id], newDocs[id])]
		if !managed {
			continue
		}

		if paths == nil {
			if !sameNode(oldDocs[id], newDocs[id]) {
				skipped = append(skipped, id)
				newDocs[id] = oldDocs[id]
			}

			continue
		}

		for _, p := range paths {
			if sameNode(lookup(oldDocs[id], p), lookup(newDocs[id], p)) {
				continue
			}

			skipped = append(skipped, id+": "+strings.Join(p, "."))

			if newDocs[id] == nil {
				newDocs[id] = oldDocs[id]

				continue
			}

			set(newDocs[id], p, lookup(oldDocs[id], p))
		}
	}

	if len(skipped) == 0 {
		return cfg, nil, nil
	}

	var b bytes.Buffer

	for _, id := range append(order, missingFrom(order, newDocs)...) {
		doc := newDocs[id]
		if doc == nil {
			continue
		}

		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("encode %s: %w", id, err)
		}

		b.WriteString("---\n")
		b.Write(out)
	}

	kept, err := configloader.NewFromBytes(b.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("reload config after keeping reboot-only fields: %w", err)
	}

	return kept, skipped, nil
}

func documents(cfg config.Provider) (map[string]*yaml.Node, []string, error) {
	b, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, nil, fmt.Errorf("encode config: %w", err)
	}

	docs := map[string]*yaml.Node{}

	var order []string

	dec := yaml.NewDecoder(bytes.NewReader(b))

	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, order, nil
			}

			return nil, nil, fmt.Errorf("decode config: %w", err)
		}

		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			continue
		}

		root := doc.Content[0]

		id := kindOf(root, nil)
		if name := lookup(root, []string{"name"}); name != nil {
			id += "/" + name.Value
		}

		docs[id] = root

		order = append(order, id)
	}
}

// the v1alpha1 document has a version and no kind
func kindOf(a, b *yaml.Node) string {
	for _, doc := range []*yaml.Node{a, b} {
		if kind := lookup(doc, []string{"kind"}); kind != nil {
			return kind.Value
		}

		if lookup(doc, []string{"version"}) != nil {
			return "v1alpha1"
		}
	}

	return ""
}

func lookup(n *yaml.Node, path []string) *yaml.Node {
	for _, key := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}

		var next *yaml.Node

		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]

				break
			}
		}

		n = next
	}

	return n
}

func set(doc *yaml.Node, path []string, value *yaml.Node) {
	parent := doc

	for _, key := range path[:len(path)-1] {
		child := lookup(parent, []string{key})
		if child == nil {
			if value == nil {
				return
			}

			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}

		parent = child
	}

	last := path[len(path)-1]

	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == last {
			if value == nil {
				parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
			} else {
				parent.Content[i+1] = value
			}

			return
		}
	}

	if value != nil {
		parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, value)
	}
}

func sameNode(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}

	x, errA := yaml.Marshal(a)
	y, errB := yaml.Marshal(b)

	return errA == nil && errB == nil && bytes.Equal(x, y)
}

func unionKeys(a, b map[string]*yaml.Node) []string {
	keys := make([]string, 0, len(a))

	for k := range a {
		keys = append(keys, k)
	}

	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	return keys
}

func missingFrom(order []string, docs map[string]*yaml.Node) []string {
	seen := map[string]bool{}
	for _, id := range order {
		seen[id] = true
	}

	var extra []string

	for id := range docs {
		if !seen[id] {
			extra = append(extra, id)
		}
	}

	sort.Strings(extra)

	return extra
}
