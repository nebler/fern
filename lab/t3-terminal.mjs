// Attaches to a terminal on T3's server, as the app's terminal pane does
// (terminal.attach starts it and streams what it prints), types a line into
// it and prints what came back: "<status> | <what T3_EXPECT matched>", or
// the end of what the terminal printed if nothing matched in 30 s. lab/check.sh runs it in T3's own service (node from T3's image,
// stdin as the script) with T3_PAIR_URL, a pairing link; T3_CWD, the folder
// the terminal opens in; T3_TYPE, the line to type once it is up (optional);
// T3_RESIZE, "cols rows" to resize it to before typing (optional); and
// T3_EXPECT, a regular expression for the output.
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

const session = { threadId: `check-${crypto.randomUUID()}`, terminalId: "check" };
const expect = new RegExp(process.env.T3_EXPECT ?? "$^");
const send = (id, tag, payload) => ws.send(JSON.stringify({ _tag: "Request", id, tag, payload, headers: [] }));
let status = "?";
let printed = "";
const matched = await new Promise((resolve) => {
  const look = () => {
    const m = expect.exec(printed);
    if (m) resolve(m[0]);
  };
  let typed = false;
  ws.onmessage = (event) => {
    const frame = JSON.parse(event.data);
    if (frame._tag === "Chunk") {
      for (const value of frame.values) {
        if (value.type === "snapshot") {
          status = value.snapshot.status;
          printed += value.snapshot.history;
        }
        if (value.type === "output") printed += value.data;
        if (value.type === "error") {
          status = "error";
          printed += `[terminal] ${value.message}`;
        }
        if (value.type === "exited") status = "exited";
      }
      ws.send(JSON.stringify({ _tag: "Ack", requestId: frame.requestId }));
      if (status === "running" && !typed) {
        typed = true;
        if (process.env.T3_RESIZE) {
          const [cols, rows] = process.env.T3_RESIZE.split(" ").map(Number);
          send("resize", "terminal.resize", { ...session, cols, rows });
        }
        // The shell in the box needs a moment before it reads what is typed.
        if (process.env.T3_TYPE) setTimeout(() => send("write", "terminal.write", { ...session, data: `${process.env.T3_TYPE}\r` }), 3000);
      }
      look();
    } else if (frame._tag === "Exit" && frame.requestId === "attach" && frame.exit._tag !== "Success") {
      resolve(`terminal.attach failed: ${JSON.stringify(frame.exit.cause)}`);
    }
  };
  setTimeout(() => resolve(null), 30_000);
  send("attach", "terminal.attach", { ...session, cwd: process.env.T3_CWD });
});
// A shell that ended says so just after its last output.
await new Promise((resolve) => setTimeout(resolve, 1500));
console.log(`${status} | ${matched ?? `no match; it printed: ${JSON.stringify(printed.slice(-400))}`}`);
send("close", "terminal.close", { ...session, deleteHistory: true });
await new Promise((resolve) => setTimeout(resolve, 500));
ws.close();
process.exit(0);
