package cosmosigner

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestStatefulSetUsesHTTPProbes(t *testing.T) {
	p := testParams()
	config, err := p.ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	sts, err := p.StatefulSet(config)
	if err != nil {
		t.Fatal(err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	for _, tc := range []struct{ name, path string }{{"startup", "/livez"}, {"liveness", "/livez"}, {"readiness", "/readyz"}} {
		probe := c.StartupProbe
		if tc.name == "liveness" {
			probe = c.LivenessProbe
		}
		if tc.name == "readiness" {
			probe = c.ReadinessProbe
		}
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != tc.path || probe.HTTPGet.Port != intstr.FromString("http") || probe.TCPSocket != nil {
			t.Fatalf("%s must GET %s on http: %#v", tc.name, tc.path, probe)
		}
	}
	if !strings.Contains(config, "http_addr: 0.0.0.0:8080") {
		t.Fatal(config)
	}
	httpEnv, httpPort := false, false
	for _, env := range c.Env {
		if env.Name == "COSMOSIGNER_HTTP_ADDR" && env.Value == "0.0.0.0:8080" {
			httpEnv = true
		}
	}
	for _, port := range c.Ports {
		if port.Name == "http" && port.ContainerPort == 8080 {
			httpPort = true
		}
	}
	if !httpEnv || !httpPort {
		t.Fatalf("HTTP env/port missing: %#v", c)
	}
	for _, port := range p.RaftService().Spec.Ports {
		if port.Port == 8080 {
			t.Fatal("HTTP port exposed by Service")
		}
	}
}
