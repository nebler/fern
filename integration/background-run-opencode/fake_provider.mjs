#!/usr/bin/env node
import http from "node:http"

const port = Number.parseInt(process.argv[2], 10)
const state = { calls: 0, disconnects: 0, requests: [] }

function send(response, value) {
  response.write(`data: ${typeof value === "string" ? value : JSON.stringify(value)}\n\n`)
}

function hang(response, body, responses) {
  if (responses) {
    send(response, {
      type: "response.created",
      sequence_number: 1,
      response: { id: "resp_fern_background", created_at: Math.floor(Date.now() / 1000), model: body.model ?? "test-model", service_tier: null },
    })
  } else {
    send(response, { id: "chatcmpl-fern-background", object: "chat.completion.chunk", choices: [{ delta: { role: "assistant" } }] })
  }
  const timer = setInterval(() => response.write(": fern-background-heartbeat\n\n"), 100)
  let closed = false
  response.on("close", () => {
    if (closed) return
    closed = true
    clearInterval(timer)
    state.disconnects += 1
  })
}

const server = http.createServer((request, response) => {
  if (request.method === "GET" && request.url === "/stats") {
    const payload = JSON.stringify(state)
    response.writeHead(200, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) })
    response.end(payload)
    return
  }
  if (request.method !== "POST" || !["/v1/chat/completions", "/v1/responses"].includes(request.url)) {
    response.writeHead(404).end()
    return
  }
  const chunks = []
  request.on("data", (chunk) => chunks.push(chunk))
  request.on("end", () => {
    const body = JSON.parse(Buffer.concat(chunks).toString() || "{}")
    state.calls += 1
    state.requests.push(body)
    response.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", connection: "close" })
    hang(response, body, request.url === "/v1/responses")
  })
})

server.listen(port, "0.0.0.0")
