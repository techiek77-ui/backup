package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// SizeClass describes one request size: the resources a worker pod gets
// (requests = limits, so the pod is Guaranteed and, on Karpenter or virtual
// nodes, the request is what sizes and bills the node) and where it may run.
type SizeClass struct {
	CPU    resource.Quantity
	Memory resource.Quantity
	// NodeSelector is merged over the global WORKER_NODE_SELECTOR; class keys win.
	NodeSelector map[string]string
	// Tolerations are appended to the global WORKER_TOLERATIONS.
	Tolerations []corev1.Toleration
	// NodePoolID is the OKE node pool the controller scales for this class.
	// Required only in NODE_MODE=oke.
	NodePoolID string
}

// sizeClassJSON is the on-the-wire shape of SIZE_CLASSES entries.
type sizeClassJSON struct {
	CPU          string              `json:"cpu"`
	Memory       string              `json:"memory"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	NodePoolID   string              `json:"nodePoolId,omitempty"`
}

// DefaultSizeClasses is used when SIZE_CLASSES is unset: one class matching
// the limits the PoC used before size classes existed.
func DefaultSizeClasses() map[string]SizeClass {
	return map[string]SizeClass{
		"default": {CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi")},
	}
}

// ParseSizeClasses decodes SIZE_CLASSES, e.g.
//
//	{"small":{"cpu":"1","memory":"2Gi","nodeSelector":{"size":"small"},"nodePoolId":"ocid1.nodepool..."},
//	 "large":{"cpu":"8","memory":"32Gi","nodeSelector":{"size":"large"},"nodePoolId":"ocid1.nodepool..."}}
func ParseSizeClasses(raw string) (map[string]SizeClass, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSizeClasses(), nil
	}
	var in map[string]sizeClassJSON
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("SIZE_CLASSES must be a JSON object of classes: %w", err)
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("SIZE_CLASSES must define at least one class")
	}
	out := make(map[string]SizeClass, len(in))
	for name, c := range in {
		if errs := validation.IsValidLabelValue(name); name == "" || len(errs) > 0 {
			return nil, fmt.Errorf("SIZE_CLASSES: class name %q is not a valid label value (%s)", name, strings.Join(errs, "; "))
		}
		cpu, err := resource.ParseQuantity(c.CPU)
		if err != nil || cpu.Sign() <= 0 {
			return nil, fmt.Errorf("SIZE_CLASSES[%s].cpu must be a positive quantity such as \"500m\" or \"2\"", name)
		}
		mem, err := resource.ParseQuantity(c.Memory)
		if err != nil || mem.Sign() <= 0 {
			return nil, fmt.Errorf("SIZE_CLASSES[%s].memory must be a positive quantity such as \"256Mi\" or \"4Gi\"", name)
		}
		for k := range c.NodeSelector {
			if errs := validation.IsQualifiedName(k); len(errs) > 0 {
				return nil, fmt.Errorf("SIZE_CLASSES[%s].nodeSelector key %q: %s", name, k, strings.Join(errs, "; "))
			}
		}
		out[name] = SizeClass{
			CPU:          cpu,
			Memory:       mem,
			NodeSelector: c.NodeSelector,
			Tolerations:  c.Tolerations,
			NodePoolID:   strings.TrimSpace(c.NodePoolID),
		}
	}
	return out, nil
}

// ClassNames returns the class names sorted, for messages and the API.
func ClassNames(classes map[string]SizeClass) []string {
	names := make([]string, 0, len(classes))
	for n := range classes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
