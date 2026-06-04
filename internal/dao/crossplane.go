// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package dao

import (
	"context"
	"fmt"

	"github.com/derailed/k9s/internal/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CrossplaneNode represents a node in a Crossplane resource tree.
type CrossplaneNode struct {
	GVR      *client.GVR
	Object   *unstructured.Unstructured
	Missing  bool
	Children []*CrossplaneNode
}

// Crossplane fetches Crossplane resource trees.
type Crossplane struct {
	factory Factory
}

// NewCrossplane returns a new Crossplane DAO.
func NewCrossplane(f Factory) *Crossplane {
	return &Crossplane{factory: f}
}

// FetchTree fetches the full Crossplane resource tree, walking up to the root
// first if the given resource is not already the top-level claim/composite.
func (c *Crossplane) FetchTree(ctx context.Context, gvr *client.GVR, path string) (*CrossplaneNode, error) {
	rootGVR, rootPath, err := c.findRoot(ctx, gvr, path)
	if err != nil {
		rootGVR, rootPath = gvr, path
	}

	visited := make(map[string]bool)
	return c.fetchNode(ctx, rootGVR, rootPath, "", visited)
}

// findRoot walks up ownerReferences and claimRef to find the top-level resource.
func (c *Crossplane) findRoot(ctx context.Context, gvr *client.GVR, path string) (*client.GVR, string, error) {
	currentGVR, currentPath := gvr, path
	seen := make(map[string]bool)

	for {
		key := currentGVR.String() + "/" + currentPath
		if seen[key] {
			break
		}
		seen[key] = true

		obj, err := c.getResource(ctx, currentGVR, currentPath)
		if err != nil {
			return currentGVR, currentPath, nil
		}

		// If this resource has spec.resourceRef, it's a Claim — already a root.
		if _, ok := extractResourceRef(obj); ok {
			return currentGVR, currentPath, nil
		}

		// If this resource has spec.claimRef, walk up to the Claim.
		if claimRef, ok := extractClaimRef(obj); ok {
			claimGVR, claimPath, err := c.resolveRef(claimRef, "")
			if err == nil {
				return claimGVR, claimPath, nil
			}
			return currentGVR, currentPath, nil
		}

		// Walk up via ownerReferences.
		owners := obj.GetOwnerReferences()
		if len(owners) == 0 {
			return currentGVR, currentPath, nil
		}

		owner := owners[0]
		gv, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil {
			return currentGVR, currentPath, nil
		}
		ownerGVR, namespaced, found := MetaAccess.GVK2GVR(gv, owner.Kind)
		if !found {
			return currentGVR, currentPath, nil
		}

		if namespaced && obj.GetNamespace() != "" {
			currentPath = client.FQN(obj.GetNamespace(), owner.Name)
		} else {
			currentPath = client.FQN(client.ClusterScope, owner.Name)
		}
		currentGVR = ownerGVR
	}

	return currentGVR, currentPath, nil
}

func (c *Crossplane) fetchNode(ctx context.Context, gvr *client.GVR, path, expectedKind string, visited map[string]bool) (*CrossplaneNode, error) {
	key := gvr.String() + "/" + path
	if visited[key] {
		return nil, nil
	}
	visited[key] = true

	obj, err := c.getResource(ctx, gvr, path)
	if err != nil {
		ns, name := client.Namespaced(path)
		placeholder := &unstructured.Unstructured{}
		placeholder.SetNamespace(ns)
		placeholder.SetName(name)
		if expectedKind != "" {
			placeholder.SetKind(expectedKind)
		}
		return &CrossplaneNode{GVR: gvr, Object: placeholder, Missing: true}, nil
	}

	node := &CrossplaneNode{
		GVR:    gvr,
		Object: obj,
	}

	parentNamespace := obj.GetNamespace()

	// Follow spec.resourceRef (Claim → Composite, V1 only)
	if ref, ok := extractResourceRef(obj); ok {
		childGVR, childPath, err := c.resolveRef(ref, parentNamespace)
		if err == nil {
			child, err := c.fetchNode(ctx, childGVR, childPath, ref.Kind, visited)
			if err == nil && child != nil {
				node.Children = append(node.Children, child)
			}
		}
	}

	// Follow spec.resourceRefs (V1: top-level) or spec.crossplane.resourceRefs (V2)
	if refs, ok := extractResourceRefs(obj); ok {
		for _, ref := range refs {
			childGVR, childPath, err := c.resolveRef(ref, parentNamespace)
			if err != nil {
				// GVR unknown but ref exists — show as missing node with what we know
				placeholder := &unstructured.Unstructured{}
				placeholder.SetName(ref.Name)
				placeholder.SetNamespace(ref.Namespace)
				placeholder.SetKind(ref.Kind)
				node.Children = append(node.Children, &CrossplaneNode{
					GVR:     client.NewGVR(ref.APIVersion + "/" + ref.Kind),
					Object:  placeholder,
					Missing: true,
				})
				continue
			}
			child, err := c.fetchNode(ctx, childGVR, childPath, ref.Kind, visited)
			if err == nil && child != nil {
				node.Children = append(node.Children, child)
			}
		}
	}

	return node, nil
}

func (c *Crossplane) getResource(ctx context.Context, gvr *client.GVR, path string) (*unstructured.Unstructured, error) {
	dial, err := c.factory.Client().DynDial()
	if err != nil {
		return nil, err
	}

	ns, name := client.Namespaced(path)
	res := dial.Resource(gvr.GVR())

	var obj *unstructured.Unstructured
	if client.IsClusterScoped(ns) || ns == "" {
		obj, err = res.Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = res.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}

	return obj, err
}

type resourceRef struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

func extractResourceRef(obj *unstructured.Unstructured) (resourceRef, bool) {
	ref, found, err := unstructured.NestedMap(obj.Object, "spec", "resourceRef")
	if err != nil || !found || ref == nil {
		return resourceRef{}, false
	}
	return parseRef(ref), true
}

func extractClaimRef(obj *unstructured.Unstructured) (resourceRef, bool) {
	ref, found, err := unstructured.NestedMap(obj.Object, "spec", "claimRef")
	if err != nil || !found || ref == nil {
		return resourceRef{}, false
	}
	return parseRef(ref), true
}

func extractResourceRefs(obj *unstructured.Unstructured) ([]resourceRef, bool) {
	// V1: spec.resourceRefs
	if refs, found, err := unstructured.NestedSlice(obj.Object, "spec", "resourceRefs"); err == nil && found && len(refs) > 0 {
		return parseRefs(refs)
	}
	// V2: spec.crossplane.resourceRefs
	if refs, found, err := unstructured.NestedSlice(obj.Object, "spec", "crossplane", "resourceRefs"); err == nil && found && len(refs) > 0 {
		return parseRefs(refs)
	}
	return nil, false
}

func parseRefs(refs []interface{}) ([]resourceRef, bool) {
	result := make([]resourceRef, 0, len(refs))
	for _, r := range refs {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		result = append(result, parseRef(m))
	}
	return result, len(result) > 0
}

func parseRef(m map[string]interface{}) resourceRef {
	ref := resourceRef{}
	if v, ok := m["apiVersion"].(string); ok {
		ref.APIVersion = v
	}
	if v, ok := m["kind"].(string); ok {
		ref.Kind = v
	}
	if v, ok := m["name"].(string); ok {
		ref.Name = v
	}
	if v, ok := m["namespace"].(string); ok {
		ref.Namespace = v
	}
	return ref
}

func (c *Crossplane) resolveRef(ref resourceRef, parentNamespace string) (*client.GVR, string, error) {
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return nil, "", fmt.Errorf("incomplete resource reference")
	}

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, "", err
	}

	gvr, namespaced, found := MetaAccess.GVK2GVR(gv, ref.Kind)
	if !found {
		// Version may have changed (e.g. provider upgrade from v1beta1 → v1beta2).
		// Fall back to matching by group+kind only.
		gvr, namespaced, found = MetaAccess.GKR2GVR(gv.Group, ref.Kind)
	}
	if !found {
		return nil, "", fmt.Errorf("unable to resolve GVR for %s/%s", ref.APIVersion, ref.Kind)
	}

	var path string
	if namespaced && ref.Namespace != "" {
		path = client.FQN(ref.Namespace, ref.Name)
	} else if namespaced && parentNamespace != "" {
		// V2: refs omit namespace — inherit from parent resource.
		path = client.FQN(parentNamespace, ref.Name)
	} else if namespaced {
		path = client.FQN(client.ClusterScope, ref.Name)
	} else {
		path = client.FQN(client.ClusterScope, ref.Name)
	}

	return gvr, path, nil
}
