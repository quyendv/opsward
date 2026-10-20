package kubernetes

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/quyendv/opsward/internal/capability"
)

// callerWorkloads is the Workloads of a profile acting as its caller (docs/design/0014).
// The cluster refuses some people what it lets others do, so each refusal wraps
// capability.ErrForbidden: tools and the audit tell it from a failure. For a profile
// acting as itself a refusal is a misconfiguration, and is returned as it is.
type callerWorkloads struct{ w *workloads }

func refused(err error) error {
	if apierrors.IsForbidden(err) && !errors.Is(err, capability.ErrForbidden) {
		return fmt.Errorf("%w: %w", capability.ErrForbidden, err)
	}
	return err
}

func (c callerWorkloads) GetWorkload(ctx context.Context, ref capability.WorkloadRef) (*capability.Workload, error) {
	out, err := c.w.GetWorkload(ctx, ref)
	return out, refused(err)
}

func (c callerWorkloads) WorkloadLabels(ctx context.Context, ref capability.WorkloadRef) (map[string]string, error) {
	out, err := c.w.WorkloadLabels(ctx, ref)
	return out, refused(err)
}

func (c callerWorkloads) WorkloadMetadata(ctx context.Context, ref capability.WorkloadRef) (*capability.Metadata, error) {
	out, err := c.w.WorkloadMetadata(ctx, ref)
	return out, refused(err)
}

func (c callerWorkloads) WorkloadEvents(ctx context.Context, ref capability.WorkloadRef, limit int) ([]capability.Event, error) {
	out, err := c.w.WorkloadEvents(ctx, ref, limit)
	return out, refused(err)
}

func (c callerWorkloads) WorkloadLogs(ctx context.Context, ref capability.WorkloadRef, opt capability.LogOptions) (*capability.WorkloadLogs, error) {
	out, err := c.w.WorkloadLogs(ctx, ref, opt)
	return out, refused(err)
}

func (c callerWorkloads) PodNamePattern(ctx context.Context, ref capability.WorkloadRef) (string, error) {
	out, err := c.w.PodNamePattern(ctx, ref)
	return out, refused(err)
}

func (c callerWorkloads) ListNamespaces(ctx context.Context) ([]string, error) {
	out, err := c.w.ListNamespaces(ctx)
	return out, refused(err)
}

func (c callerWorkloads) ListWorkloads(ctx context.Context, namespace string) ([]capability.WorkloadSummary, error) {
	out, err := c.w.ListWorkloads(ctx, namespace)
	return out, refused(err)
}

func (c callerWorkloads) WorkloadRestarts(ctx context.Context, namespace string) ([]capability.WorkloadRestarts, error) {
	out, err := c.w.WorkloadRestarts(ctx, namespace)
	return out, refused(err)
}

func (c callerWorkloads) RestartWorkload(ctx context.Context, ref capability.WorkloadRef, dryRun bool) (*capability.WorkloadChange, error) {
	out, err := c.w.RestartWorkload(ctx, ref, dryRun)
	return out, refused(err)
}

func (c callerWorkloads) ScaleWorkload(ctx context.Context, ref capability.WorkloadRef, replicas int32, dryRun bool) (*capability.WorkloadChange, error) {
	out, err := c.w.ScaleWorkload(ctx, ref, replicas, dryRun)
	return out, refused(err)
}
