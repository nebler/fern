import { app } from "./app.js";

const port = Number(process.env.PORT ?? 3000);
app("data/notes.json").listen(port, () => console.log(`notes on :${port}`));
