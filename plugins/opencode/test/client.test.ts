import { afterEach, describe, expect, test } from "bun:test"
import { FernClient, FernClientError, parseEndpoint } from "../src/client.js"
import { InMemoryCredentialStore } from "../src/credentials.js"

const servers: Array<{ stop(closeActiveConnections?: boolean): void }> = []

afterEach(() => {
  servers.splice(0).forEach((server) => server.stop(true))
})

describe("FernClient", () => {
  test("sends the exact create payload, bearer credential, and caller idempotency key", async () => {
    const capture: { authorization: string | null; idempotencyKey: string | null; body: unknown } = {
      authorization: null,
      idempotencyKey: null,
      body: undefined,
    }
    const server = serve(async (incoming) => {
      capture.authorization = incoming.headers.get("authorization")
      capture.idempotencyKey = incoming.headers.get("idempotency-key")
      capture.body = await incoming.json()
      return Response.json({ run_id: "run_123", committed: true }, { status: 202 })
    })
    const runID = await client(server).createRun({
      instruction: "Fix the race",
      profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6",
      git: gitContext(),
      idempotencyKey: "caller-key",
    })

    expect(runID).toBe("run_123")
    expect(capture.authorization).toBe("Bearer secret")
    expect(capture.idempotencyKey).toBe("caller-key")
    expect(capture.body).toEqual({
      repository: "https://github.com/fern/repo",
      base_oid: "a".repeat(40),
      branch: "main",
      instruction: "Fix the race",
      profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6",
    })
  })

  test("requires an explicitly committed run ID", async () => {
    const server = serve(() => Response.json({ run_id: "run_123", committed: false }, { status: 202 }))
    await expect(
      client(server).createRun({
        instruction: "Fix it",
        profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6",
        git: gitContext(),
        idempotencyKey: "caller-key",
      }),
    ).rejects.toThrow("committed run ID")
  })

  test("binds run identity, states, and optional fields", async () => {
    const wrongID = serve(() => Response.json({ id: "run_other", state: "queued" }))
    await expect(client(wrongID).getRun("run_123")).rejects.toThrow("wrong identity")

    const unknownState = serve(() => Response.json({ id: "run_123", state: "complete" }))
    await expect(client(unknownState).getRun("run_123")).rejects.toThrow("unknown run state")

    const malformedOptional = serve(() => Response.json({ id: "run_123", state: "working", repository: 42 }))
    await expect(client(malformedOptional).getRun("run_123")).rejects.toThrow("repository")

    const valid = serve(() => Response.json({ id: "run_123", state: "needs_you", head: "b".repeat(40), branch: null }))
    await expect(client(valid).getRun("run_123")).resolves.toEqual({
      id: "run_123",
      state: "needs_you",
      head: "b".repeat(40),
      branch: null,
      repository: undefined,
    })
  })

  test("stops a run with the caller idempotency key", async () => {
    const capture: { method?: string; path?: string; key?: string | null; body?: string } = {}
    const server = serve(async (request) => {
      capture.method = request.method
      capture.path = new URL(request.url).pathname
      capture.key = request.headers.get("idempotency-key")
      capture.body = await request.text()
      return Response.json({ run_id: "run_123", state: "canceling" }, { status: 202 })
    })

    await expect(client(server).stopRun("run_123", "stop-key")).resolves.toBe("canceling")
    expect(capture).toEqual({
      method: "POST",
      path: "/fern/api/runs/run_123/stop",
      key: "stop-key",
      body: "{}",
    })
  })

  test("seals a run with exact empty JSON and committed authority", async () => {
    const capture: { method?: string; path?: string; key?: string | null; body?: string } = {}
    const server = serve(async (request) => {
      capture.method = request.method
      capture.path = new URL(request.url).pathname
      capture.key = request.headers.get("idempotency-key")
      capture.body = await request.text()
      return Response.json(sealResponse())
    })

    await expect(client(server).sealRun("run_123", "seal-key")).resolves.toEqual({
      runID: "run_123",
      state: "canceling",
      resultPhase: "seal_requested",
      sealRequestID: "slr_0198d34d-7007-7007-8007-000000000007",
      committed: true,
    })
    expect(capture).toEqual({
      method: "POST",
      path: "/fern/api/runs/run_123/seal",
      key: "seal-key",
      body: "{}",
    })

    const ready = serve(() => Response.json({ ...sealResponse(), state: "result_ready", result_phase: "ready" }))
    await expect(client(ready).sealRun("run_123", "new-key")).resolves.toMatchObject({
      state: "result_ready",
      resultPhase: "ready",
    })
  })

  test("rejects malformed seal authority", async () => {
    const malformed = [
      { ...sealResponse(), run_id: "run_other" },
      { ...sealResponse(), committed: false },
      { ...sealResponse(), seal_request_id: "slr_not-a-uuid" },
      { ...sealResponse(), state: "working" },
      { ...sealResponse(), result_phase: "exporting" },
      { ...sealResponse(), state: "result_ready" },
    ]
    for (const response of malformed) {
      const fern = new FernClient(new URL("http://127.0.0.1:1/"), new InMemoryCredentialStore("secret"), {
        fetch: async () => Response.json(response),
      })
      await expect(fern.sealRun("run_123", "seal-key")).rejects.toThrow("invalid seal response")
    }
  })

  test("does not replay a seal after ambiguous transport loss", async () => {
    let requests = 0
    const fern = new FernClient(new URL("http://127.0.0.1:1/"), new InMemoryCredentialStore("secret"), {
      fetch: (async () => {
        requests++
        throw new Error("response lost")
      }) as typeof fetch,
    })
    await expect(fern.sealRun("run_123", "one-shot-key")).rejects.toMatchObject({ kind: "transport" })
    expect(requests).toBe(1)
  })

  test("reads a ready retained result", async () => {
    const server = serve((request) => {
      expect(request.method).toBe("GET")
      expect(new URL(request.url).pathname).toBe("/fern/api/runs/run_123/result")
      return Response.json(resultResponse())
    })

    await expect(client(server).getResult("run_123")).resolves.toEqual({
      runID: "run_123",
      state: "result_ready",
      result: {
        id: "res_0198d34d-7007-7007-8007-000000000007",
        outcome: "changed",
        repository: "https://github.com/fern/repo",
        baseOID: "a".repeat(40),
        resultCommit: "b".repeat(40),
        treeOID: "c".repeat(40),
        manifestEntries: 2,
        manifestSHA256: "1".repeat(64),
      },
      artifact: {
        id: "art_0198d34d-7008-7008-8008-000000000008",
        format: "git_bundle_v1",
        sha256: "e".repeat(64),
        bundleSHA256: "f".repeat(64),
        bundleSize: 1234,
        manifestSHA256: "d".repeat(64),
      },
      retention: { verified: true, reconstructable: true },
      cleanup: { complete: false },
    })
  })

  test("fails closed on malformed retained result authority", async () => {
    const malformed: unknown[] = [
      { ...resultResponse(), state: "working" },
      { ...resultResponse(), result: { ...resultResponse().result, id: "res_bad" } },
      { ...resultResponse(), result: { ...resultResponse().result, repository: "https://github.com/fern/repo.git" } },
      { ...resultResponse(), result: { ...resultResponse().result, result_commit: "B".repeat(40) } },
      { ...resultResponse(), result: { ...resultResponse().result, manifest_entries: -1 } },
      { ...resultResponse(), artifact: { ...resultResponse().artifact, format: "zip" } },
      { ...resultResponse(), artifact: { ...resultResponse().artifact, bundle_sha256: "F".repeat(64) } },
      { ...resultResponse(), artifact: { ...resultResponse().artifact, bundle_size: -1 } },
      { ...resultResponse(), artifact: { ...resultResponse().artifact, manifest_sha256: "invalid" } },
      { ...resultResponse(), retention: { verified: "yes", reconstructable: true } },
      { ...resultResponse(), cleanup: { complete: null } },
      { ...resultResponse(), artifact: { ...resultResponse().artifact, download_url: "https://fern.example/a" } },
      { ...resultResponse(), result: { ...resultResponse().result, server_path: "/srv/result" } },
      { run_id: "run_123", state: "result_ready", summary: "legacy", url: "https://fern.example/result" },
    ]
    for (const response of malformed) {
      const fern = new FernClient(new URL("http://127.0.0.1:1/"), new InMemoryCredentialStore("secret"), {
        fetch: async () => Response.json(response),
      })
      await expect(fern.getResult("run_123")).rejects.toThrow(FernClientError)
    }
  })

  test("requires JSON, bounds bodies, and preserves malformed error status", async () => {
    const nonJSON = serve(() => new Response("ok", { headers: { "Content-Type": "text/plain" } }))
    await expect(client(nonJSON).listRuns()).rejects.toThrow("non-JSON")

    const oversized = serve(() => Response.json({ runs: [], padding: "x".repeat(200) }))
    await expect(client(oversized, { responseLimit: 64 }).listRuns()).rejects.toThrow("allowed size")

    const malformedError = serve(
      () => new Response("not-json", { status: 502, headers: { "Content-Type": "application/json" } }),
    )
    const error = client(malformedError)
      .listRuns()
      .catch((failure: unknown) => failure)
    await expect(error).resolves.toMatchObject({ status: 502, message: "Fern request failed with HTTP 502." })
  })

  test("uses redirect:error, timeout, and lifecycle cancellation", async () => {
    let followed = false
    const redirect = serve((request) => {
      if (new URL(request.url).pathname === "/followed") {
        followed = true
        return Response.json({ runs: [] })
      }
      return Response.redirect(new URL("/followed", request.url), 302)
    })
    await expect(client(redirect).listRuns()).rejects.toThrow("valid response")
    expect(followed).toBe(false)

    const neverFetch = ((_input: RequestInfo | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        if (init?.signal?.aborted) {
          reject(new Error("aborted"))
          return
        }
        init?.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true })
      })) as typeof fetch
    const endpoint = new URL("http://127.0.0.1:1/")
    await expect(
      new FernClient(endpoint, new InMemoryCredentialStore("secret"), { fetch: neverFetch, timeoutMs: 5 }).listRuns(),
    ).rejects.toThrow("timed out")

    const controller = new AbortController()
    const canceled = new FernClient(endpoint, new InMemoryCredentialStore("secret"), {
      fetch: neverFetch,
      signal: controller.signal,
    }).listRuns()
    controller.abort()
    await expect(canceled).rejects.toThrow("shutting down")

    const hangingBody = ((_input: RequestInfo | URL, init?: RequestInit) =>
      Promise.resolve(
        new Response(
          new ReadableStream({
            start(stream) {
              stream.enqueue(new TextEncoder().encode('{"runs":'))
              init?.signal?.addEventListener("abort", () => stream.error(new Error("aborted")), { once: true })
            },
          }),
          { headers: { "Content-Type": "application/json" } },
        ),
      )) as typeof fetch
    await expect(
      new FernClient(endpoint, new InMemoryCredentialStore("secret"), {
        fetch: hangingBody,
        timeoutMs: 5,
      }).listRuns(),
    ).rejects.toThrow("timed out")
  })

  test("validates endpoint in the constructor and handles IPv6 localhost", () => {
    expect(() => new FernClient(new URL("https://fern.example/path"), new InMemoryCredentialStore("secret"))).toThrow(
      "root HTTPS origin",
    )
    expect(parseEndpoint("http://[::1]:8080/").hostname).toBe("[::1]")
    expect(() => parseEndpoint("https://fern.example/?query=1")).toThrow(FernClientError)
  })

  test("rejects missing credentials and validates run IDs before requests", async () => {
    const server = serve(() => Response.json({ runs: [] }))
    await expect(new FernClient(new URL(server.url), new InMemoryCredentialStore()).listRuns()).rejects.toThrow(
      "not connected",
    )
    expect(() => client(server).requireRunID("x")).toThrow("valid Fern run ID")
  })

  test("forgets a rejected credential without replaying a mutation", async () => {
    let requests = 0
    const server = serve(() => {
      requests++
      return Response.json(
        { error: "do not expose remote text" },
        { status: 401, headers: { "WWW-Authenticate": 'Bearer realm="fern-plugin"' } },
      )
    })
    const credentials = new InMemoryCredentialStore("secret")
    const fern = new FernClient(new URL(server.url), credentials)
    await expect(
      fern.createRun({
        instruction: "Fix it",
        profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6",
        git: gitContext(),
        idempotencyKey: "one-attempt",
      }),
    ).rejects.toThrow("rejected its plugin credential")
    expect(requests).toBe(1)
    expect(await credentials.get()).toBeUndefined()
  })

  test("preserves non-Fern 401 credentials and the original auth error when deletion fails", async () => {
    const unrelated = serve(() =>
      Response.json({}, { status: 401, headers: { "WWW-Authenticate": 'Basic realm="operator"' } }),
    )
    const preserved = new InMemoryCredentialStore("secret")
    await expect(new FernClient(new URL(unrelated.url), preserved).listRuns()).rejects.toMatchObject({
      status: 401,
      kind: "http",
    })
    expect(await preserved.get()).toBe("secret")

    const fern = serve(() =>
      Response.json({}, { status: 401, headers: { "WWW-Authenticate": 'Bearer realm="fern-plugin"' } }),
    )
    const failingDelete = {
      get: async () => "secret",
      set: async () => {},
      delete: async () => {
        throw new Error("keyring failure")
      },
    }
    await expect(new FernClient(new URL(fern.url), failingDelete).listRuns()).rejects.toMatchObject({
      status: 401,
      kind: "authentication",
      fernBearerChallenge: true,
      message: "Fern rejected its plugin credential. Reconnect on the next explicit Fern action.",
    })

    const revokeCredentials = new InMemoryCredentialStore("secret")
    await expect(new FernClient(new URL(fern.url), revokeCredentials).revokeSelf()).rejects.toMatchObject({
      kind: "authentication",
      fernBearerChallenge: true,
    })
    expect(await revokeCredentials.get()).toBe("secret")
  })
})

function gitContext() {
  return {
    root: "/repo",
    remote: "https://github.com/fern/repo",
    head: "a".repeat(40),
    branch: "main",
    dirty: false,
  }
}

function sealResponse() {
  return {
    run_id: "run_123",
    state: "canceling",
    result_phase: "seal_requested",
    seal_request_id: "slr_0198d34d-7007-7007-8007-000000000007",
    committed: true,
  }
}

function resultResponse() {
  return {
    run_id: "run_123",
    state: "result_ready",
    result: {
      id: "res_0198d34d-7007-7007-8007-000000000007",
      outcome: "changed",
      repository: "https://github.com/fern/repo",
      base_oid: "a".repeat(40),
      result_commit: "b".repeat(40),
      tree_oid: "c".repeat(40),
      manifest_entries: 2,
      manifest_sha256: "1".repeat(64),
      additive_server_field: "ignored",
    },
    artifact: {
      id: "art_0198d34d-7008-7008-8008-000000000008",
      format: "git_bundle_v1",
      sha256: "e".repeat(64),
      bundle_sha256: "f".repeat(64),
      bundle_size: 1234,
      manifest_sha256: "d".repeat(64),
    },
    retention: { verified: true, reconstructable: true },
    cleanup: { complete: false },
    additive_server_field: { ignored: true },
  }
}

function client(server: { url: URL }, options: ConstructorParameters<typeof FernClient>[2] = {}) {
  return new FernClient(new URL(server.url), new InMemoryCredentialStore("secret"), options)
}

function serve(fetch: (request: Request) => Response | Promise<Response>) {
  const server = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch })
  servers.push(server)
  return server
}
