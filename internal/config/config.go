// Package config reads controller settings from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Config is everything the controller needs, sourced from the environment.
type Config struct {
	Port      int
	Namespace string
	// Kubeconfig is only used when not running in-cluster (local dev).
	Kubeconfig string

	NodeSelector     map[string]string
	Tolerations      []corev1.Toleration
	ImagePullSecrets []string
	WorkerSA         string
	DefaultTimeout   time.Duration
	ScheduleTimeout  time.Duration
	ImagePullTimeout time.Duration
	APIRetryWindow   time.Duration
	SweepInterval    time.Duration
	MaxLogBytes      int64
	MaxConcurrent    int
	KeepFailedPods   bool
	DefaultImage     string
	LogLevel         string

	// NodeMode decides who brings nodes up for worker pods:
	//   none  the pod just carries its scheduling constraints; Cluster
	//         Autoscaler, Karpenter or a virtual node pool provides the node
	//   oke   the controller grows the class's OKE node pool by one, pins the
	//         pod to the new node and deletes the node when the pod is done
	NodeMode         string
	SizeClasses      map[string]SizeClass
	DefaultSizeClass string
	// OnePodPerNode adds a required pod anti-affinity so two worker pods never
	// share a node, whichever mode provides the nodes.
	OnePodPerNode bool
	// ProvisionTimeout bounds how long a node may take to become Ready in oke mode.
	ProvisionTimeout time.Duration
	// OCIAuthMode: auto | workload-identity | instance-principal | config-file.
	OCIAuthMode string
}

const (
	NodeModeNone = "none"
	NodeModeOKE  = "oke"

	OCIAuthAuto              = "auto"
	OCIAuthWorkloadIdentity  = "workload-identity"
	OCIAuthInstancePrincipal = "instance-principal"
	OCIAuthConfigFile        = "config-file"
)

// FromEnv builds a Config, applying defaults for anything unset.
func FromEnv() (Config, error) {
	c := Config{
		Port:             envInt("PORT", 8080),
		Namespace:        envStr("POD_NAMESPACE", "podspawner"),
		Kubeconfig:       os.Getenv("KUBECONFIG"),
		WorkerSA:         os.Getenv("WORKER_SERVICE_ACCOUNT"),
		MaxLogBytes:      int64(envInt("MAX_LOG_BYTES", 64*1024)),
		MaxConcurrent:    envInt("MAX_CONCURRENT", 10),
		KeepFailedPods:   envBool("KEEP_FAILED_PODS", false),
		DefaultImage:     envStr("WORKER_DEFAULT_IMAGE", "busybox:1.36"),
		LogLevel:         envStr("LOG_LEVEL", "info"),
		NodeMode:         strings.ToLower(envStr("NODE_MODE", NodeModeNone)),
		DefaultSizeClass: envStr("DEFAULT_SIZE_CLASS", "default"),
		OnePodPerNode:    envBool("ONE_POD_PER_NODE", true),
		ProvisionTimeout: 15 * time.Minute,
		OCIAuthMode:      strings.ToLower(envStr("OCI_AUTH_MODE", OCIAuthAuto)),
		DefaultTimeout:   10 * time.Minute,
		ScheduleTimeout:  2 * time.Minute,
		ImagePullTimeout: 3 * time.Minute,
		APIRetryWindow:   2 * time.Minute,
		SweepInterval:    5 * time.Minute,
	}

	var err error
	if c.NodeSelector, err = ParseSelector(envStr("WORKER_NODE_SELECTOR", "workload=worker")); err != nil {
		return c, fmt.Errorf("WORKER_NODE_SELECTOR: %w", err)
	}
	if c.DefaultTimeout, err = envDuration("WORKER_DEFAULT_TIMEOUT", c.DefaultTimeout); err != nil {
		return c, err
	}
	if c.ScheduleTimeout, err = envDuration("WORKER_SCHEDULE_TIMEOUT", c.ScheduleTimeout); err != nil {
		return c, err
	}
	if c.ImagePullTimeout, err = envDuration("WORKER_IMAGE_PULL_TIMEOUT", c.ImagePullTimeout); err != nil {
		return c, err
	}
	if c.APIRetryWindow, err = envDuration("API_RETRY_WINDOW", c.APIRetryWindow); err != nil {
		return c, err
	}
	if c.SweepInterval, err = envDuration("SWEEP_INTERVAL", c.SweepInterval); err != nil {
		return c, err
	}
	if c.ProvisionTimeout, err = envDuration("NODE_PROVISION_TIMEOUT", c.ProvisionTimeout); err != nil {
		return c, err
	}
	for name, d := range map[string]time.Duration{
		"WORKER_DEFAULT_TIMEOUT": c.DefaultTimeout, "WORKER_SCHEDULE_TIMEOUT": c.ScheduleTimeout,
		"WORKER_IMAGE_PULL_TIMEOUT": c.ImagePullTimeout, "API_RETRY_WINDOW": c.APIRetryWindow,
		"SWEEP_INTERVAL": c.SweepInterval, "NODE_PROVISION_TIMEOUT": c.ProvisionTimeout,
	} {
		if d <= 0 {
			return c, fmt.Errorf("%s must be > 0", name)
		}
	}
	if c.SizeClasses, err = ParseSizeClasses(os.Getenv("SIZE_CLASSES")); err != nil {
		return c, err
	}
	if _, ok := c.SizeClasses[c.DefaultSizeClass]; !ok {
		return c, fmt.Errorf("DEFAULT_SIZE_CLASS %q is not one of the configured classes %v", c.DefaultSizeClass, ClassNames(c.SizeClasses))
	}
	switch c.NodeMode {
	case NodeModeNone:
	case NodeModeOKE:
		for _, name := range ClassNames(c.SizeClasses) {
			if c.SizeClasses[name].NodePoolID == "" {
				return c, fmt.Errorf("NODE_MODE=oke requires nodePoolId on every size class; %q has none", name)
			}
		}
	default:
		return c, fmt.Errorf("NODE_MODE must be %q or %q, got %q", NodeModeNone, NodeModeOKE, c.NodeMode)
	}
	switch c.OCIAuthMode {
	case OCIAuthAuto, OCIAuthWorkloadIdentity, OCIAuthInstancePrincipal, OCIAuthConfigFile:
	default:
		return c, fmt.Errorf("OCI_AUTH_MODE must be one of auto, workload-identity, instance-principal, config-file; got %q", c.OCIAuthMode)
	}
	if v := strings.TrimSpace(os.Getenv("WORKER_TOLERATIONS")); v != "" {
		if err := json.Unmarshal([]byte(v), &c.Tolerations); err != nil {
			return c, fmt.Errorf("WORKER_TOLERATIONS must be a JSON array of tolerations: %w", err)
		}
	}
	if v := strings.TrimSpace(os.Getenv("WORKER_IMAGE_PULL_SECRET")); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.ImagePullSecrets = append(c.ImagePullSecrets, s)
			}
		}
	}
	if c.MaxConcurrent < 1 {
		return c, fmt.Errorf("MAX_CONCURRENT must be >= 1")
	}
	return c, nil
}

// ParseSelector turns "k1=v1,k2=v2" into a map. An empty string yields a nil
// map, meaning "no node selector" (schedule anywhere).
func ParseSelector(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("bad selector term %q, want key=value", pair)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
