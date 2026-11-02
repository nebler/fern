import pg from "pg";

export const dynamic = "force-dynamic";

// Reads the database docker-compose.yml describes, through the DATABASE_URL
// Fern's guess sets.
export default async function Page() {
  const db = new pg.Client({ connectionString: process.env.DATABASE_URL });
  await db.connect();
  const { rows } = await db.query("select current_user as name");
  await db.end();
  return <h1>Hello from Next.js, database user {rows[0].name}</h1>;
}
