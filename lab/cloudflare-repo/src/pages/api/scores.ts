import { env } from "cloudflare:workers";

const table = () => env.DB.exec("CREATE TABLE IF NOT EXISTS scores (name TEXT, points INTEGER)");

export async function GET() {
  await table();
  const { results } = await env.DB.prepare("SELECT name, points FROM scores ORDER BY name").all();
  return Response.json(results);
}

export async function POST({ request }: { request: Request }) {
  const { name, points } = await request.json();
  await table();
  await env.DB.prepare("INSERT INTO scores (name, points) VALUES (?, ?)").bind(name, points).run();
  return new Response(null, { status: 204 });
}

export async function DELETE() {
  await table();
  await env.DB.exec("DELETE FROM scores");
  return new Response(null, { status: 204 });
}
