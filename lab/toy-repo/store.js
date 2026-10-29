// Notes in one JSON file. Writes go to a temporary file and are renamed over
// the old one, so a snapshot never catches half a file.
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

const seed = [
  { id: 1, text: "buy milk" },
  { id: 2, text: "call Ada" },
  { id: 3, text: "ship v0.1" },
];

export function load(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (err) {
    if (err.code !== "ENOENT") throw err;
    save(path, seed);
    return structuredClone(seed);
  }
}

export function save(path, notes) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path + ".tmp", JSON.stringify(notes, null, 2));
  renameSync(path + ".tmp", path);
}
