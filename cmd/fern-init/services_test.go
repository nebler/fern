package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestParseServices(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    map[string]service
		wantErr bool
	}{
		{
			name: "full",
			yaml: `
services:
  db:
    run: postgres -D .pg
    stop: pg_ctl stop -D .pg
    ready: pg_isready
    port: 5432
    env:
      PGDATA: .pg
  web:
    run: npm start
`,
			want: map[string]service{
				"db": {
					Run: "postgres -D .pg", Stop: "pg_ctl stop -D .pg", Ready: "pg_isready",
					Port: 5432, Env: map[string]string{"PGDATA": ".pg"},
				},
				"web": {Run: "npm start"},
			},
		},
		{name: "empty file", yaml: ""},
		{name: "no services", yaml: "services: {}\n"},
		{name: "missing run", yaml: "services:\n  db:\n    port: 5432\n", wantErr: true},
		{name: "unknown field", yaml: "services:\n  db:\n    run: x\n    restart: always\n", wantErr: true},
		{name: "unknown top-level field", yaml: "version: 1\nservices: {}\n", wantErr: true},
		{name: "bad port", yaml: "services:\n  db:\n    run: x\n    port: high\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServices([]byte(tt.yaml))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parseServices(%q) error = %v, want error: %t", tt.yaml, err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parseServices(%q) mismatch (-want +got):\n%s", tt.yaml, diff)
			}
		})
	}
}

func TestServiceEnviron(t *testing.T) {
	tests := []struct {
		name string
		svc  service
		want []string
	}{
		{name: "nothing added", svc: service{Run: "x"}, want: []string{"PATH=/bin"}},
		{
			name: "port and env",
			svc:  service{Run: "x", Port: 8080, Env: map[string]string{"A": "1", "B": "2"}},
			want: []string{"PATH=/bin", "PORT=8080", "A=1", "B=2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Spare capacity would let an append write into the caller's array.
			base := make([]string, 1, 8)
			base[0] = "PATH=/bin"
			got := tt.svc.environ(base)
			// Env comes from a map, so its order is not defined.
			sortEnv := cmpopts.SortSlices(func(a, b string) bool { return a < b })
			if diff := cmp.Diff(tt.want, got, sortEnv); diff != "" {
				t.Errorf("environ() mismatch (-want +got):\n%s", diff)
			}
			if got := base[:cap(base)][1]; got != "" {
				t.Errorf("environ() wrote %q into the caller's slice", got)
			}
		})
	}
}
