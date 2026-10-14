package configrender

import (
	"bytes"
	"errors"
	"io"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"go.yaml.in/yaml/v3"
)

// Dropping a field from the tree does not remove it from the node; a delete
// patch does, and Talos fails it once the target is gone. A delete-only file
// therefore counts as converged when its target is absent.

func isRemovalOnly(content []byte, patch configpatcher.Patch) bool {
	if ops, ok := patch.(jsonpatch.Patch); ok {
		if len(ops) == 0 {
			return false
		}

		for _, op := range ops {
			if op.Kind() != "remove" {
				return false
			}
		}

		return true
	}

	dec := yaml.NewDecoder(bytes.NewReader(content))
	docs := 0

	for {
		var doc yaml.Node

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil || len(doc.Content) == 0 {
			return false
		}

		if !deletesOnly(doc.Content[0], true) {
			return false
		}

		docs++
	}

	return docs > 0
}

func deletesOnly(n *yaml.Node, top bool) bool {
	if n.Kind != yaml.MappingNode {
		return false
	}

	if hasDelete(n) {
		return true
	}

	nested := 0

	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]

		switch val.Kind {
		case yaml.MappingNode:
			if !deletesOnly(val, false) {
				return false
			}

			nested++
		case yaml.SequenceNode:
			if len(val.Content) == 0 {
				return false
			}

			for _, item := range val.Content {
				if item.Kind != yaml.MappingNode || !hasDelete(item) {
					return false
				}
			}

			nested++
		default:
			if !top || (key != "version" && key != "apiVersion" && key != "kind" && key != "name") {
				return false
			}
		}
	}

	return nested > 0
}

func hasDelete(n *yaml.Node) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "$patch" && n.Content[i+1].Value == "delete" {
			return true
		}
	}

	return false
}
