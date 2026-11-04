package config

import (
	"reflect"
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
}
