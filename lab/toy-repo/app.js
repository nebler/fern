import { createServer } from "node:http";
import { load, save } from "./store.js";

export function app(path) {
  return createServer(async (req, res) => {
    const send = (code, body) => {
      res.writeHead(code, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "GET" && req.url === "/health") return send(200, { ok: true });
    if (req.method === "GET" && req.url === "/notes") return send(200, load(path));
    if (req.method === "POST" && req.url === "/notes") {
      let body = "";
      for await (const chunk of req) body += chunk;
      const { text } = JSON.parse(body || "{}");
      if (!text) return send(400, { error: "text is required" });
      const notes = load(path);
      const note = { id: Math.max(0, ...notes.map((n) => n.id)) + 1, text };
      save(path, [...notes, note]);
      return send(201, note);
    }
    send(404, { error: "not found" });
  });
}
