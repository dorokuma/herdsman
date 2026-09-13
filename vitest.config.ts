import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "node",
    env: {
      HERDSMAN_HOME: join(tmpdir(), "herdsman-vitest-home"),
    },
    setupFiles: ["./test/setup-herdsman-home.ts"],
    include: ["test/unit/**/*.test.ts", "test/integration/**/*.test.ts"],
    passWithNoTests: false,
    restoreMocks: true,
    watch: false,
  },
});
