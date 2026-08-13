import http from "node:http"

const marker = "FERN_PARTIAL_MARKER"
let requests = 0

const server = http.createServer((request, response) => {
  if (request.method !== "POST" || request.url !== "/v1/chat/completions") {
    response.writeHead(404).end()
    return
  }

  requests++
  console.log(`request ${requests}`)
  response.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache",
    connection: "keep-alive",
  })
  response.flushHeaders()
  response.write(`data: {"id":"fern-test","object":"chat.completion.chunk","created":0,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}\n\n`)
  response.write(`data: {"id":"fern-test","object":"chat.completion.chunk","created":0,"model":"test-model","choices":[{"index":0,"delta":{"content":"${marker}"},"finish_reason":null}]}\n\n`)
  console.log(`marker ${marker}`)
})

server.listen(4100, "127.0.0.1", () => {
  console.log("fake LLM listening on http://127.0.0.1:4100/v1")
})
