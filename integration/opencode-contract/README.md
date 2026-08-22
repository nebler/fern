# OpenCode Contract Harness

This directory is an isolated black-box harness for OpenCode
`0.0.0-next-17444`. It starts the pinned Docker image, talks only to public HTTP
routes, and supplies a local OpenAI-compatible fake provider. It does not use a
provider credential or make a paid model request.

Run from the repository root:

```sh
python3 integration/opencode-contract/contract_harness.py
```

The default image is `fern/opencode:dev`. Override it with
`FERN_OPENCODE_IMAGE`; the harness still refuses to proceed unless the image ID
is `sha256:839fd0bfffe57ec0b9095126ac682b0337f15a514dfaafdd9d18aa1bb86076ae`
and `/api/health` reports exactly `0.0.0-next-17444`.

The suite runs 13 named scenarios:

- `test_caller_ids_and_retry`
- `test_restart_retry_and_history`
- `test_history_and_event_cursors`
- `test_permission_surface`
- `test_permission_process_epochs`
- `test_undelivered_inbox_deletion`
- `test_interrupt_before_admission`
- `test_maximum_prompt_size`
- `test_resume_not_idempotency_bound`
- `test_interrupt_after_completion`
- `test_direct_form_process_epochs`
- `test_question_surface`
- `test_cancellation`

They characterize caller-selected IDs, response-loss retry and conflict, finite
message replay, event cursor behavior, undelivered inbox deletion, permission
and form process epochs, and interruption before, during, and after provider
work. `restart_same_container()` uses `docker restart`; container replacement
removes and recreates only OpenCode while retaining the temporary repository
and data mounts. The fake provider remains alive across both operations. The
suite prints separate proven and blocked contract lists because this pinned
release does not implement every durable event surface.

No scenario executes a shell command through a model tool. Direct synthetic
permission and form requests use public HTTP endpoints, and model turns use only
the local fake provider.

The run uses random container names, a random Docker network, and random host
ports. Its OpenCode and fake-provider containers, network, temporary repository,
and temporary OpenCode data directory are removed on success, failure, or
interruption.
