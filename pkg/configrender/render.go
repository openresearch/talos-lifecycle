// Package configrender applies a tree of Talos patches onto a node's own config:
//
//	base/[<role>/]*.yaml   every node (base/<role>/ replaces base/ if present)
//	roles/<role>.yaml      every node of a role
//	nodes/<name>.yaml      one node
//
// Files are strategic merge or JSON6902 patches, as `talosctl patch machineconfig`
// takes them, applied in that order. Fields the tree does not set keep the node's value.
package configrender

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
)

const (
	BaseDir  = "base"
	RolesDir = "roles"
	NodesDir = "nodes"
)

type Node struct {
	Name string
	Role string
}

type Result struct {
	Config config.Provider
	// the exact bytes to try and commit
	Bytes    []byte
	Warnings []string
	Sources  []string
	Skipped  []string
}

func Render(fsys fs.FS, node Node, onto config.Provider) (*Result, error) {
	if node.Name == "" {
		return nil, errors.New("node name is required")
	}

	if onto == nil {
		return nil, errors.New("no config to render onto")
	}

	files, err := treeFiles(fsys, node)
	if err != nil {
		return nil, err
	}

	// an empty tree would report every node in sync
	if len(files) == 0 {
		return nil, fmt.Errorf("no config files for node %s in %s/, %s/ or %s/", node.Name, BaseDir, RolesDir, NodesDir)
	}

	cfg, warnings, err := applyFiles(onto.Clone(), files)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node.Name, err)
	}

	if err := checkIdempotent(cfg, files); err != nil {
		return nil, fmt.Errorf("node %s: %w", node.Name, err)
	}

	// after the idempotency check, which sets reboot-only fields again
	cfg, skipped, err := keepRebootOnly(onto, cfg)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node.Name, err)
	}

	// WithLocal skips the Kubernetes image check: it would judge against this build's
	// Talos version, not the node's. The node runs it on apply.
	validationWarnings, err := cfg.ValidateAsClient(metal{}, validation.WithLocal())
	if err != nil {
		return nil, fmt.Errorf("validate config for node %s: %w", node.Name, err)
	}

	encoded, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, fmt.Errorf("encode config for node %s: %w", node.Name, err)
	}

	sources := make([]string, 0, len(files))
	for _, f := range files {
		sources = append(sources, f.name)
	}

	return &Result{
		Config:   cfg,
		Bytes:    encoded,
		Warnings: append(warnings, validationWarnings...),
		Sources:  sources,
		Skipped:  skipped,
	}, nil
}

type metal struct{}

func (metal) String() string        { return "metal" }
func (metal) RequiresInstall() bool { return true }
func (metal) InContainer() bool     { return false }

type patchFile struct {
	name    string
	content []byte
	patch   configpatcher.Patch
}

func treeFiles(fsys fs.FS, node Node) ([]patchFile, error) {
	names, err := listYAML(fsys, path.Join(BaseDir, node.Role))
	if err == nil && len(names) == 0 && node.Role != "" {
		names, err = listYAML(fsys, BaseDir)
	}

	if err != nil {
		return nil, err
	}

	optional := []string{path.Join(NodesDir, node.Name)}
	if node.Role != "" {
		optional = append([]string{path.Join(RolesDir, node.Role)}, optional...)
	}

	for _, name := range optional {
		for _, ext := range []string{".yaml", ".yml"} {
			_, err := fs.Stat(fsys, name+ext)
			if err == nil {
				names = append(names, name+ext)

				break
			}

			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("stat %s: %w", name+ext, err)
			}
		}
	}

	files := make([]patchFile, 0, len(names))

	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		patch, err := configpatcher.LoadPatch(content)
		if err != nil {
			return nil, fmt.Errorf("parse patch %s: %w", name, err)
		}

		files = append(files, patchFile{name: name, content: content, patch: patch})
	}

	return files, nil
}

func applyFiles(cfg config.Provider, files []patchFile) (config.Provider, []string, error) {
	var warnings []string

	for _, f := range files {
		out, err := configpatcher.Apply(configpatcher.WithConfig(cfg), []configpatcher.Patch{f.patch})
		if err != nil {
			if isRemovalOnly(f.content, f.patch) {
				warnings = append(warnings, fmt.Sprintf("%s: removal already applied (%v)", f.name, err))

				continue
			}

			return nil, nil, fmt.Errorf("apply %s: %w", f.name, err)
		}

		if cfg, err = out.Config(); err != nil {
			return nil, nil, fmt.Errorf("materialize after %s: %w", f.name, err)
		}
	}

	return cfg, warnings, nil
}

// checkIdempotent: strategic merge appends to lists, so lists belong in JSON6902
// add files, or every rollout would grow them.
func checkIdempotent(cfg config.Provider, files []patchFile) error {
	again, _, err := applyFiles(cfg.Clone(), files)
	if err != nil {
		return fmt.Errorf("re-apply: %w", err)
	}

	first, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	second, err := again.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return fmt.Errorf("encode re-applied config: %w", err)
	}

	if !bytes.Equal(first, second) {
		return errors.New("source tree is not idempotent: applying it twice changes the config, " +
			"usually a strategic-merge patch on a list field; use a JSON6902 add for lists")
	}

	return nil
}

func listYAML(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	var files []string

	for _, e := range entries {
		if ext := path.Ext(e.Name()); !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, path.Join(dir, e.Name()))
		}
	}

	sort.Strings(files)

	return files, nil
}
