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
	MaxLogBytes      int64
	MaxConcurrent    int
	KeepFailedPods   bool
	DefaultImage     string
	LogLevel         string
}

// FromEnv builds a Config, applying defaults for anything unset.
func FromEnv() (Config, error) {
	c := Config{
		Port:            envInt("PORT", 8080),
		Namespace:       envStr("POD_NAMESPACE", "podspawner"),
		Kubeconfig:      os.Getenv("KUBECONFIG"),
		WorkerSA:        os.Getenv("WORKER_SERVICE_ACCOUNT"),
		MaxLogBytes:     int64(envInt("MAX_LOG_BYTES", 64*1024)),
		MaxConcurrent:   envInt("MAX_CONCURRENT", 10),
		KeepFailedPods:  envBool("KEEP_FAILED_PODS", false),
		DefaultImage:    envStr("WORKER_DEFAULT_IMAGE", "busybox:1.36"),
		LogLevel:        envStr("LOG_LEVEL", "info"),
		DefaultTimeout:  10 * time.Minute,
		ScheduleTimeout: 2 * time.Minute,
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
