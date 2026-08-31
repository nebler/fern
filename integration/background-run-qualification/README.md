# Background Run Qualification

This is the authoritative local, real-Docker qualification suite for the
source-commit Background Run profile. Run it from any directory:

```sh
integration/background-run-qualification/run.sh
```

The suite builds and runs the source contract, captures the exact canonical
local image ID, reruns the contract with builds disabled and that ID required,
then runs the disposable provider lifecycle and serial OpenCode/coordinator
harnesses with the same ID. Harness cleanup fails on exact container, volume,
clone, network, or temporary-path residue. The fake provider is local and
zero-cost; no provider secret or paid model is used.

This suite does not run the separate published `opencode-ai@1.18.16` negative
contract. A passing local image ID is build-local evidence, not a registry
digest, publication, promotion, Tailscale, or physical-device evidence.
