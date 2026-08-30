#!/usr/bin/env node
// Zero-cost OpenAI-compatible provider for the official 1.18.16 contract.

import http from "node:http"

const port = Number.parseInt(process.argv[2], 10)
const state = { calls: 0, disconnects: 0, hanging: 0, requests: [] }

function chunk(response, delta = {}, finish_reason = null) {
  response.write(`data: ${JSON.stringify({
    id: "chatcmpl-background-contract",
    object: "chat.completion.chunk",
    created: 0,
    model: "test-model",
    choices: [{ index: 0, delta, finish_reason }],
  })}\n\n`)
}

function done(response) {
  response.write("data: [DONE]\n\n")
  response.end()
}

function text(response) {
  chunk(response, { role: "assistant" })
  chunk(response, { content: "contract fake response" })
  chunk(response, {}, "stop")
  done(response)
}

function question(response) {
  const args = JSON.stringify({
    questions: [{
      header: "Contract",
      question: "Choose the contract answer",
      options: [
        { label: "Choice A", description: "The first choice" },
        { label: "Choice B", description: "The second choice" },
      ],
      multiple: false,
    }],
  })
  chunk(response, { role: "assistant" })
  chunk(response, {
    tool_calls: [{
      index: 0,
      id: "call_background_contract_question",
      type: "function",
      function: { name: "question", arguments: "" },
    }],
  })
  chunk(response, { tool_calls: [{ index: 0, function: { arguments: args } }] })
  chunk(response, {}, "tool_calls")
  done(response)
}

function hang(response) {
  chunk(response, { role: "assistant" })
  chunk(response, { content: "contract partial response" })
  state.hanging += 1
  const timer = setInterval(() => response.write(": contract-heartbeat\n\n"), 100)
  response.on("close", () => {
    clearInterval(timer)
    state.hanging -= 1
    state.disconnects += 1
  })
}

const server = http.createServer((request, response) => {
  if (request.method === "GET" && request.url === "/stats") {
    const body = JSON.stringify(state)
    response.writeHead(200, { "content-type": "application/json", "content-length": Buffer.byteLength(body) })
    response.end(body)
    return
  }
  if (request.method !== "POST" || request.url !== "/v1/chat/completions") {
    response.writeHead(404).end()
    return
  }

  const chunks = []
  request.on("data", (value) => chunks.push(value))
  request.on("end", () => {
    const body = JSON.parse(Buffer.concat(chunks).toString() || "{}")
    state.calls += 1
    state.requests.push(body)
    const raw = JSON.stringify(body)
    const isQuestion = raw.includes("CONTRACT_QUESTION") &&
      !["tool_call_id", '"role":"tool"'].some((marker) => raw.includes(marker))
    response.writeHead(200, {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "close",
    })
    if (raw.includes("CONTRACT_HANG")) {
      hang(response)
      return
    }
    if (isQuestion) {
      question(response)
      return
    }
    text(response)
  })
})

server.listen(port, "0.0.0.0", () => console.log(`fake provider listening on ${port}`))
