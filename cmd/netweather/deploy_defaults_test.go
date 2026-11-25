package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func TestKubernetesEnablesTorByDefault(t *testing.T) {
	t.Parallel()

	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `yaml:"name"`
						Image string `yaml:"image"`
						Ports []any  `yaml:"ports"`
						Env   []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "deploy/k8s/deployment.yaml")), &dep); err != nil {
		t.Fatalf("deployment.yaml: %v", err)
	}

	var app, tor *struct {
		Name  string `yaml:"name"`
		Image string `yaml:"image"`
		Ports []any  `yaml:"ports"`
		Env   []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
	}
	for i := range dep.Spec.Template.Spec.Containers {
		switch dep.Spec.Template.Spec.Containers[i].Name {
		case "netweather":
			app = &dep.Spec.Template.Spec.Containers[i]
		case "tor":
			tor = &dep.Spec.Template.Spec.Containers[i]
		}
	}
	if app == nil || tor == nil {
		t.Fatalf("containers = %v, want netweather and tor", dep.Spec.Template.Spec.Containers)
	}
	env := map[string]string{}
	for _, e := range app.Env {
		env[e.Name] = e.Value
	}
	if env["NETWEATHER_TOR"] != "true" {
		t.Fatalf("NETWEATHER_TOR = %q, want true", env["NETWEATHER_TOR"])
	}
	if env["NETWEATHER_TOR_SOCKS"] != "127.0.0.1:9050" {
		t.Fatalf("NETWEATHER_TOR_SOCKS = %q, want the loopback sidecar", env["NETWEATHER_TOR_SOCKS"])
	}
	if !strings.Contains(tor.Image, "netweather-tor") {
		t.Fatalf("tor image = %q, want netweather-tor", tor.Image)
	}
	if len(tor.Ports) != 0 {
		t.Fatalf("tor ports = %v, want none", tor.Ports)
	}
}

func TestHelmEnablesTorByDefault(t *testing.T) {
	t.Parallel()

	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
		Tor struct {
			Enabled bool `yaml:"enabled"`
			Image   struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
			} `yaml:"image"`
			Resources map[string]any `yaml:"resources"`
		} `yaml:"tor"`
		Resources map[string]any `yaml:"resources"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "deploy/helm/netweather/values.yaml")), &values); err != nil {
		t.Fatalf("values.yaml: %v", err)
	}
	if !values.Tor.Enabled {
		t.Fatal("tor.enabled = false, want the default install to enable Tor")
	}
	if values.Tor.Image.Repository == "" && values.Image.Repository == "" {
		t.Fatal("both tor and main image repositories are empty")
	}
	if len(values.Tor.Resources) == 0 || len(values.Resources) == 0 {
		t.Fatal("main and tor resources must both have defaults")
	}

	tmpl := readRepoFile(t, "deploy/helm/netweather/templates/deployment.yaml")
	for _, want := range []string{"if .Values.tor.enabled", "NETWEATHER_TOR_SOCKS", "name: tor", "printf \"%s-tor\""} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("Helm deployment template lacks %q", want)
		}
	}
}

func TestCloudInitDerivesTheTorImageTag(t *testing.T) {
	t.Parallel()

	script := readRepoFile(t, "deploy/azure/terraform/cloud_init.yaml.tftpl")
	for _, want := range []string{"netweather-tor:$(printf", "ghcr.io/OWNER/netweather-tor:latest|$tor_image"} {
		if !strings.Contains(script, want) {
			t.Fatalf("cloud init lacks %q", want)
		}
	}
}
