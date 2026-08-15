package main

import (
	"os"
	"strings"
	"testing"
)

func TestValidateDockerTopology(t *testing.T) {
	tests := []struct {
		name string
		host *string
		want string
	}{
		{name: "unset"},
		{name: "empty", host: dockerHost("")},
		{name: "default Unix socket", host: dockerHost("unix:///var/run/docker.sock")},
		{name: "macOS user Unix socket", host: dockerHost("unix:///Users/test/.docker/run/docker.sock")},
		{name: "TCP", host: dockerHost("tcp://docker.example.com:2376"), want: "only local Unix socket endpoints are supported"},
		{name: "HTTP", host: dockerHost("http://docker.example.com:2375"), want: "only local Unix socket endpoints are supported"},
		{name: "HTTPS", host: dockerHost("https://docker.example.com:2376"), want: "only local Unix socket endpoints are supported"},
		{name: "SSH", host: dockerHost("ssh://docker.example.com"), want: "only local Unix socket endpoints are supported"},
		{name: "named pipe", host: dockerHost("npipe:////./pipe/docker_engine"), want: "only local Unix socket endpoints are supported"},
		{name: "malformed", host: dockerHost("docker.example.com:2376"), want: "unable to parse docker host"},
		{name: "missing endpoint", host: dockerHost("unix://"), want: "unable to parse docker host"},
		{name: "relative Unix socket", host: dockerHost("unix://docker.sock"), want: "Unix socket path must be absolute"},
		{name: "Unix URL with hostname", host: dockerHost("unix://localhost/var/run/docker.sock"), want: "Unix socket path must be absolute"},
		{name: "localhost over TCP", host: dockerHost("tcp://localhost:2375"), want: "only local Unix socket endpoints are supported"},
		{name: "loopback over HTTPS", host: dockerHost("https://127.0.0.1:2376"), want: "only local Unix socket endpoints are supported"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.host == nil {
				original, wasSet := os.LookupEnv("DOCKER_HOST")
				if err := os.Unsetenv("DOCKER_HOST"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if wasSet {
						if err := os.Setenv("DOCKER_HOST", original); err != nil {
							t.Errorf("restore DOCKER_HOST: %v", err)
						}
						return
					}
					if err := os.Unsetenv("DOCKER_HOST"); err != nil {
						t.Errorf("restore DOCKER_HOST: %v", err)
					}
				})
			} else {
				t.Setenv("DOCKER_HOST", *test.host)
			}

			err := validateDockerTopology()
			if test.want == "" {
				if err != nil {
					t.Fatalf("validateDockerTopology() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateDockerTopology() accepted unsupported Docker host")
			}
			for _, want := range []string{test.want, "bind mounts", "loopback publication", "host-local coordination"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func dockerHost(value string) *string {
	return &value
}
