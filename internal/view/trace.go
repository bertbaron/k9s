// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/dao"
	"github.com/derailed/k9s/internal/model"
	"github.com/derailed/k9s/internal/render"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/k9s/internal/ui/dialog"
	"github.com/derailed/k9s/internal/view/cmd"
	"github.com/derailed/k9s/internal/xray"
	"github.com/derailed/tcell/v2"
	"github.com/derailed/tview"
	"github.com/sahilm/fuzzy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
)

const traceTitle = "Trace"

var _ model.Component = (*Trace)(nil)

// Trace represents a Crossplane resource trace tree view.
type Trace struct {
	*ui.Tree

	app      *App
	gvr      *client.GVR
	path     string
	model    *model.Trace
	cancelFn context.CancelFunc
	envFn    EnvFunc
}

// NewTrace returns a new trace view.
func NewTrace(gvr *client.GVR, path string) ResourceViewer {
	return &Trace{
		gvr:   gvr,
		path:  path,
		Tree:  ui.NewTree(),
		model: model.NewTrace(gvr, path),
	}
}

func (*Trace) SetCommand(*cmd.Interpreter)            {}
func (*Trace) SetFilter(string, bool)                 {}
func (*Trace) SetLabelSelector(labels.Selector, bool) {}

// Init initializes the view.
func (t *Trace) Init(ctx context.Context) error {
	t.envFn = t.k9sEnv

	if err := t.Tree.Init(ctx); err != nil {
		return err
	}
	t.SetKeyListenerFn(t.keyEntered)

	var err error
	if t.app, err = extractApp(ctx); err != nil {
		return err
	}

	t.bindKeys()
	t.SetBackgroundColor(t.app.Styles.Xray().BgColor.Color())
	t.SetBorderColor(t.app.Styles.Xray().FgColor.Color())
	t.SetBorderFocusColor(t.app.Styles.Frame().Border.FocusColor.Color())
	t.SetGraphicsColor(t.app.Styles.Xray().GraphicColor.Color())

	_, name := client.Namespaced(t.path)
	t.SetTitle(fmt.Sprintf(" %s-%s/%s ", traceTitle, t.gvr.R(), name))

	t.model.SetRefreshRate(t.app.Config.K9s.RefreshDuration())
	t.model.AddListener(t)

	t.SetChangedFunc(func(n *tview.TreeNode) {
		spec, ok := n.GetReference().(xray.NodeSpec)
		if !ok {
			return
		}
		t.SetSelectedItem(spec.AsPath())
		t.refreshActions()
	})
	t.refreshActions()

	return nil
}

// InCmdMode checks if prompt is active.
func (*Trace) InCmdMode() bool {
	return false
}

// ExtraHints returns additional hints.
func (t *Trace) ExtraHints() map[string]string {
	if t.app.Config.K9s.UI.NoIcons {
		return nil
	}
	return xray.EmojiInfo()
}

// SetInstance sets specific resource instance.
func (*Trace) SetInstance(string) {}

func (t *Trace) bindKeys() {
	t.Actions().Bulk(ui.KeyMap{
		ui.KeySlash:     ui.NewSharedKeyAction("Filter Mode", t.activateCmd, false),
		tcell.KeyEscape: ui.NewSharedKeyAction("Filter Reset", t.resetCmd, false),
		tcell.KeyEnter:  ui.NewKeyAction("Goto", t.gotoCmd, true),
	})
}

func (t *Trace) keyEntered() {
	t.ClearSelection()
	t.update(t.filter(t.model.Peek()))
}

func (t *Trace) refreshActions() {
	aa := ui.NewKeyActions()

	defer func() {
		if err := pluginActions(t, aa); err != nil {
			slog.Warn("Plugins load failed", slogs.Error, err)
		}
		if err := hotKeyActions(t, aa); err != nil {
			slog.Warn("HotKeys load failed", slogs.Error, err)
		}
		t.Actions().Merge(aa)
		t.app.Menu().HydrateMenu(t.Hints())
	}()

	t.Actions().Clear()
	t.bindKeys()
	t.BindKeys()

	spec := t.selectedSpec()
	if spec == nil {
		return
	}

	gvr := spec.GVR()
	meta, err := dao.MetaAccess.MetaFor(gvr)
	if err != nil {
		return
	}

	if !t.app.Config.IsReadOnly() {
		if client.Can(meta.Verbs, "edit") {
			aa.Add(ui.KeyE, ui.NewKeyActionWithOpts("Edit", t.editCmd,
				ui.ActionOpts{
					Visible:   true,
					Dangerous: true,
				}))
		}
		if client.Can(meta.Verbs, "delete") {
			aa.Add(tcell.KeyCtrlD, ui.NewKeyActionWithOpts("Delete", t.deleteCmd,
				ui.ActionOpts{
					Visible:   true,
					Dangerous: true,
				}))
		}
	}
	if !dao.IsK9sMeta(meta) {
		aa.Bulk(ui.KeyMap{
			ui.KeyY: ui.NewKeyAction(yamlAction, t.viewCmd, true),
			ui.KeyD: ui.NewKeyAction("Describe", t.describeCmd, true),
		})
	}
	t.Actions().Merge(aa)
}

// GetSelectedPath returns the current selection as string.
func (t *Trace) GetSelectedPath() string {
	spec := t.selectedSpec()
	if spec == nil {
		return ""
	}
	return spec.Path()
}

func (t *Trace) selectedSpec() *xray.NodeSpec {
	node := t.GetCurrentNode()
	if node == nil {
		return nil
	}
	ref, ok := node.GetReference().(xray.NodeSpec)
	if !ok {
		return nil
	}
	return &ref
}

// EnvFn returns a plugin env function if available.
func (t *Trace) EnvFn() EnvFunc {
	return t.envFn
}

func (t *Trace) k9sEnv() Env {
	env := k8sEnv(t.app.Conn().Config())

	spec := t.selectedSpec()
	if spec == nil {
		return env
	}

	env["FILTER"] = t.CmdBuff().GetText()
	if env["FILTER"] == "" {
		ns, n := client.Namespaced(spec.Path())
		env["NAMESPACE"], env["FILTER"] = ns, n
	}
	ns, n := client.Namespaced(spec.Path())
	env["NAMESPACE"], env["NAME"] = ns, n

	return env
}

// Aliases returns all available aliases.
func (t *Trace) Aliases() sets.Set[string] {
	return sets.New[string]()
}

func (t *Trace) viewCmd(evt *tcell.EventKey) *tcell.EventKey {
	spec := t.selectedSpec()
	if spec == nil {
		return evt
	}

	ctx := t.defaultContext()
	raw, err := t.model.ToYAML(ctx, spec.GVR(), spec.Path())
	if err != nil {
		t.App().Flash().Errf("unable to get resource %q -- %s", spec.GVR(), err)
		return nil
	}

	details := NewDetails(t.app, yamlAction, spec.Path(), contentYAML, true).Update(raw)
	if err := t.app.inject(details, false); err != nil {
		t.app.Flash().Err(err)
	}

	return nil
}

func (t *Trace) deleteCmd(evt *tcell.EventKey) *tcell.EventKey {
	spec := t.selectedSpec()
	if spec == nil {
		return evt
	}

	t.Stop()
	defer t.Start()

	meta, err := dao.MetaAccess.MetaFor(spec.GVR())
	if err != nil {
		return nil
	}
	t.resourceDelete(spec.GVR(), spec, fmt.Sprintf("Delete %s %s?", meta.SingularName, spec.Path()))

	return nil
}

func (t *Trace) describeCmd(evt *tcell.EventKey) *tcell.EventKey {
	spec := t.selectedSpec()
	if spec == nil {
		return evt
	}

	ctx := t.defaultContext()
	yaml, err := t.model.Describe(ctx, spec.GVR(), spec.Path())
	if err != nil {
		t.app.Flash().Errf("Describe command failed: %s", err)
		return nil
	}

	details := NewDetails(t.app, "Describe", spec.Path(), contentYAML, true).Update(yaml)
	if err := t.app.inject(details, false); err != nil {
		t.app.Flash().Err(err)
	}

	return nil
}

func (t *Trace) editCmd(evt *tcell.EventKey) *tcell.EventKey {
	spec := t.selectedSpec()
	if spec == nil {
		return evt
	}

	t.Stop()
	defer t.Start()

	ns, n := client.Namespaced(spec.Path())
	args := make([]string, 0, 10)
	args = append(args,
		"edit",
		spec.GVR().R(),
		"-n", ns,
		"--context", t.app.Config.K9s.ActiveContextName(),
	)
	if cfg := t.app.Conn().Config().Flags().KubeConfig; cfg != nil && *cfg != "" {
		args = append(args, "--kubeconfig", *cfg)
	}
	if err := runK(t.app, &shellOpts{args: append(args, n)}); err != nil {
		t.app.Flash().Errf("Edit exec failed: %s", err)
	}

	return evt
}

func (t *Trace) activateCmd(evt *tcell.EventKey) *tcell.EventKey {
	if t.app.InCmdMode() {
		return evt
	}
	t.app.ResetPrompt(t.CmdBuff())
	return nil
}

func (t *Trace) resetCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !t.CmdBuff().InCmdMode() {
		t.CmdBuff().Reset()
		return t.app.PrevCmd(evt)
	}
	t.CmdBuff().Reset()
	t.model.ClearFilter()
	t.Start()
	return nil
}

func (t *Trace) gotoCmd(*tcell.EventKey) *tcell.EventKey {
	if t.CmdBuff().IsActive() {
		if internal.IsLabelSelector(t.CmdBuff().GetText()) {
			t.Start()
		}
		t.CmdBuff().SetActive(false)
		t.GetRoot().ExpandAll()
		return nil
	}

	spec := t.selectedSpec()
	if spec == nil {
		return nil
	}
	if len(strings.Split(spec.Path(), "/")) == 1 {
		return nil
	}
	t.app.gotoResource(spec.GVR().String(), spec.Path(), false, true)
	return nil
}

func (t *Trace) filter(root *xray.TreeNode) *xray.TreeNode {
	q := t.CmdBuff().GetText()
	if t.CmdBuff().Empty() || internal.IsLabelSelector(q) {
		return root
	}

	t.UpdateTitle()
	if f, ok := internal.IsFuzzySelector(q); ok {
		return root.Filter(f, traceFuzzyFilter)
	}
	if internal.IsInverseSelector(q) {
		return root.Filter(q, traceRxInverseFilter)
	}
	return root.Filter(q, traceRxFilter)
}

// TreeNodeSelected callback for node selection.
func (t *Trace) TreeNodeSelected() {
	t.app.QueueUpdateDraw(func() {
		n := t.GetCurrentNode()
		if n != nil {
			n.SetColor(t.app.Styles.Xray().CursorColor.Color())
		}
	})
}

// TreeLoadFailed notifies the load failed.
func (t *Trace) TreeLoadFailed(err error) {
	t.app.Flash().Err(err)
}

func (t *Trace) update(node *xray.TreeNode) {
	root := traceTreeNode(node, t.ExpandNodes(), t.app.Config.K9s.UI.NoIcons, t.app.Styles)
	if node == nil {
		t.app.QueueUpdateDraw(func() {
			t.SetRoot(root)
		})
		return
	}

	for _, c := range node.Children {
		t.hydrate(root, c)
	}
	if t.GetSelectedItem() == "" {
		if ip := t.model.InitialPath(); ip != "" {
			t.SetSelectedItem(ip)
		} else {
			t.SetSelectedItem(node.Spec().Path())
		}
	}

	t.app.QueueUpdateDraw(func() {
		t.SetRoot(root)
		root.Walk(func(node, parent *tview.TreeNode) bool {
			spec, ok := node.GetReference().(xray.NodeSpec)
			if !ok {
				return false
			}
			if parent != nil {
				node.SetExpanded(t.ExpandNodes())
			} else {
				node.SetExpanded(true)
			}
			if spec.AsPath() == t.GetSelectedItem() {
				node.SetExpanded(true).SetSelectable(true)
				t.SetCurrentNode(node)
			}
			return true
		})
	})
}

// TreeChanged notifies the model data changed.
func (t *Trace) TreeChanged(node *xray.TreeNode) {
	t.Count = node.Count(client.NoGVR)
	t.update(t.filter(node))
	t.UpdateTitle()
}

func (t *Trace) hydrate(parent *tview.TreeNode, n *xray.TreeNode) {
	node := traceTreeNode(n, t.ExpandNodes(), t.app.Config.K9s.UI.NoIcons, t.app.Styles)
	for _, c := range n.Children {
		t.hydrate(node, c)
	}
	parent.AddChild(node)
}

// SetEnvFn sets the custom environment function.
func (*Trace) SetEnvFn(EnvFunc) {}

// Refresh updates the view.
func (*Trace) Refresh() {}

// BufferCompleted indicates the buffer was changed.
func (t *Trace) BufferCompleted(_, _ string) {
	t.update(t.filter(t.model.Peek()))
}

// BufferChanged indicates the buffer was changed.
func (*Trace) BufferChanged(_, _ string) {}

// BufferActive indicates the buff activity changed.
func (t *Trace) BufferActive(state bool, k model.BufferKind) {
	t.app.BufferActive(state, k)
}

func (t *Trace) defaultContext() context.Context {
	ctx := context.WithValue(context.Background(), internal.KeyFactory, t.app.factory)
	ctx = context.WithValue(ctx, internal.KeyFields, "")
	ctx = context.WithValue(ctx, internal.KeyLabels, labels.Everything())
	return ctx
}

// Start initializes resource watch loop.
func (t *Trace) Start() {
	t.Stop()
	t.CmdBuff().AddListener(t)

	ctx := t.defaultContext()
	ctx, t.cancelFn = context.WithCancel(ctx)
	t.model.Watch(ctx)
	t.UpdateTitle()
}

// Stop terminates watch loop.
func (t *Trace) Stop() {
	if t.cancelFn == nil {
		return
	}
	t.cancelFn()
	t.cancelFn = nil
	t.CmdBuff().RemoveListener(t)
}

// AddBindKeysFn sets up extra key bindings.
func (*Trace) AddBindKeysFn(BindKeysFunc) {}

// SetContextFn sets custom context.
func (*Trace) SetContextFn(ContextFunc) {}

// Name returns the component name.
func (*Trace) Name() string { return "Trace" }

// GetTable returns the underlying table.
func (*Trace) GetTable() *Table { return nil }

// GVR returns a resource descriptor.
func (t *Trace) GVR() *client.GVR { return t.gvr }

// App returns the current app handle.
func (t *Trace) App() *App {
	return t.app
}

// UpdateTitle updates the view title.
func (t *Trace) UpdateTitle() {
	title := t.styleTitle()
	t.app.QueueUpdateDraw(func() {
		t.SetTitle(title)
	})
}

func (t *Trace) styleTitle() string {
	_, name := client.Namespaced(t.path)
	base := fmt.Sprintf("%s-%s/%s", traceTitle, t.gvr.R(), name)

	var title string
	styles := t.app.Styles.Frame()
	title = ui.SkinTitle(fmt.Sprintf(ui.TitleFmt, base, render.AsThousands(int64(t.Count))), &styles)

	buff := t.CmdBuff().GetText()
	if buff == "" {
		return title
	}
	return title + ui.SkinTitle(fmt.Sprintf(ui.SearchFmt, buff), &styles)
}

func (t *Trace) resourceDelete(gvr *client.GVR, spec *xray.NodeSpec, msg string) {
	d := t.app.Styles.Dialog()
	dialog.ShowDelete(&d, t.app.Content.Pages, msg, func(_ *metav1.DeletionPropagation, force bool) {
		t.app.Flash().Infof("Delete resource %s %s", spec.GVR(), spec.Path())
		accessor, err := dao.AccessorFor(t.app.factory, gvr)
		if err != nil {
			slog.Error("No accessor found", slogs.GVR, gvr, slogs.Error, err)
			return
		}
		nuker, ok := accessor.(dao.Nuker)
		if !ok {
			t.app.Flash().Errf("Invalid nuker %T", accessor)
			return
		}
		grace := dao.DefaultGrace
		if force {
			grace = dao.ForceGrace
		}
		if err := nuker.Delete(context.Background(), spec.Path(), nil, grace); err != nil {
			t.app.Flash().Errf("Delete failed with `%s", err)
		} else {
			t.app.Flash().Infof("%s `%s deleted successfully", t.GVR(), spec.Path())
		}
		t.Refresh()
	}, func() {})
}

// ----------------------------------------------------------------------------
// Helpers...

func traceFuzzyFilter(q, path string) bool {
	q = strings.TrimSpace(q[2:])
	mm := fuzzy.Find(q, []string{path})
	return len(mm) > 0
}

func traceRxFilter(q, path string) bool {
	rx := regexp.MustCompile(`(?i)` + q)
	tokens := strings.Split(path, xray.PathSeparator)
	for _, tok := range tokens {
		if rx.MatchString(tok) {
			return true
		}
	}
	return false
}

func traceRxInverseFilter(q, path string) bool {
	q = strings.TrimSpace(q[1:])
	rx := regexp.MustCompile(`(?i)` + q)
	tokens := strings.Split(path, xray.PathSeparator)
	for _, tok := range tokens {
		if rx.MatchString(tok) {
			return false
		}
	}
	return true
}

func traceTreeNode(node *xray.TreeNode, expanded, showIcons bool, styles *config.Styles) *tview.TreeNode {
	n := tview.NewTreeNode("No data...")
	if node != nil {
		n.SetText(node.Title(showIcons))
		n.SetReference(node.Spec())
	}
	n.SetSelectable(true)
	n.SetExpanded(expanded)
	n.SetColor(styles.Xray().CursorColor.Color())
	n.SetSelectedFunc(func() {
		n.SetExpanded(!n.IsExpanded())
	})
	return n
}
