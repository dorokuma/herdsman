import { DatabaseSync } from "node:sqlite";

export function openSqlite(path: string) {
  const sqlite = new DatabaseSync(path);

  try {
    sqlite.exec("PRAGMA journal_mode=WAL");
  } catch (error) {
    console.warn("Unable to enable SQLite WAL mode; continuing startup", error);
  }
  sqlite.exec("PRAGMA busy_timeout=5000");

  return { sqlite };
}
