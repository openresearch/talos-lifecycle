package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

// A gate is something the cluster or an operator can resolve: it shows as Waiting
// with a reason. An error is something the controller could not evaluate.

type gateResult struct {
	reason string
}

func (g gateResult) passed() bool { return g.reason == "" }

func blocked(format string, args ...any) gateResult {
	return gateResult{reason: fmt.Sprintf(format, args...)}
}

func emit(rec record.EventRecorder, obj runtime.Object, eventType, reason, message string) {
	ctrl.Log.WithName("event").Info(message, "reason", reason, "type", eventType)

	if rec != nil {
		rec.Event(obj, eventType, reason, message)
	}
}

func specDuration(v *int64, unit, fallback time.Duration) time.Duration {
	if v == nil {
		return fallback
	}

	return time.Duration(*v) * unit
}

func windowGate(w *upgradev1alpha1.UpgradeWindow, now time.Time) (gateResult, time.Duration) {
	if w == nil {
		return gateResult{}, 0
	}

	wait, err := untilWindow(now, w)

	switch {
	case err != nil:
		return blocked("window %s-%s is invalid (%v); nothing starts until it is fixed", w.Start, w.End, err), idleInterval
	case wait > 0:
		return blocked("outside the window %s-%s, opens at %s", w.Start, w.End, now.Add(wait).Format("15:04")), wait
	}

	return gateResult{}, 0
}

func untilWindow(now time.Time, w *upgradev1alpha1.UpgradeWindow) (time.Duration, error) {
	start, err := time.Parse("15:04", w.Start)
	if err != nil {
		return 0, err
	}

	end, err := time.Parse("15:04", w.End)
	if err != nil {
		return 0, err
	}

	nowMinute := minuteOfDay(now)
	startMinute := minuteOfDay(start)
	endMinute := minuteOfDay(end)

	var inside bool
	if startMinute <= endMinute {
		inside = nowMinute >= startMinute && nowMinute < endMinute
	} else {
		// wraps past midnight, e.g. 20:00 to 05:00
		inside = nowMinute >= startMinute || nowMinute < endMinute
	}

	if inside {
		return 0, nil
	}

	wait := startMinute - nowMinute
	if wait < 0 {
		wait += 24 * 60
	}

	return time.Duration(wait) * time.Minute, nil
}

func minuteOfDay(t time.Time) int {
	return t.Hour()*60 + t.Minute()
}

func uncached(apiReader client.Reader, c client.Client) client.Reader {
	if apiReader == nil {
		return c
	}

	return apiReader
}

// persistStatus writes from a copy: Update would replace status.nodes and
// detach callers' pointers into it.
func persistStatus(ctx context.Context, c client.Client, obj client.Object) error {
	written, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("copy %T for a status write", obj)
	}

	if err := c.Status().Update(ctx, written); err != nil {
		return err
	}

	obj.SetResourceVersion(written.GetResourceVersion())

	return nil
}

// removeAnnotation patches, so it cannot overwrite a spec changed meanwhile.
func removeAnnotation(ctx context.Context, c client.Client, obj client.Object, key string) error {
	patched, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("copy %T to patch an annotation", obj)
	}

	annotations := patched.GetAnnotations()
	delete(annotations, key)
	patched.SetAnnotations(annotations)

	if err := c.Patch(ctx, patched, client.MergeFrom(obj)); err != nil {
		return fmt.Errorf("remove annotation %s: %w", key, err)
	}

	obj.SetAnnotations(patched.GetAnnotations())
	obj.SetResourceVersion(patched.GetResourceVersion())

	return nil
}
