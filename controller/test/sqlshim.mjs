import { DatabaseSync } from "node:sqlite";

function makeCursor(rows) {
  let consumed = false;
  const consume = () => {
    if (consumed) return [];
    consumed = true;
    return rows.map((row) => ({ ...row }));
  };
  return {
    toArray() {
      return consume();
    },
    one() {
      const all = consume();
      if (all.length !== 1) {
        throw new Error(`Expected exactly 1 row, got ${all.length}`);
      }
      return all[0];
    },
    *[Symbol.iterator]() {
      yield* consume();
    },
  };
}

/**
 * Creates a mock Durable Object `ctx.storage` backed by in-memory `node:sqlite`.
 * @returns {{ sql: { exec: (query: string, ...bindings: unknown[]) => any }, close: () => void }}
 */
export function createMockStorage() {
  const db = new DatabaseSync(":memory:");
  return {
    sql: {
      exec(query, ...bindings) {
        const trimmed = query.trim();
        if (bindings.length === 0 && /;\s*\S/.test(trimmed)) {
          db.exec(query);
          return makeCursor([]);
        }
        const stmt = db.prepare(query);
        const rows = stmt.all(...bindings);
        return makeCursor(rows);
      },
    },
    close() {
      db.close();
    },
  };
}
