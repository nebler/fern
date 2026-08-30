export function subprocessEnvironment() {
  return Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !["FERN_TOKEN", "OPENCODE_FERN_TOKEN"].includes(name.toUpperCase())),
  )
}

export type ProcessResult = {
  exitCode: number
  stdout: string
  stderr: string
  timedOut: boolean
  canceled: boolean
  overflow: boolean
}

export async function runProcess(
  argv: readonly string[],
  options: {
    stdin?: string
    env?: Record<string, string | undefined>
    signal?: AbortSignal
    timeoutMs?: number
    outputLimit?: number
    cwd?: string
  } = {},
): Promise<ProcessResult> {
  const timeoutMs = options.timeoutMs ?? 10_000
  const outputLimit = options.outputLimit ?? 4_096
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0 || !Number.isSafeInteger(outputLimit) || outputLimit < 0) {
    throw new Error("Invalid subprocess safety bounds.")
  }

  let timedOut = false
  let canceled = options.signal?.aborted ?? false
  let overflow = false
  let stop = () => {}
  const cancel = () => {
    canceled = true
    stop()
  }
  const timeout = setTimeout(() => {
    timedOut = true
    stop()
  }, timeoutMs)
  options.signal?.addEventListener("abort", cancel, { once: true })

  try {
    if (canceled) return { exitCode: -1, stdout: "", stderr: "", timedOut, canceled, overflow }
    const child = Bun.spawn([...argv], {
      cwd: options.cwd,
      env: options.env ?? subprocessEnvironment(),
      stdin: options.stdin === undefined ? "ignore" : new Blob([options.stdin]),
      stdout: outputLimit === 0 ? "ignore" : "pipe",
      stderr: outputLimit === 0 ? "ignore" : "pipe",
    })
    stop = () => child.kill()
    const overflowed = () => {
      overflow = true
      stop()
    }
    const [exitCode, stdout, stderr] = await Promise.all([
      child.exited,
      child.stdout ? readOutput(child.stdout, outputLimit, overflowed) : "",
      child.stderr ? readOutput(child.stderr, outputLimit, overflowed) : "",
    ])
    return { exitCode, stdout, stderr, timedOut, canceled, overflow }
  } catch {
    return { exitCode: -1, stdout: "", stderr: "", timedOut, canceled, overflow }
  } finally {
    clearTimeout(timeout)
    options.signal?.removeEventListener("abort", cancel)
  }
}

async function readOutput(stream: ReadableStream<Uint8Array>, limit: number, overflowed: () => void) {
  const reader = stream.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  while (true) {
    const item = await reader.read()
    if (item.done) break
    total += item.value.byteLength
    if (total > limit) {
      overflowed()
      await reader.cancel()
      break
    }
    chunks.push(item.value)
  }
  const output = new Uint8Array(chunks.reduce((size, chunk) => size + chunk.byteLength, 0))
  let offset = 0
  for (const chunk of chunks) {
    output.set(chunk, offset)
    offset += chunk.byteLength
  }
  return new TextDecoder().decode(output)
}
