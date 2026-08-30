export function subprocessEnvironment() {
  return Object.fromEntries(Object.entries(process.env).filter(([name]) => name.toUpperCase() !== "FERN_TOKEN"))
}
