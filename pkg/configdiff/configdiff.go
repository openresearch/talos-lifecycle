// Package configdiff diffs machine configs without showing secrets. Redaction
// makes every secret the same placeholder, so changed secrets are reported
// separately, as hashes.
package configdiff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config"
	machineryconfigdiff "github.com/siderolabs/talos/pkg/machinery/config/configdiff"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
)

const Placeholder = "***"

const hashPrefixLen = 12

type SecretChange struct {
	Field string
	// empty OldHash: added; empty NewHash: removed
	OldHash, NewHash string
}

func (c SecretChange) String() string {
	from, to := c.OldHash, c.NewHash
	if from == "" {
		from = "(absent)"
	}

	if to == "" {
		to = "(absent)"
	}

	return fmt.Sprintf("%s: %s -> %s", c.Field, from, to)
}

type Report struct {
	Structural    string
	SecretChanges []SecretChange
}

func (r Report) String() string {
	var b strings.Builder

	if r.Structural == "" {
		b.WriteString("(no structural changes)\n")
	} else {
		b.WriteString(r.Structural)
	}

	if len(r.SecretChanges) > 0 {
		b.WriteString("\n# secrets changed (values hashed, never shown):\n")

		for _, c := range r.SecretChanges {
			fmt.Fprintf(&b, "#   %s\n", c)
		}
	}

	return b.String()
}

func Diff(oldCfg, newCfg config.Provider) (Report, error) {
	structural, err := machineryconfigdiff.DiffConfigs(
		oldCfg.RedactSecrets(Placeholder), newCfg.RedactSecrets(Placeholder))
	if err != nil {
		return Report{}, fmt.Errorf("structural diff: %w", err)
	}

	oldSecrets, err := secretHashes(oldCfg)
	if err != nil {
		return Report{}, fmt.Errorf("hash old secrets: %w", err)
	}

	newSecrets, err := secretHashes(newCfg)
	if err != nil {
		return Report{}, fmt.Errorf("hash new secrets: %w", err)
	}

	var changes []SecretChange

	for field, oldHash := range oldSecrets {
		if oldHash != newSecrets[field] {
			changes = append(changes, SecretChange{Field: field, OldHash: oldHash, NewHash: newSecrets[field]})
		}
	}

	for field, newHash := range newSecrets {
		if _, ok := oldSecrets[field]; !ok {
			changes = append(changes, SecretChange{Field: field, NewHash: newHash})
		}
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })

	return Report{Structural: structural, SecretChanges: changes}, nil
}

func secretHashes(cfg config.Provider) (map[string]string, error) {
	plain, err := encodeLines(cfg)
	if err != nil {
		return nil, err
	}

	redacted, err := encodeLines(cfg.RedactSecrets(Placeholder))
	if err != nil {
		return nil, err
	}

	if len(plain) != len(redacted) {
		return nil, fmt.Errorf("redaction changed the line count (%d vs %d); cannot align secrets", len(plain), len(redacted))
	}

	hashes := map[string]string{}
	seen := map[string]int{}

	for i, line := range redacted {
		if !strings.Contains(line, Placeholder) {
			continue
		}

		key, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		field := strings.TrimSpace(strings.TrimPrefix(key, "- "))

		if seen[field]++; seen[field] > 1 {
			field = fmt.Sprintf("%s#%d", field, seen[field])
		}

		_, value, found := strings.Cut(plain[i], ":")
		if !found {
			value = plain[i]
		}

		sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
		hashes[field] = "sha256:" + hex.EncodeToString(sum[:])[:hashPrefixLen]
	}

	return hashes, nil
}

func encodeLines(cfg config.Encoder) ([]string, error) {
	b, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	return strings.Split(string(b), "\n"), nil
}
