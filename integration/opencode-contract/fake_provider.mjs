#!/usr/bin/env node
// Zero-cost OpenAI-compatible provider used by the black-box contract harness.

import http from "node:http"

const port = Number.parseInt(process.argv[2], 10)
const state = { calls: 0, disconnects: 0, hanging: 0, requests: [] }

function chatChunk(delta = {}, finish) {
  return {
    id: "chatcmpl-contract",
    object: "chat.completion.chunk",
    choices: [{ delta, ...(finish ? { finish_reason: finish } : {}) }],
  }
}

function questionArgs() {
  return {
    questions: [
      {
        header: "Contract",
        question: "Choose the contract answer",
        options: [
          { label: "Choice A", description: "The first choice" },
          { label: "Choice B", description: "The second choice" },
        ],
        multiple: false,
      },
    ],
  }
}

function line(response, value) {
  response.write(`data: ${typeof value === "string" ? value : JSON.stringify(value)}\n\n`)
}

function created(response, body) {
  line(response, {
    type: "response.created",
    sequence_number: 1,
    response: {
      id: "resp_contract",
      created_at: Math.floor(Date.now() / 1000),
      model: body.model ?? "test-model",
      service_tier: null,
    },
  })
}

function completed(response, sequence) {
  line(response, {
    type: "response.completed",
    sequence_number: sequence,
    response: {
      incomplete_details: null,
      service_tier: null,
      usage: {
        input_tokens: 1,
        input_tokens_details: { cached_tokens: null },
        output_tokens: 1,
        output_tokens_details: { reasoning_tokens: null },
      },
    },
  })
  line(response, "[DONE]")
  response.end()
}

function chatText(response, text) {
  line(response, chatChunk({ role: "assistant" }))
  line(response, chatChunk({ content: text }))
  line(response, chatChunk({}, "stop"))
  line(response, "[DONE]")
  response.end()
}

function chatQuestion(response) {
  const args = JSON.stringify(questionArgs())
  line(response, chatChunk({ role: "assistant" }))
  line(response, chatChunk({
    tool_calls: [{
      index: 0,
      id: "call_contract_question",
      type: "function",
      function: { name: "question", arguments: "" },
    }],
  }))
  line(response, chatChunk({ tool_calls: [{ index: 0, function: { arguments: args } }] }))
  line(response, chatChunk({}, "tool_calls"))
  line(response, "[DONE]")
  response.end()
}

function responsesText(response, body, text) {
  created(response, body)
  line(response, {
    type: "response.output_item.added",
    sequence_number: 2,
    output_index: 0,
    item: { type: "message", id: "msg_contract_provider" },
  })
  line(response, {
    type: "response.output_text.delta",
    sequence_number: 3,
    item_id: "msg_contract_provider",
    delta: text,
    logprobs: null,
  })
  line(response, {
    type: "response.output_item.done",
    sequence_number: 4,
    output_index: 0,
    item: { type: "message", id: "msg_contract_provider" },
  })
  completed(response, 5)
}

function responsesQuestion(response, body) {
  const args = JSON.stringify(questionArgs())
  created(response, body)
  line(response, {
    type: "response.output_item.added",
    sequence_number: 2,
    output_index: 0,
    item: {
      type: "function_call",
      id: "fc_contract_question",
      call_id: "call_contract_question",
      name: "question",
      arguments: "",
      status: "in_progress",
    },
  })
  line(response, {
    type: "response.function_call_arguments.delta",
    sequence_number: 3,
    output_index: 0,
    item_id: "fc_contract_question",
    delta: args,
  })
  line(response, {
    type: "response.function_call_arguments.done",
    sequence_number: 4,
    output_index: 0,
    item_id: "fc_contract_question",
    arguments: args,
  })
  line(response, {
    type: "response.output_item.done",
    sequence_number: 5,
    output_index: 0,
    item: {
      type: "function_call",
      id: "fc_contract_question",
      call_id: "call_contract_question",
      name: "question",
      arguments: args,
      status: "completed",
    },
  })
  completed(response, 6)
}

function hang(response, body, responses) {
  if (responses) created(response, body)
  else line(response, chatChunk({ role: "assistant" }))
  state.hanging += 1
  const timer = setInterval(() => response.write(": contract-heartbeat\n\n"), 100)
  let settled = false
  response.on("close", () => {
    if (settled) return
    settled = true
    clearInterval(timer)
    state.hanging -= 1
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
    response.writeHead(404)
    response.end()
    return
  }
  const chunks = []
  request.on("data", (chunk) => chunks.push(chunk))
  request.on("end", () => {
    const body = JSON.parse(Buffer.concat(chunks).toString() || "{}")
    state.calls += 1
    state.requests.push(body)
    const raw = JSON.stringify(body)
    const title = raw.includes("Generate a title for this conversation") || raw.includes("You are a title generator")
    const question = !title && raw.includes("CONTRACT_QUESTION") &&
      !["tool_call_output", "function_call_output", '\"role\":\"tool\"'].some((marker) => raw.includes(marker))
    const cancel = !title && raw.includes("CONTRACT_CANCEL")
    const responses = request.url === "/v1/responses"
    response.writeHead(200, {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "close",
    })
    if (cancel) {
      hang(response, body, responses)
      return
    }
    if (!responses) {
      if (question) chatQuestion(response)
      else chatText(response, title ? "Contract Title" : "contract fake response")
      return
    }
    if (question) responsesQuestion(response, body)
    else responsesText(response, body, title ? "Contract Title" : "contract fake response")
  })
})

server.listen(port, "0.0.0.0", () => console.log(`fake provider listening on ${port}`))
