// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package xray

import (
	"github.com/derailed/k9s/internal/client"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CrossplaneStatus returns a Synced/Ready string used as StatusKey.
// When both are True, returns OkStatus (no indicator shown).
// Otherwise returns the synced/ready values which render in red.
func CrossplaneStatus(obj *unstructured.Unstructured) string {
	if obj == nil {
		return MissingRefStatus
	}
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return OkStatus
	}

	synced, ready := true, true
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		s, _ := cond["status"].(string)
		switch t {
		case "Synced":
			synced = s == "True"
		case "Ready":
			ready = s == "True"
		}
	}

	if synced && ready {
		return OkStatus
	}

	sr := "True"
	if !synced {
		sr = "False"
	}
	rr := "True"
	if !ready {
		rr = "False"
	}
	return sr + "/" + rr
}

// CrossplaneInfo builds a concise info string showing the Kind,
// composition-resource-name, and the status message.
func CrossplaneInfo(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "Missing"
	}

	// Add composition-resource-name only (Kind is shown as the node title prefix via KindKey)
	annots := obj.GetAnnotations()
	resName := annots["crossplane.io/composition-resource-name"]
	var prefix string
	if resName != "" {
		prefix = resName
	}

	// Add status message from conditions
	msg := crossplaneConditionMessage(obj)

	switch {
	case prefix != "" && msg != "":
		return prefix + " | " + msg
	case prefix != "":
		return prefix
	case msg != "":
		return msg
	default:
		return ""
	}
}

const maxMessageLen = 80

func crossplaneConditionMessage(obj *unstructured.Unstructured) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}

	// Prefer the message from Synced=False (error), then Ready=False (creating/waiting)
	var syncedMsg, readyMsg string
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		s, _ := cond["status"].(string)
		m, _ := cond["message"].(string)
		reason, _ := cond["reason"].(string)
		switch t {
		case "Synced":
			if s != "True" && m != "" {
				syncedMsg = reason + ": " + m
			}
		case "Ready":
			if s != "True" && m != "" {
				readyMsg = reason + ": " + m
			}
		}
	}

	msg := syncedMsg
	if msg == "" {
		msg = readyMsg
	}
	if len(msg) > maxMessageLen {
		msg = msg[:maxMessageLen] + "..."
	}
	return msg
}

// BuildCrossplaneNode creates a TreeNode for a Crossplane resource.
// navGVR is the canonical/registered GVR used for display and navigation;
// it may differ from the GVR used to fetch the resource.
func BuildCrossplaneNode(navGVR *client.GVR, obj *unstructured.Unstructured, missing bool) *TreeNode {
	var id string
	if obj.GetNamespace() != "" && obj.GetNamespace() != client.ClusterScope {
		id = client.FQN(obj.GetNamespace(), obj.GetName())
	} else {
		id = client.FQN(client.ClusterScope, obj.GetName())
	}

	node := NewTreeNode(navGVR, id)
	if obj.GetKind() != "" {
		node.Extras[KindKey] = obj.GetKind()
	}
	if missing {
		node.Extras[StatusKey] = MissingRefStatus
	} else {
		node.Extras[StatusKey] = CrossplaneStatus(obj)
		if info := CrossplaneInfo(obj); info != "" {
			node.Extras[InfoKey] = info
		}
	}
	return node
}
