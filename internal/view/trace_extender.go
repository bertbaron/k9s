// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"context"
	"log/slog"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/dao"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/tcell/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// TraceExtender adds Crossplane trace action to a given viewer.
type TraceExtender struct {
	ResourceViewer
}

// NewTraceExtender returns a new extender.
func NewTraceExtender(r ResourceViewer) ResourceViewer {
	v := &TraceExtender{ResourceViewer: r}
	v.AddBindKeysFn(v.bindKeys)
	return v
}

func (v *TraceExtender) bindKeys(aa *ui.KeyActions) {
	aa.Add(tcell.KeyCtrlT, ui.NewKeyAction("Trace", v.traceCmd, true))
}

func (v *TraceExtender) traceCmd(evt *tcell.EventKey) *tcell.EventKey {
	path := v.GetTable().GetSelectedItem()
	if path == "" {
		return evt
	}

	if !v.isCrossplaneResource(path) {
		v.App().Flash().Warn("Resource does not appear to be a Crossplane resource")
		return nil
	}

	if err := v.App().inject(NewTrace(v.GVR(), path), false); err != nil {
		v.App().Flash().Err(err)
	}

	return nil
}

func (v *TraceExtender) isCrossplaneResource(path string) bool {
	ctx := context.WithValue(context.Background(), internal.KeyFactory, v.App().factory)

	res, err := dao.AccessorFor(v.App().factory, v.GVR())
	if err != nil {
		slog.Debug("Trace extender: accessor error", slogs.Error, err)
		return false
	}

	o, err := res.Get(ctx, path)
	if err != nil {
		return false
	}

	u, ok := asUnstructured(o)
	if !ok {
		return false
	}

	// Check for spec.resourceRef (V1 Claim)
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "resourceRef"); found {
		return true
	}
	// Check for spec.resourceRefs (V1 Composite)
	if refs, found, _ := unstructured.NestedSlice(u.Object, "spec", "resourceRefs"); found && len(refs) > 0 {
		return true
	}
	// Check for spec.crossplane.resourceRefs (V2 Composite/Claim)
	if refs, found, _ := unstructured.NestedSlice(u.Object, "spec", "crossplane", "resourceRefs"); found && len(refs) > 0 {
		return true
	}
	// Check for crossplane.io/composite label (Managed resource)
	labels := u.GetLabels()
	if _, ok := labels["crossplane.io/composite"]; ok {
		return true
	}

	return false
}

func asUnstructured(o runtime.Object) (*unstructured.Unstructured, bool) {
	u, ok := o.(*unstructured.Unstructured)
	return u, ok
}
