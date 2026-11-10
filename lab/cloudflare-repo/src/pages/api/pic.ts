import { env } from "cloudflare:workers";

export async function GET() {
  const pic = await env.PICS.get("broccoli.txt");
  return pic ? new Response(pic.body) : new Response("none", { status: 404 });
}

export async function PUT({ request }: { request: Request }) {
  await env.PICS.put("broccoli.txt", await request.text());
  return new Response(null, { status: 204 });
}

export async function DELETE() {
  await env.PICS.delete("broccoli.txt");
  return new Response(null, { status: 204 });
}
