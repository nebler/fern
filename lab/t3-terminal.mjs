// Opens a terminal on T3's server, as the app's terminal pane does, and prints
// what T3 answered: the session's status, its process and the error the pane
// would show. lab/check.sh runs it in T3's own service (node from T3's image,
// stdin as the script) with T3_PAIR_URL, a pairing link, and T3_CWD, a folder.
const url = new URL(process.env.T3_PAIR_URL);
let res = await fetch(new URL("/oauth/token", url.origin), {
  method: "POST",
  body: new URLSearchParams({
    grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
    subject_token: new URLSearchParams(url.hash.slice(1)).get("token"),
    subject_token_type: "urn:t3:params:oauth:token-type:environment-bootstrap",
    requested_token_type: "urn:ietf:params:oauth:token-type:access_token",
    client_label: "fern check",
  }),
});
if (!res.ok) throw new Error(`pair: ${res.status} ${await res.text()}`);
const { access_token: token } = await res.json();
res = await fetch(new URL("/api/auth/websocket-ticket", url.origin), {
  method: "POST",
  headers: { authorization: `Bearer ${token}` },
});
if (!res.ok) throw new Error(`websocket ticket: ${res.status} ${await res.text()}`);
const { ticket } = await res.json();
const ws = new WebSocket(`${url.origin.replace(/^http/, "ws")}/ws?wsTicket=${ticket}&orchestrationProtocol=2`);
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = () => reject(new Error("websocket: could not connect"));
});
const threadId = `check-${crypto.randomUUID()}`;
const send = (id, tag, payload) => ws.send(JSON.stringify({ _tag: "Request", id, tag, payload, headers: [] }));
const answer = await new Promise((resolve, reject) => {
  let opened = null;
  let message = null;
  const done = () => opened && message !== null && resolve({ ...opened, message });
  ws.onmessage = (event) => {
    const frame = JSON.parse(event.data);
    if (frame._tag === "Chunk") {
      for (const value of frame.values) {
        if (value.threadId === threadId && value.type === "error") message = value.message;
      }
      ws.send(JSON.stringify({ _tag: "Ack", requestId: frame.requestId }));
    } else if (frame._tag === "Exit" && frame.requestId === "open") {
      if (frame.exit._tag !== "Success") return reject(new Error(`terminal.open: ${JSON.stringify(frame.exit.cause)}`));
      opened = frame.exit.value;
      // A terminal that started has no error to wait for.
      if (opened.status !== "error") message = "";
    }
    done();
  };
  setTimeout(() => reject(new Error("no answer from T3 in 10 s")), 10_000);
  send("events", "subscribeTerminalEvents", {});
  send("open", "terminal.open", { threadId, terminalId: "check", cwd: process.env.T3_CWD });
});
if (answer.status !== "error") send("close", "terminal.close", { threadId, terminalId: "check" });
console.log(`${answer.status} pid=${answer.pid} ${answer.message}`);
ws.close();
process.exit(0);
