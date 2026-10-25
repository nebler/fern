import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { app } from "../app.js";

async function start() {
  const server = app(join(mkdtempSync(join(tmpdir(), "notes-")), "notes.json"));
  await new Promise((resolve) => server.listen(0, resolve));
  const base = `http://localhost:${server.address().port}`;
  return { base, close: () => server.close() };
}

test("lists the seeded notes", async () => {
  const { base, close } = await start();
  const notes = await (await fetch(`${base}/notes`)).json();
  assert.equal(notes.length, 3);
  close();
});

test("adds a note", async () => {
  const { base, close } = await start();
  const res = await fetch(`${base}/notes`, { method: "POST", body: JSON.stringify({ text: "hi" }) });
  assert.equal(res.status, 201);
  assert.deepEqual(await res.json(), { id: 4, text: "hi" });
  close();
});

test("rejects a note without text", async () => {
  const { base, close } = await start();
  const res = await fetch(`${base}/notes`, { method: "POST", body: "{}" });
  assert.equal(res.status, 400);
  close();
});
