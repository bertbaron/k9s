// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package model

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/dao"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/xray"
)

// Trace represents a Crossplane resource tree model.
type Trace struct {
	gvr         *client.GVR
	path        string
	initialPath string
	root        *xray.TreeNode
	listeners   []TreeListener
	inUpdate    int32
	refreshRate time.Duration
	query       string
}

// NewTrace returns a new trace model.
func NewTrace(gvr *client.GVR, path string) *Trace {
	return &Trace{
		gvr:         gvr,
		path:        path,
		refreshRate: 2 * time.Second,
	}
}

// ClearFilter clears out active filter.
func (t *Trace) ClearFilter() {
	t.query = ""
}

// SetFilter sets the current filter.
func (t *Trace) SetFilter(q string) {
	t.query = q
}

// AddListener adds a listener.
func (t *Trace) AddListener(l TreeListener) {
	t.listeners = append(t.listeners, l)
}

// RemoveListener delete a listener.
func (t *Trace) RemoveListener(l TreeListener) {
	victim := -1
	for i, lis := range t.listeners {
		if lis == l {
			victim = i
			break
		}
	}
	if victim >= 0 {
		t.listeners = append(t.listeners[:victim], t.listeners[victim+1:]...)
	}
}

// Watch initiates model updates.
func (t *Trace) Watch(ctx context.Context) {
	go t.updater(ctx)
}

// Refresh updates the model now.
func (t *Trace) Refresh(ctx context.Context) {
	t.refresh(ctx)
}

// Peek returns model data.
func (t *Trace) Peek() *xray.TreeNode {
	return t.root
}

// InitialPath returns the FQN path of the node from which the trace was initiated.
func (t *Trace) InitialPath() string {
	return t.initialPath
}

// SetRefreshRate sets model refresh duration.
func (t *Trace) SetRefreshRate(d time.Duration) {
	t.refreshRate = d
}

// Describe describes a given resource.
func (t *Trace) Describe(ctx context.Context, gvr *client.GVR, path string) (string, error) {
	factory, ok := ctx.Value(internal.KeyFactory).(dao.Factory)
	if !ok {
		return "", fmt.Errorf("expected Factory in context but got %T", ctx.Value(internal.KeyFactory))
	}
	var g dao.Generic
	g.Init(factory, gvr)
	return g.Describe(path)
}

// ToYAML returns a resource yaml.
func (t *Trace) ToYAML(ctx context.Context, gvr *client.GVR, path string) (string, error) {
	factory, ok := ctx.Value(internal.KeyFactory).(dao.Factory)
	if !ok {
		return "", fmt.Errorf("expected Factory in context but got %T", ctx.Value(internal.KeyFactory))
	}
	var g dao.Generic
	g.Init(factory, gvr)
	return g.ToYAML(path, false)
}

func (t *Trace) updater(ctx context.Context) {
	defer slog.Debug("Trace-model canceled", slogs.GVR, t.gvr)

	rate := initTreeRefreshRate
	for {
		select {
		case <-ctx.Done():
			t.root = nil
			return
		case <-time.After(rate):
			rate = t.refreshRate
			t.refresh(ctx)
		}
	}
}

func (t *Trace) refresh(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&t.inUpdate, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&t.inUpdate, 0)

	if err := t.reconcile(ctx); err != nil {
		slog.Error("Trace reconcile failed", slogs.Error, err)
		t.fireTreeLoadFailed(err)
	}
}

func (t *Trace) reconcile(ctx context.Context) error {
	factory, ok := ctx.Value(internal.KeyFactory).(dao.Factory)
	if !ok {
		return fmt.Errorf("expected Factory in context but got %T", ctx.Value(internal.KeyFactory))
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cp := dao.NewCrossplane(factory)
	tree, err := cp.FetchTree(fetchCtx, t.gvr, t.path)
	if err != nil {
		return err
	}

	root := xray.NewTreeNode(client.NoGVR, "trace")
	t.buildTreeNodes(root, tree)
	root.Sort()

	if t.initialPath == "" {
		t.initialPath = t.findInitialPath(root)
	}

	if t.query != "" {
		t.root = root.Filter(t.query, rxMatch)
	}
	if t.root == nil || t.root.Diff(root) {
		t.root = root
		t.fireTreeChanged(t.root)
	}

	return nil
}

func (t *Trace) findInitialPath(root *xray.TreeNode) string {
	node := t.findNodeByPath(root, t.path)
	if node == nil {
		return ""
	}
	return node.Spec().AsPath()
}

func (t *Trace) findNodeByPath(node *xray.TreeNode, path string) *xray.TreeNode {
	if node.ID == path {
		return node
	}
	for _, c := range node.Children {
		if found := t.findNodeByPath(c, path); found != nil {
			return found
		}
	}
	return nil
}

func (t *Trace) buildTreeNodes(parent *xray.TreeNode, node *dao.CrossplaneNode) {
	if node == nil {
		return
	}

	navGVR := node.NavGVR
	if navGVR == nil {
		navGVR = node.GVR
	}
	treeNode := xray.BuildCrossplaneNode(navGVR, node.Object, node.Missing)
	parent.Add(treeNode)

	for _, child := range node.Children {
		t.buildTreeNodes(treeNode, child)
	}
}

func (t *Trace) fireTreeChanged(root *xray.TreeNode) {
	for _, l := range t.listeners {
		l.TreeChanged(root)
	}
}

func (t *Trace) fireTreeLoadFailed(err error) {
	for _, l := range t.listeners {
		l.TreeLoadFailed(err)
	}
}
