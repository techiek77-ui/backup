package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSelector(t *testing.T) {
	cases := []struct {
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"", nil, false},
		{"  ", nil, false},
		{"workload=worker", map[string]string{"workload": "worker"}, false},
		{"name=pool1, oci.oraclecloud.com/fault-domain=FAULT-DOMAIN-1", map[string]string{"name": "pool1", "oci.oraclecloud.com/fault-domain": "FAULT-DOMAIN-1"}, false},
		{"workload=", map[string]string{"workload": ""}, false},
		{"novalue", nil, true},
		{"=x", nil, true},
	}
	for _, c := range cases {
		got, err := ParseSelector(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "jobs")
	t.Setenv("WORKER_NODE_SELECTOR", "name=workers")
	t.Setenv("WORKER_TOLERATIONS", `[{"key":"dedicated","operator":"Equal","value":"worker","effect":"NoSchedule"}]`)
	t.Setenv("WORKER_IMAGE_PULL_SECRET", "ocirsecret, other")
	t.Setenv("WORKER_DEFAULT_TIMEOUT", "90s")
	t.Setenv("WORKER_IMAGE_PULL_TIMEOUT", "45s")
	t.Setenv("SWEEP_INTERVAL", "1m")
	t.Setenv("MAX_CONCURRENT", "3")
	t.Setenv("KEEP_FAILED_PODS", "true")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Namespace != "jobs" || c.NodeSelector["name"] != "workers" || c.MaxConcurrent != 3 || !c.KeepFailedPods {
		t.Errorf("unexpected config: %+v", c)
	}
	if c.DefaultTimeout != 90*time.Second || c.ScheduleTimeout != 2*time.Minute {
		t.Errorf("timeouts: %v %v", c.DefaultTimeout, c.ScheduleTimeout)
	}
	if c.ImagePullTimeout != 45*time.Second || c.SweepInterval != time.Minute || c.APIRetryWindow != 2*time.Minute {
		t.Errorf("new durations: pull=%v sweep=%v retry=%v", c.ImagePullTimeout, c.SweepInterval, c.APIRetryWindow)
	}
	if len(c.Tolerations) != 1 || c.Tolerations[0].Key != "dedicated" || string(c.Tolerations[0].Effect) != "NoSchedule" {
		t.Errorf("tolerations: %+v", c.Tolerations)
	}
	if !reflect.DeepEqual(c.ImagePullSecrets, []string{"ocirsecret", "other"}) {
		t.Errorf("pull secrets: %v", c.ImagePullSecrets)
	}
}

func TestFromEnvErrors(t *testing.T) {
	t.Setenv("WORKER_TOLERATIONS", `not json`)
	if _, err := FromEnv(); err == nil {
		t.Error("bad tolerations should error")
	}
	t.Setenv("WORKER_TOLERATIONS", "")
	t.Setenv("WORKER_NODE_SELECTOR", "oops")
	if _, err := FromEnv(); err == nil {
		t.Error("bad selector should error")
	}
	t.Setenv("WORKER_NODE_SELECTOR", "")
	t.Setenv("MAX_CONCURRENT", "0")
	if _, err := FromEnv(); err == nil {
		t.Error("MAX_CONCURRENT=0 should error")
	}
	t.Setenv("MAX_CONCURRENT", "")
	t.Setenv("SWEEP_INTERVAL", "0s")
	if _, err := FromEnv(); err == nil {
		t.Error("SWEEP_INTERVAL=0 should error")
	}
}

func TestParseSizeClasses(t *testing.T) {
	def, err := ParseSizeClasses("")
	if err != nil || cpuOf(def["default"]) != "500m" || memOf(def["default"]) != "256Mi" {
		t.Fatalf("default classes = %v err=%v", def, err)
	}

	classes, err := ParseSizeClasses(`{
	  "small": {"cpu":"1","memory":"2Gi","nodeSelector":{"size":"small"},"nodePoolId":"ocid1.nodepool.oc1..small"},
	  "large": {"cpu":"8","memory":"32Gi","tolerations":[{"key":"dedicated","operator":"Equal","value":"scan","effect":"NoSchedule"}]}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassNames(classes); !reflect.DeepEqual(got, []string{"large", "small"}) {
		t.Fatalf("names = %v", got)
	}
	if cpuOf(classes["small"]) != "1" || classes["small"].NodeSelector["size"] != "small" || classes["small"].NodePoolID != "ocid1.nodepool.oc1..small" {
		t.Errorf("small = %+v", classes["small"])
	}
	if memOf(classes["large"]) != "32Gi" || len(classes["large"].Tolerations) != 1 || classes["large"].Tolerations[0].Key != "dedicated" {
		t.Errorf("large = %+v", classes["large"])
	}

	for name, bad := range map[string]string{
		"not json":      `{`,
		"empty":         `{}`,
		"bad cpu":       `{"a":{"cpu":"lots","memory":"1Gi"}}`,
		"zero memory":   `{"a":{"cpu":"1","memory":"0"}}`,
		"unknown field": `{"a":{"cpu":"1","memory":"1Gi","shape":"VM.Standard.E4.Flex"}}`,
		"bad name":      `{"Big Scan!":{"cpu":"1","memory":"1Gi"}}`,
		"bad selector":  `{"a":{"cpu":"1","memory":"1Gi","nodeSelector":{"bad key!":"x"}}}`,
	} {
		if _, err := ParseSizeClasses(bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFromEnvNodeModes(t *testing.T) {
	t.Setenv("SIZE_CLASSES", `{"small":{"cpu":"1","memory":"2Gi","nodePoolId":"ocid1.nodepool.oc1..a"},"large":{"cpu":"4","memory":"8Gi"}}`)
	t.Setenv("DEFAULT_SIZE_CLASS", "small")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeMode != NodeModeNone || !c.OnePodPerNode || c.ProvisionTimeout != 15*time.Minute || c.OCIAuthMode != OCIAuthAuto {
		t.Errorf("defaults: %+v", c)
	}
	if c.DefaultTimeout != 24*time.Hour || c.MaxTimeout != 24*time.Hour || c.LogTailLines != 1000 || c.MaxLogBytes != 256*1024 {
		t.Errorf("long-scan defaults: default=%s max=%s tail=%d bytes=%d", c.DefaultTimeout, c.MaxTimeout, c.LogTailLines, c.MaxLogBytes)
	}

	t.Setenv("NODE_MODE", "oke")
	if c, err := FromEnv(); err != nil || c.SizeClasses["large"].NodePoolID != "" {
		t.Fatalf("oke mode allows classes without a pool (scheduled normally), got %v", err)
	}
	t.Setenv("SIZE_CLASSES", `{"small":{"cpu":"1","memory":"2Gi"},"large":{"cpu":"4","memory":"8Gi"}}`)
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "at least one size class") {
		t.Fatalf("oke mode must require at least one class with a pool, got %v", err)
	}
	t.Setenv("SIZE_CLASSES", `{"small":{"cpu":"1","memory":"2Gi","nodePoolId":"ocid1.nodepool.oc1..a"}}`)
	t.Setenv("OCI_AUTH_MODE", "instance-principal")
	t.Setenv("NODE_PROVISION_TIMEOUT", "20m")
	t.Setenv("ONE_POD_PER_NODE", "false")
	c, err = FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeMode != NodeModeOKE || c.OCIAuthMode != OCIAuthInstancePrincipal || c.ProvisionTimeout != 20*time.Minute || c.OnePodPerNode {
		t.Errorf("oke config: %+v", c)
	}

	t.Setenv("DEFAULT_SIZE_CLASS", "huge")
	if _, err := FromEnv(); err == nil {
		t.Error("unknown DEFAULT_SIZE_CLASS must error")
	}
	t.Setenv("DEFAULT_SIZE_CLASS", "small")
	t.Setenv("NODE_MODE", "karpenter")
	if _, err := FromEnv(); err == nil {
		t.Error("unknown NODE_MODE must error")
	}
	t.Setenv("NODE_MODE", "none")
	t.Setenv("OCI_AUTH_MODE", "password")
	if _, err := FromEnv(); err == nil {
		t.Error("unknown OCI_AUTH_MODE must error")
	}
	t.Setenv("OCI_AUTH_MODE", "")
	t.Setenv("WORKER_DEFAULT_TIMEOUT", "48h")
	t.Setenv("WORKER_MAX_TIMEOUT", "24h")
	if _, err := FromEnv(); err == nil {
		t.Error("default timeout above the max must error")
	}
}

// Quantity's String has a pointer receiver; map values are not addressable.
func cpuOf(c SizeClass) string { return c.CPU.String() }
func memOf(c SizeClass) string { return c.Memory.String() }

func TestSizeClassesFromYAMLFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "size-classes.yaml")
	if err := os.WriteFile(file, []byte(`# comments are fine
small:
  cpu: 1          # bare numbers work
  memory: 2Gi
  nodePoolId: ocid1.nodepool.oc1..small
large:
  cpu: "8"
  memory: 32Gi
  nodeSelector:
    size: large
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIZE_CLASSES_FILE", file)
	t.Setenv("SIZE_CLASSES", `{"ignored":{"cpu":"1","memory":"1Gi"}}`) // file wins
	t.Setenv("DEFAULT_SIZE_CLASS", "small")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassNames(c.SizeClasses); !reflect.DeepEqual(got, []string{"large", "small"}) {
		t.Fatalf("names = %v", got)
	}
	if cpuOf(c.SizeClasses["small"]) != "1" || memOf(c.SizeClasses["large"]) != "32Gi" || c.SizeClasses["large"].NodeSelector["size"] != "large" {
		t.Errorf("classes = %+v", c.SizeClasses)
	}

	t.Setenv("SIZE_CLASSES_FILE", filepath.Join(dir, "missing.yaml"))
	if _, err := FromEnv(); err == nil {
		t.Error("missing file must error")
	}
	if err := os.WriteFile(file, []byte("small: [not, a, class]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIZE_CLASSES_FILE", file)
	if _, err := FromEnv(); err == nil {
		t.Error("malformed file must error")
	}

	// bare numbers are accepted in the env JSON form too
	t.Setenv("SIZE_CLASSES_FILE", "")
	t.Setenv("SIZE_CLASSES", `{"small":{"cpu":2,"memory":"4Gi"}}`)
	if c, err := FromEnv(); err != nil || cpuOf(c.SizeClasses["small"]) != "2" {
		t.Fatalf("numeric cpu in JSON: %v %v", err, c.SizeClasses)
	}
}
