import type { TuiDialogStack, TuiPluginApi, TuiPluginModule } from "@opencode-ai/plugin/tui"
import { FernClient, parseEndpoint, type RunResult } from "./client.js"
import { InMemoryCredentialStore } from "./credentials.js"
import { subprocessEnvironment } from "./process.js"
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

    const credentials = new InMemoryCredentialStore(process.env.FERN_TOKEN)
    const endpoint = process.env.FERN_ENDPOINT
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
    const client = () => {
      if (!endpoint)
        throw new Error("Set FERN_ENDPOINT for this OpenCode process. Fern never persists endpoint authority.")
      return new FernClient(parseEndpoint(endpoint), credentials, { signal: api.lifecycle.signal })
    }
    const run = () =>
      showPrompt(
        api,
        "Run on Fern",
        `Instruction (${INSTRUCTION_MAX_LENGTH} characters maximum)`,
        (instruction, dialog) =>
          createLatch.run(() => submitRun(api, client(), endpoint!, pending, instruction, dialog)),
      )
    const runs = () =>
      handle(api, async () => {
        const items = await client().listRuns()
        api.ui.dialog.setSize("xlarge")
        api.ui.dialog.replace(() =>
          api.ui.DialogSelect({
            title: "Fern runs",
            options: items.map((item) => ({
              title: item.id,
              value: item.id,
              description: `${item.state}${item.repository ? ` - ${item.repository}` : ""}`,
            })),
            onSelect: (item) => void handle(api, () => showRun(api, client(), String(item.value))),
          }),
        )
      })
    const open = () => showPrompt(api, "Open Fern run", "Run ID", (runID) => openRun(api, client(), runID))
    const stop = () =>
      showPrompt(api, "Stop Fern run", "Run ID", (runID, dialog) => stopRun(api, client(), runID, dialog))
    const result = () => showPrompt(api, "Fern run result", "Run ID", (runID) => showResult(api, client(), runID))
    const disconnect = () =>
      handle(api, async () => {
        const approved = await confirm(
          api,
          api.ui.dialog,
          "Disconnect Fern?",
          "Forget this plugin's in-memory credential? The inherited FERN_TOKEN environment variable cannot be removed and can reconnect after reload.",
        )
        if (!approved) return
        await credentials.delete()
        api.ui.toast({
          variant: "success",
          title: "Fern credential forgotten",
          message: "Removed from plugin memory; inherited FERN_TOKEN is unchanged.",
        })
      })
    const menu = () => {
      api.ui.dialog.setSize("xlarge")
      api.ui.dialog.replace(() =>
        api.ui.DialogSelect({
          title: "Fern",
          options: [
            {
              title: "Run",
              value: "run",
              description: "Start a background run",
              onSelect: run,
            },
            {
              title: "Runs",
              value: "runs",
              description: "List background runs",
              onSelect: runs,
            },
            {
              title: "Open",
              value: "open",
              description: "Open the authoritative live run",
              onSelect: open,
            },
            {
              title: "Stop",
              value: "stop",
              description: "Request a durable stop",
              onSelect: stop,
            },
            {
              title: "Result",
              value: "result",
              description: "Read the retained result",
              onSelect: result,
            },
            {
              title: "Disconnect",
              value: "disconnect",
              description: "Forget this process credential",
              onSelect: disconnect,
            },
          ],
        }),
      )
    }

    api.keymap.registerLayer({
      commands: [
        command("fern", "Fern", "Open Fern actions", menu, "fern"),
        command("fern.run", "Fern: Run", "Start a background run", run),
        command("fern.runs", "Fern: Runs", "List background runs", runs),
        command("fern.open", "Fern: Open", "Open a background run", open),
        command("fern.stop", "Fern: Stop", "Stop a background run", stop),
        command("fern.result", "Fern: Result", "Show a background run result", result),
        command("fern.disconnect", "Fern: Disconnect", "Forget the in-memory credential", disconnect),
      ],
    })
  },
}

export default plugin

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
  const command =
    process.platform === "darwin"
      ? ["open", url.href]
      : process.platform === "win32"
        ? ["cmd", "/c", "start", "", url.href]
        : ["xdg-open", url.href]
  try {
    const processResult = Bun.spawn(command, {
      env: subprocessEnvironment(),
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
    })
    if ((await processResult.exited) !== 0) throw new Error()
  } catch {
    throw new Error("Could not open the Fern run in the system browser.")
  }
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
