# Serial Background Run Integration

`run.sh` executes a provider/store/coordinator integration harness, not a full
`fern up` startup. It includes the qualified source-profile serial
`taskstore` + `backgroundruncoord` scenario and uses the local fake
model provider, injects one lost prompt response after the server effect,
recreates the store and coordinator, proves one prompt/provider call, observes
positive active work, commits stop, and verifies exact cleanup and terminal
parent consistency without a shell or paid model. It also crashes after the
durable prompt fence but before dispatch, restarts read-only with zero POSTs,
replaces the exact container process epoch, and proves the run becomes
cleanup-required. The replacement is fenced with 502 before coordinator
reconciliation, then the listener becomes an authenticated no-store 404. The
harness starts the concrete route on a fixed loopback address,
GETs the official root and authenticated health with a durable paired-device
cookie, rejects missing/wrong device credentials, reconstructs the unbound
listener and exact route after coordinator restart, and proves a response-loss
style durable `open` replay returns the same secret-free projection. Residue
checks use exact run labels and inspect the
private clone root for stage, quarantine, and marker remnants.
