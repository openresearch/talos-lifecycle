// Package confignode reads a node's config slots and applies config with TRY or NO_REBOOT.
package confignode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

type Slot = resource.ID

// TRY writes only the active slot.
const (
	SlotActive     Slot = configres.ActiveID
	SlotPersistent Slot = configres.PersistentID
)

// ErrRejected: the node refused the config. A rejected commit still cancels the
// try timer, so the tried config runs, unpersisted, until the next reboot.
var ErrRejected = errors.New("node rejected the config")

// ErrNotPersisted: Talos creates the persistent slot only on a persisting apply.
var ErrNotPersisted = errors.New("nothing persisted since boot")

// ReadBooted returns the persistent slot, or the active one if nothing was
// persisted since boot. During a try it returns the tried config.
func ReadBooted(ctx context.Context, c Client) (config.Provider, error) {
	cfg, err := c.ReadSlot(ctx, SlotPersistent)
	if errors.Is(err, ErrNotPersisted) {
		return c.ReadSlot(ctx, SlotActive)
	}

	return cfg, err
}

type Client interface {
	ReadSlot(ctx context.Context, slot Slot) (config.Provider, error)
	ApplyTry(ctx context.Context, cfg []byte, timeout time.Duration) (string, error)
	// cancels the try timer even when the config is rejected
	ApplyCommit(ctx context.Context, cfg []byte) (string, error)
	Close() error
}

type Dialer interface {
	Dial(ctx context.Context, address string) (Client, error)
}

// needs os:admin: ApplyConfiguration and MachineConfig require it
const DefaultTalosConfigPath = "/var/run/secrets/talos.dev/config"

type TalosDialer struct {
	ConfigPath string
}

func (d TalosDialer) Dial(ctx context.Context, address string) (Client, error) {
	if address == "" {
		return nil, errors.New("node address is required")
	}

	path := d.ConfigPath
	if path == "" {
		path = DefaultTalosConfigPath
	}

	cfg, err := clientconfig.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load talosconfig %q: %w", path, err)
	}

	c, err := client.New(client.WithNode(ctx, address), client.WithConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("create talos client for %s: %w", address, err)
	}

	return &talosClient{c: c, address: address}, nil
}

type talosClient struct {
	c       *client.Client
	address string
}

func (t *talosClient) ReadSlot(ctx context.Context, slot Slot) (config.Provider, error) {
	md := resource.NewMetadata(configres.NamespaceName, configres.MachineConfigType, slot, resource.VersionUndefined)

	res, err := safe.StateGet[*configres.MachineConfig](client.WithNode(ctx, t.address), t.c.COSI, md)
	if err != nil {
		if slot == SlotPersistent && state.IsNotFoundError(err) {
			return nil, fmt.Errorf("read %s config: %w", slot, ErrNotPersisted)
		}

		return nil, fmt.Errorf("read %s config: %w", slot, err)
	}

	return res.Provider(), nil
}

func (t *talosClient) ApplyTry(ctx context.Context, cfg []byte, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return "", errors.New("try timeout must be positive")
	}

	return t.apply(ctx, &machine.ApplyConfigurationRequest{
		Data:           cfg,
		Mode:           machine.ApplyConfigurationRequest_TRY,
		TryModeTimeout: durationpb.New(timeout),
	})
}

func (t *talosClient) ApplyCommit(ctx context.Context, cfg []byte) (string, error) {
	return t.apply(ctx, &machine.ApplyConfigurationRequest{
		Data: cfg,
		Mode: machine.ApplyConfigurationRequest_NO_REBOOT,
	})
}

// never DryRun: a dry run cancels a pending try timer
func (t *talosClient) apply(ctx context.Context, req *machine.ApplyConfigurationRequest) (string, error) {
	resp, err := t.c.ApplyConfiguration(client.WithNode(ctx, t.address), req)
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.InvalidArgument {
			return "", fmt.Errorf("%w: %s", ErrRejected, s.Message())
		}

		return "", fmt.Errorf("apply configuration: %w", err)
	}

	msgs := resp.GetMessages()
	if len(msgs) != 1 {
		return "", fmt.Errorf("expected a response from exactly one node, got %d", len(msgs))
	}

	return msgs[0].GetModeDetails(), nil
}

func (t *talosClient) Close() error { return t.c.Close() }

func Hash(p config.Provider) (string, error) {
	b, err := Encode(p)
	if err != nil {
		return "", err
	}

	return HashBytes(b), nil
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func Encode(p config.Provider) ([]byte, error) {
	b, err := p.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	return b, nil
}

func Load(b []byte) (config.Provider, error) {
	p, err := configloader.NewFromBytes(b)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return p, nil
}
