package confignode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config"
)

// FakeNode models the config slots and the try timer. Outside _test files
// because the controller tests use it too.
type FakeNode struct {
	mu sync.Mutex

	active, persistent []byte
	persisted          bool
	tryDeadline        time.Time
	tryRollback        []byte
	now                func() time.Time

	Unreachable bool
	RejectApply bool
	Calls       []string
}

func NewFakeNode(cfg []byte) *FakeNode {
	return &FakeNode{active: clone(cfg), persistent: clone(cfg), now: time.Now}
}

func (f *FakeNode) SetClock(start time.Time) func(time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	current := start
	f.now = func() time.Time { return current }

	return func(d time.Duration) {
		f.mu.Lock()
		defer f.mu.Unlock()

		current = current.Add(d)
	}
}

// expire runs at every call, which is how the revert happens unasked.
func (f *FakeNode) expire() {
	if f.tryDeadline.IsZero() || f.now().Before(f.tryDeadline) {
		return
	}

	f.active, f.tryRollback, f.tryDeadline = f.tryRollback, nil, time.Time{}
}

func (f *FakeNode) TryPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire()

	return !f.tryDeadline.IsZero()
}

func (f *FakeNode) SlotBytes(slot Slot) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire()

	if slot == SlotActive {
		return clone(f.active)
	}

	return clone(f.persistent)
}

func (f *FakeNode) Reboot() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.active, f.tryRollback, f.tryDeadline = clone(f.persistent), nil, time.Time{}
	f.persisted = false
}

func (f *FakeNode) enter(name string) error {
	f.expire()
	f.Calls = append(f.Calls, name)

	if f.Unreachable {
		return errors.New("node unreachable")
	}

	return nil
}

func (f *FakeNode) ReadSlot(_ context.Context, slot Slot) (config.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.enter("ReadSlot:" + slot); err != nil {
		return nil, err
	}

	if slot == SlotPersistent {
		if !f.persisted {
			return nil, fmt.Errorf("read %s config: %w", slot, ErrNotPersisted)
		}

		return Load(f.persistent)
	}

	return Load(f.active)
}

func (f *FakeNode) ApplyTry(_ context.Context, cfg []byte, timeout time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.enter("ApplyTry"); err != nil {
		return "", err
	}

	switch {
	case f.RejectApply:
		return "", fmt.Errorf("%w: rejected by fake node", ErrRejected)
	case timeout <= 0:
		return "", errors.New("try timeout must be positive")
	}

	f.tryRollback, f.tryDeadline, f.active = f.active, f.now().Add(timeout), clone(cfg)

	return fmt.Sprintf("applied in try mode, reverts in %s", timeout), nil
}

func (f *FakeNode) ApplyCommit(_ context.Context, cfg []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.enter("ApplyCommit"); err != nil {
		return "", err
	}

	f.tryRollback, f.tryDeadline = nil, time.Time{}

	if f.RejectApply {
		return "", fmt.Errorf("%w: rejected by fake node", ErrRejected)
	}

	f.active, f.persistent, f.persisted = clone(cfg), clone(cfg), true

	return "applied without a reboot", nil
}

func (f *FakeNode) Close() error { return nil }

type FakeDialer struct {
	Nodes map[string]*FakeNode
}

func (d *FakeDialer) Dial(_ context.Context, address string) (Client, error) {
	n, ok := d.Nodes[address]
	if !ok {
		return nil, fmt.Errorf("no fake node at %s", address)
	}

	return n, nil
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }
