package main

import "testing"

// FuzzParseServices checks that whatever services.yaml holds (the file is
// the agent's), fern-init either refuses it or gets services it can run:
// each with a command, and a socket it can listen on, if any.
func FuzzParseServices(f *testing.F) {
	for _, seed := range []string{
		"services:\n  db:\n    run: postgres\n    ready: pg_isready\n    listen: unix:/tmp/.s.PGSQL.5432\n",
		"services:\n  web:\n    run: node server.js\n    listen: tcp:3000\n",
		"services:\n  x:\n    listen: tcp:\n",
		"services:\n  x:\n    run: a\n    listen: udp:53\n",
		"golden:\n  inputs: [seed.sql]\n",
		"services: [1, 2]",
		"{",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		services, err := parseServices(data)
		if err != nil {
			return
		}
		for name, svc := range services {
			if svc.Run == "" {
				t.Errorf("service %q has no run command", name)
			}
			if svc.Listen == "" {
				continue
			}
			if _, _, err := listenAddr(svc.Listen); err != nil {
				t.Errorf("service %q: accepted listen %q: %v", name, svc.Listen, err)
			}
		}
	})
}
