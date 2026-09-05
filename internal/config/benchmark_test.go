package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func benchmarkConfigFixture(b *testing.B) (string, string, []byte) {
	b.Helper()
	repo := b.TempDir()
	data := []byte(fmt.Sprintf(`workspace:
  name: bench
  repo: %q
  github:
    mode: github-app-broker
    installationId: 123
    repository:
      id: 456
      fullName: owner/repository
tasks:
  agent: build
  model:
    provider: anthropic
    id: test-model
  attemptTimeout: 30m
  leaseDuration: 2m
  backgroundImage: fern/opencode-background-source:dev
  backgroundImageID: sha256:%s
  backgroundRoute:
    listen: 127.0.0.1:8443
    origin: https://bench.example.ts.net:8443
control:
  password: %q
proxy:
  listen: 127.0.0.1:8080
  operatorListen: 127.0.0.1:8081
  remoteOrigin: https://bench.example.ts.net
`, repo, strings.Repeat("a", 64), strings.Repeat("p", 32)))
	path := filepath.Join(b.TempDir(), "fern.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		b.Fatal(err)
	}
	cfg, err := Load(path, repo, true, Overrides{})
	if err != nil {
		b.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		b.Fatal(err)
	}
	return path, repo, data
}

func BenchmarkLoad(b *testing.B) {
	path, repo, _ := benchmarkConfigFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Load(path, repo, true, Overrides{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseTaskPolicy(b *testing.B) {
	_, _, data := benchmarkConfigFixture(b)
	var file fileConfig
	if err := decode(data, &file); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseTaskPolicy(file.Tasks); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	path, repo, _ := benchmarkConfigFixture(b)
	cfg, err := Load(path, repo, true, Overrides{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Validate(cfg); err != nil {
			b.Fatal(err)
		}
	}
}
