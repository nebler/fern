import type { TuiDialogStack, TuiPluginApi, TuiPluginModule } from "@opencode-ai/plugin/tui"
import { disconnectFern, OnboardingLatch, pollForAuthorization, revokeFern, type RevokeOutcome } from "./auth.js"
import { FernClient, parseEndpoint, type RunResult } from "./client.js"
import { InMemoryCredentialStore, OSCredentialStore, type CredentialStore } from "./credentials.js"
import { configuredOrigin, persistOrigin } from "./origin.js"
import { runProcess, subprocessEnvironment } from "./process.js"
import {
  CreateRunLatch,
  INSTRUCTION_MAX_LENGTH,
  SUPPORTED_OPENCODE_VERSION,
  createRunWorkflow,
  stopRunWorkflow,
  type PendingSubmissionStore,
  type RunConfirmation,
} from "./workflow.js"

const plugin: TuiPluginModule = {
  id: "fern.opencode",
  async tui(api) {
    if (api.app.version !== SUPPORTED_OPENCODE_VERSION) {
      api.ui.toast({
        variant: "error",
        title: "Fern plugin disabled",
        message: `Requires OpenCode ${SUPPORTED_OPENCODE_VERSION}; found ${api.app.version}.`,
        duration: 10_000,
      })
      return
    }

    let endpoint = configuredOrigin(api.kv)
    let credentials: CredentialStore | undefined
    const developmentToken = process.env.FERN_TOKEN
    const pending: PendingSubmissionStore = {
      async get(digest) {
        return api.kv.get<Record<string, string>>("fern.pending-submissions", {})[digest]
      },
      async set(digest, idempotencyKey) {
        api.kv.set("fern.pending-submissions", {
          ...api.kv.get<Record<string, string>>("fern.pending-submissions", {}),
          [digest]: idempotencyKey,
        })
      },
      async delete(digest) {
        const next = { ...api.kv.get<Record<string, string>>("fern.pending-submissions", {}) }
        delete next[digest]
        api.kv.set("fern.pending-submissions", next)
      },
    }
    const createLatch = new CreateRunLatch()
    const onboardingLatch = new OnboardingLatch()
    const connection = async (onboard = true) => {
      if (!endpoint) {
        const input = await promptValue(api, "Connect Fern", "Root HTTPS origin, for example https://fern.example")
        if (input === undefined) throw new Error("Fern connection was canceled.")
        endpoint = persistOrigin(api.kv, input)
      }
      if (!credentials) {
        credentials = developmentToken
          ? new InMemoryCredentialStore(developmentToken)
          : new OSCredentialStore(endpoint.href, { signal: api.lifecycle.signal })
      }
      const client = new FernClient(endpoint, credentials, { signal: api.lifecycle.signal })
      if (!(await credentials.get()) && onboard) {
        await onboardingLatch.run(() => authorize(api, endpoint!, client, credentials!))
      }
      return { client, endpoint, credentials }
    }
    const run = async () => {
      const connected = await connection()
      return showPrompt(
        api,
        "Run on Fern",
        `Instruction (${INSTRUCTION_MAX_LENGTH} characters maximum)`,
        (instruction, dialog) =>
          createLatch.run(() =>
            submitRun(api, connected.client, connected.endpoint.href, pending, instruction, dialog),
          ),
      )
    }
    const runs = () =>
      handle(api, async () => {
        const { client } = await connection()
        const items = await client.listRuns()
        api.ui.dialog.setSize("xlarge")
        api.ui.dialog.replace(() =>
          api.ui.DialogSelect({
            title: "Fern runs",
            options: items.map((item) => ({
              title: item.id,
              value: item.id,
              description: `${item.state}${item.repository ? ` - ${item.repository}` : ""}`,
            })),
            onSelect: (item) => void handle(api, () => showRun(api, client, String(item.value))),
          }),
        )
      })
    const open = async () => {
      const { client } = await connection()
      return showPrompt(api, "Open Fern run", "Run ID", (runID) => openRun(api, client, runID))
    }
    const stop = async () => {
      const { client } = await connection()
      return showPrompt(api, "Stop Fern run", "Run ID", (runID, dialog) => stopRun(api, client, runID, dialog))
    }
    const result = async () => {
      const { client } = await connection()
      return showPrompt(api, "Fern run result", "Run ID", (runID) => showResult(api, client, runID))
    }
    const disconnect = () =>
      handle(api, async () => {
        const connected = await connection(false)
        if (!(await connected.credentials.get())) throw new Error("Fern has no local credential to disconnect.")
        const approved = await confirm(
          api,
          api.ui.dialog,
          "Disconnect Fern?",
          "Revoke this plugin credential on Fern, then remove it from the operating system keyring?",
        )
        if (!approved) return
        const outcome = await disconnectFern(connected.client, connected.credentials)
        api.ui.toast({
          variant:
            outcome === "revoked" || outcome === "already_ineffective"
              ? "success"
              : outcome === "definitive_failure"
                ? "error"
                : "warning",
          title: "Fern credential forgotten",
          message: disconnectMessage(outcome, Boolean(developmentToken)),
        })
      })
    const menu = async () => {
      await connection()
      api.ui.dialog.setSize("xlarge")
      api.ui.dialog.replace(() =>
        api.ui.DialogSelect({
          title: "Fern",
          options: [
            {
              title: "Run",
              value: "run",
              description: "Start a background run",
              onSelect: () => void handle(api, run),
            },
            {
              title: "Runs",
              value: "runs",
              description: "List background runs",
              onSelect: () => void handle(api, runs),
            },
            {
              title: "Open",
              value: "open",
              description: "Open the authoritative live run",
              onSelect: () => void handle(api, open),
            },
            {
              title: "Stop",
              value: "stop",
              description: "Request a durable stop",
              onSelect: () => void handle(api, stop),
            },
            {
              title: "Result",
              value: "result",
              description: "Read the retained result",
              onSelect: () => void handle(api, result),
            },
            {
              title: "Disconnect",
              value: "disconnect",
              description: "Revoke and forget this plugin credential",
              onSelect: () => void handle(api, disconnect),
            },
          ],
        }),
      )
    }

    api.keymap.registerLayer({
      commands: [
        command("fern", "Fern", "Open Fern actions", () => handle(api, menu), "fern"),
        command("fern.run", "Fern: Run", "Start a background run", () => handle(api, run)),
        command("fern.runs", "Fern: Runs", "List background runs", () => handle(api, runs)),
        command("fern.open", "Fern: Open", "Open a background run", () => handle(api, open)),
        command("fern.stop", "Fern: Stop", "Stop a background run", () => handle(api, stop)),
        command("fern.result", "Fern: Result", "Show a background run result", () => handle(api, result)),
        command("fern.disconnect", "Fern: Disconnect", "Revoke and forget the credential", () =>
          handle(api, disconnect),
        ),
      ],
    })
  },
}

export default plugin

export async function authorize(
  api: TuiPluginApi,
  endpoint: URL,
  client: FernClient,
  credentials: CredentialStore,
  options: {
    openBrowser?: typeof openBrowser
    poll?: typeof pollForAuthorization
    revokeApproved?: (credential: string) => Promise<RevokeOutcome>
  } = {},
) {
  if (credentials instanceof OSCredentialStore && !(await credentials.available())) {
    throw new Error(
      "Fern needs an available macOS Keychain or Linux Secret Service for durable credentials. No plaintext credential was saved.",
    )
  }

  const started = await client.startAuthorization()
  const controller = new AbortController()
  const cancel = () => controller.abort()
  let ownsDialog = true
  const closed = () => {
    ownsDialog = false
    cancel()
  }
  api.lifecycle.signal.addEventListener("abort", cancel, { once: true })
  api.ui.dialog.setSize("xlarge")
  api.ui.dialog.replace(
    () =>
      api.ui.DialogAlert({
        title: "Authorize Fern",
        message: `Confirm this code in your browser:\n\n${started.userCode}\n\nWaiting for authorization. Closing this dialog cancels onboarding.`,
        onConfirm: cancel,
      }),
    closed,
  )
  try {
    await (options.openBrowser ?? openBrowser)(started.verificationURIComplete, api.lifecycle.signal)
    const approved = await (options.poll ?? pollForAuthorization)(client, started, { signal: controller.signal })
    try {
      if (controller.signal.aborted || api.lifecycle.signal.aborted) {
        throw new Error("Fern authorization was canceled before its credential could be saved.")
      }
      await credentials.set(approved.accessToken)
      if ((await credentials.get()) !== approved.accessToken) {
        throw new Error("The operating system keyring did not retain the Fern credential.")
      }
    } catch (persistenceError) {
      const revoke = options.revokeApproved
        ? await options.revokeApproved(approved.accessToken)
        : await revokeFern(new FernClient(endpoint, new InMemoryCredentialStore(approved.accessToken)))
      let localRemoved = true
      try {
        await credentials.delete()
      } catch {
        localRemoved = false
      }
      throw new Error(persistenceFailureMessage(persistenceError, revoke, localRemoved))
    }
    api.ui.toast({
      variant: "success",
      title: "Fern connected",
      message:
        credentials instanceof InMemoryCredentialStore
          ? `Authorized ${started.verificationURI.host} in memory using the development-only FERN_TOKEN path.`
          : `Authorized ${started.verificationURI.host}. The credential is stored only in the operating system keyring.`,
    })
  } finally {
    api.lifecycle.signal.removeEventListener("abort", cancel)
    if (ownsDialog) {
      ownsDialog = false
      api.ui.dialog.clear()
    }
  }
}

export async function openBrowser(url: URL, signal?: AbortSignal, run: typeof runProcess = runProcess) {
  const command =
    process.platform === "darwin"
      ? ["open", url.href]
      : process.platform === "win32"
        ? ["cmd", "/c", "start", "", url.href]
        : ["xdg-open", url.href]
  try {
    const result = await run(command, {
      env: subprocessEnvironment(),
      signal,
      timeoutMs: 15_000,
      outputLimit: 1_024,
    })
    if (result.canceled) throw new Error("Fern browser launch was canceled.")
    if (result.timedOut) throw new Error("Fern browser launch timed out.")
    if (result.overflow) throw new Error("Fern browser launcher output exceeded the allowed size.")
    if (result.exitCode !== 0) throw new Error("Could not open Fern in the system browser.")
  } catch (error) {
    if (error instanceof Error && error.message.startsWith("Fern browser")) throw error
    if (error instanceof Error && error.message === "Could not open Fern in the system browser.") throw error
    throw new Error("Could not open Fern in the system browser.")
  }
}

function command(
  name: string,
  title: string,
  description: string,
  run: () => void | Promise<void>,
  slashName?: string,
) {
  return {
    namespace: "palette",
    name,
    title,
    desc: description,
    category: "Fern",
    suggested: true,
    slashName,
    run,
  }
}

function showPrompt(
  api: TuiPluginApi,
  title: string,
  placeholder: string,
  action: (value: string, dialog: TuiDialogStack) => void | Promise<void>,
) {
  api.ui.dialog.setSize("xlarge")
  api.ui.dialog.replace(() =>
    api.ui.DialogPrompt({
      title,
      placeholder,
      onConfirm: (value) => void handle(api, () => action(value, api.ui.dialog)),
    }),
  )
}

function promptValue(api: TuiPluginApi, title: string, placeholder: string) {
  api.ui.dialog.setSize("xlarge")
  return new Promise<string | undefined>((resolve) => {
    let settled = false
    const finish = (value?: string) => {
      if (settled) return
      settled = true
      resolve(value)
    }
    api.ui.dialog.replace(
      () => api.ui.DialogPrompt({ title, placeholder, onConfirm: finish, onCancel: () => finish() }),
      () => finish(),
    )
  })
}

async function submitRun(
  api: TuiPluginApi,
  client: FernClient,
  endpoint: string,
  pending: PendingSubmissionStore,
  instruction: string,
  dialog: TuiDialogStack,
) {
  const runID = await createRunWorkflow({
    client,
    pending,
    directory: api.state.path.worktree || api.state.path.directory,
    host: parseEndpoint(endpoint).host,
    instruction,
    confirm: (input) => confirmRun(api, dialog, input),
  })
  if (!runID) return
  dialog.clear()
  api.ui.toast({
    variant: "success",
    title: "Fern run committed",
    message: `Background run ${runID} was accepted.`,
  })
}

async function showRun(api: TuiPluginApi, client: FernClient, runID: string) {
  const run = await client.getRun(runID)
  await alert(
    api,
    api.ui.dialog,
    `Fern run ${run.id}`,
    `State: ${run.state}\nRepository: ${run.repository ?? "unknown"}\nBase: ${run.head ?? "unknown"}`,
  )
}

async function openRun(api: TuiPluginApi, client: FernClient, runID: string) {
  const expected = client.requireRunID(runID.trim())
  const url = await client.resolveOpen(expected, crypto.randomUUID())
  await openBrowser(url, api.lifecycle.signal)
  api.ui.dialog.clear()
  api.ui.toast({
    variant: "success",
    title: "Fern run opened",
    message: `Opened run ${expected}.`,
  })
}

async function stopRun(api: TuiPluginApi, client: FernClient, runID: string, dialog: TuiDialogStack) {
  const state = await stopRunWorkflow({
    client,
    runID,
    confirm: (id) =>
      confirm(
        api,
        dialog,
        `Stop Fern run ${id}?`,
        "Fern will durably request cancellation. Active work may take time to fence.",
      ),
  })
  if (!state) return
  dialog.clear()
  api.ui.toast({
    variant: "success",
    title: "Fern stop requested",
    message: `Run ${runID.trim()} is ${state}.`,
  })
}

async function showResult(api: TuiPluginApi, client: FernClient, runID: string) {
  const result = await client.getResult(runID.trim())
  await alert(api, api.ui.dialog, `Fern result ${result.runID}`, formatResult(result))
}

function confirmRun(api: TuiPluginApi, dialog: TuiDialogStack, input: RunConfirmation) {
  return confirm(
    api,
    dialog,
    "Run on Fern?",
    `Host: ${input.host}\nRepository: ${input.git.remote}\nBase: ${input.git.head}\nBranch: ${input.git.branch ?? "detached HEAD"}\nWorking tree: clean\nRuntime: ${input.profile}\nPrompt: ${input.instruction}`,
  )
}

function confirm(api: TuiPluginApi, dialog: TuiDialogStack, title: string, message: string) {
  dialog.setSize("xlarge")
  return new Promise<boolean>((resolve) => {
    let settled = false
    const finish = (value: boolean) => {
      if (settled) return
      settled = true
      resolve(value)
    }
    dialog.replace(
      () =>
        api.ui.DialogConfirm({
          title,
          message,
          onConfirm: () => finish(true),
          onCancel: () => finish(false),
        }),
      () => finish(false),
    )
  })
}

function alert(api: TuiPluginApi, dialog: TuiDialogStack, title: string, message: string) {
  dialog.setSize("xlarge")
  return new Promise<void>((resolve) => {
    dialog.replace(() => api.ui.DialogAlert({ title, message, onConfirm: resolve }), resolve)
  })
}

async function handle(api: TuiPluginApi, action: () => void | Promise<void>) {
  try {
    await action()
  } catch (error) {
    api.ui.toast({
      variant: "error",
      title: "Fern",
      message: error instanceof Error ? error.message : String(error),
    })
  }
}

function formatResult(result: RunResult) {
  return [`State: ${result.state}`, result.resultCommit ? `Commit: ${result.resultCommit}` : undefined, result.summary]
    .filter((line): line is string => Boolean(line))
    .join("\n")
}

function disconnectMessage(outcome: RevokeOutcome, developmentToken: boolean) {
  const local = developmentToken
    ? "Removed from plugin memory; the inherited development-only FERN_TOKEN remains in the parent environment."
    : "Removed from the operating system keyring."
  if (outcome === "revoked") return `Revoked on Fern. ${local}`
  if (outcome === "already_ineffective") return `Fern identified the credential as already ineffective. ${local}`
  if (outcome === "definitive_failure") {
    return `Fern definitively rejected the revocation request. ${local} The server grant may remain active and needs operator revocation.`
  }
  return `${local} Fern's revocation response was lost, so the server grant may or may not have been revoked.`
}

function persistenceFailureMessage(_error: unknown, revoke: RevokeOutcome, localRemoved: boolean) {
  const local = localRemoved
    ? "No credential was retained locally."
    : "Local keyring cleanup also failed; inspect the operating system keyring."
  if (revoke === "revoked") return `The Fern credential could not be persisted, and the new grant was revoked. ${local}`
  if (revoke === "already_ineffective") {
    return `The Fern credential could not be persisted, and the new grant is already ineffective. ${local}`
  }
  if (revoke === "definitive_failure") {
    return `The Fern credential could not be persisted, and Fern definitively rejected cleanup. ${local} The new grant may remain active and needs operator revocation.`
  }
  return `The Fern credential could not be persisted. ${local} The cleanup response was lost, so the new grant may or may not remain active.`
}
