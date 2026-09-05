import { describe, expect, test } from "bun:test"
import { DisconnectError, disconnectFern, OnboardingLatch, pollForAuthorization } from "../src/auth.js"
import { FernClient, FernClientError, type AuthorizationStart } from "../src/client.js"
import { InMemoryCredentialStore } from "../src/credentials.js"

const deviceCode = "A".repeat(43)
const authorizationID = `pa_${"A".repeat(22)}`
const credentialID = `pc_${"A".repeat(22)}`
const scopes = ["run:create", "run:read", "run:stop", "run:attach", "run:result"]

describe("plugin authorization client", () => {
  test("starts without bearer and validates the complete bounded identity", async () => {
    let authorization: string | null = "not-called"
    const client = authClient((request) => {
      authorization = request.headers.get("authorization")
      return Response.json(startResponse(), { status: 201 })
    })
    const started = await client.startAuthorization()
    expect(authorization).toBeNull()
    expect(started).toMatchObject({ authorizationID, deviceCode, userCode: "ABCDE-FGHIJ-KLM", interval: 5 })

    const extra = authClient(() => Response.json({ ...startResponse(), extra: true }, { status: 201 }))
    await expect(extra.startAuthorization()).rejects.toThrow("invalid authorization start")

    const wrongOrigin = authClient(() =>
      Response.json(
        { ...startResponse(), verification_uri_complete: "https://evil.example/authorize" },
        { status: 201 },
      ),
    )
    await expect(wrongOrigin.startAuthorization()).rejects.toThrow("unsafe authorization URL")
  })

  test("polls publicly and accepts only exact bearer, scopes, expiry, and device identity", async () => {
    let authorization: string | null = "not-called"
    const approved = authClient((request) => {
      authorization = request.headers.get("authorization")
      return Response.json({
        access_token: deviceCode,
        token_type: "Bearer",
        credential_id: credentialID,
        expires_in: 60,
        scopes,
      })
    })
    await expect(approved.pollAuthorization(deviceCode)).resolves.toEqual({
      status: "approved",
      accessToken: deviceCode,
      credentialID,
      expiresIn: 60,
    })
    expect(authorization).toBeNull()

    for (const invalid of [
      { access_token: "E".repeat(43), token_type: "Bearer", credential_id: credentialID, expires_in: 60, scopes },
      { access_token: deviceCode, token_type: "bearer", credential_id: credentialID, expires_in: 60, scopes },
      { access_token: deviceCode, token_type: "Bearer", credential_id: credentialID, expires_in: 0, scopes },
      {
        access_token: deviceCode,
        token_type: "Bearer",
        credential_id: credentialID,
        expires_in: 60,
        scopes: [...scopes].reverse(),
      },
    ]) {
      await expect(authClient(() => Response.json(invalid)).pollAuthorization(deviceCode)).rejects.toThrow(
        "invalid approved authorization",
      )
    }
  })

  test("parses pending outcomes and bounded Retry-After", async () => {
    await expect(
      authClient(() => Response.json({ status: "pending" }, { status: 202 })).pollAuthorization(deviceCode),
    ).resolves.toEqual({ status: "pending" })
    await expect(
      authClient(() => new Response("limited", { status: 429, headers: { "Retry-After": "9" } })).pollAuthorization(
        deviceCode,
      ),
    ).resolves.toEqual({ status: "pending", retryAfter: 9 })
    await expect(
      authClient(() => new Response("limited", { status: 429, headers: { "Retry-After": "9999" } })).pollAuthorization(
        deviceCode,
      ),
    ).rejects.toThrow("retry interval")
  })

  test("accepts same-origin loopback HTTP authorization URLs for development", async () => {
    const response = startResponse()
    response.verification_uri = "http://127.0.0.1:8080/fern/plugin-auth/authorize"
    response.verification_uri_complete =
      "http://127.0.0.1:8080/fern/plugin-auth/authorize?code=ABCDE-FGHIJ-KLM&id=" + authorizationID
    const client = new FernClient(new URL("http://127.0.0.1:8080/"), new InMemoryCredentialStore(), {
      fetch: (async () => Response.json(response, { status: 201 })) as typeof fetch,
    })
    await expect(client.startAuthorization()).resolves.toMatchObject({ userCode: "ABCDE-FGHIJ-KLM" })
  })
})

describe("authorization polling", () => {
  test("waits for the server interval and honors a longer Retry-After", async () => {
    let now = 0
    const sleeps: number[] = []
    const outcomes = [
      { status: "pending" as const, retryAfter: 9 },
      { status: "pending" as const },
      { status: "approved" as const, accessToken: deviceCode, credentialID, expiresIn: 60 },
    ]
    const result = await pollForAuthorization({ pollAuthorization: async () => outcomes.shift()! }, started(), {
      now: () => now,
      sleep: async (milliseconds) => {
        sleeps.push(milliseconds)
        now += milliseconds
      },
    })
    expect(result.status).toBe("approved")
    expect(sleeps).toEqual([5_000, 9_000, 5_000])
  })

  test("reports denial, expiry, and lifecycle cancellation", async () => {
    const immediate = { now: () => 0, sleep: async () => {} }
    await expect(
      pollForAuthorization({ pollAuthorization: async () => ({ status: "denied" }) }, started(), immediate),
    ).rejects.toThrow("denied")
    await expect(
      pollForAuthorization({ pollAuthorization: async () => ({ status: "expired" }) }, started(), immediate),
    ).rejects.toThrow("expired")

    const controller = new AbortController()
    controller.abort()
    await expect(
      pollForAuthorization({ pollAuthorization: async () => ({ status: "pending" }) }, started(), {
        signal: controller.signal,
      }),
    ).rejects.toThrow("canceled")
  })
})

test("disconnect classifies revoke outcomes and owns exactly one local deletion", async () => {
  const cases = [
    { expected: "revoked", revoke: async () => {} },
    {
      expected: "already_ineffective",
      revoke: async () => {
        throw new FernClientError("ineffective", 401, "authentication", true)
      },
    },
    {
      expected: "definitive_failure",
      revoke: async () => {
        throw new FernClientError("rejected", 503, "http")
      },
    },
    {
      expected: "ambiguous_transport",
      revoke: async () => {
        throw new FernClientError("response lost", undefined, "transport")
      },
    },
  ] as const
  for (const item of cases) {
    let deletes = 0
    const credentials = {
      get: async () => deviceCode,
      set: async () => {},
      delete: async () => {
        deletes++
      },
    }
    await expect(disconnectFern({ revokeSelf: item.revoke }, credentials)).resolves.toBe(item.expected)
    expect(deletes).toBe(1)
  }

  const failedDelete = disconnectFern(
    { revokeSelf: async () => {} },
    { get: async () => deviceCode, set: async () => {}, delete: async () => Promise.reject(new Error("keyring")) },
  ).catch((error: unknown) => error)
  await expect(failedDelete).resolves.toBeInstanceOf(DisconnectError)
  await expect(failedDelete).resolves.toMatchObject({ outcome: "revoked" })
})

test("OnboardingLatch shares one in-flight authorization and resets afterward", async () => {
  const latch = new OnboardingLatch()
  let calls = 0
  let release = () => {}
  const action = () => {
    calls++
    return new Promise<void>((resolve) => (release = resolve))
  }
  const first = latch.run(action)
  const second = latch.run(action)
  expect(calls).toBe(1)
  release()
  await Promise.all([first, second])
  const third = latch.run(async () => {
    calls++
  })
  await third
  expect(calls).toBe(2)
})

function authClient(handler: (request: Request) => Response) {
  return new FernClient(new URL("https://fern.example/"), new InMemoryCredentialStore("local-secret"), {
    fetch: (async (input: RequestInfo | URL, init?: RequestInit) => handler(new Request(input, init))) as typeof fetch,
  })
}

function startResponse() {
  return {
    authorization_id: authorizationID,
    device_code: deviceCode,
    user_code: "ABCDE-FGHIJ-KLM",
    verification_uri: "https://fern.example/fern/plugin-auth/authorize",
    verification_uri_complete:
      "https://fern.example/fern/plugin-auth/authorize?code=ABCDE-FGHIJ-KLM&id=" + authorizationID,
    expires_in: 600,
    interval: 5,
    scopes,
  }
}

function started(): AuthorizationStart {
  return {
    authorizationID,
    deviceCode,
    userCode: "ABCDE-FGHIJ-KLM",
    verificationURI: new URL("https://fern.example/fern/plugin-auth/authorize"),
    verificationURIComplete: new URL(startResponse().verification_uri_complete),
    expiresIn: 600,
    interval: 5,
  }
}
