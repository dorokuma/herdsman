import { existsSync, readFileSync, readlinkSync, unlinkSync } from "node:fs";
import { createConnection, createServer } from "node:net";
import { tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { describe, expect, test } from "vitest";
import {
  linuxPeerPid,
  PYTHON3_CANDIDATES,
  peerBoundToPaneCwd,
  peerBoundToPanePid,
  resolvePython3Path,
  unixSocketFd,
} from "@/shared/unix-peercred.js";

describe("unix peer credentials", () => {
  test("reads the connecting process pid via Linux UDS SO_PEERCRED", async () => {
    const socketPath = join(tmpdir(), `herdsman-peercred-${process.pid}.sock`);
    if (existsSync(socketPath)) unlinkSync(socketPath);
    const peerPid = await new Promise<number | undefined>((resolve, reject) => {
      const server = createServer((socket) => {
        try {
          resolve(linuxPeerPid(socket));
        } catch (error) {
          reject(error);
        } finally {
          socket.destroy();
          server.close();
          if (existsSync(socketPath)) unlinkSync(socketPath);
        }
      });
      server.once("error", reject);
      server.listen(socketPath, () => {
        const client = createConnection(socketPath);
        client.once("error", reject);
      });
    });
    expect(unixSocketFd).toBeTypeOf("function");
    expect(peerPid).toBe(process.pid);
  });

  test("resolves python3 from a trusted absolute path rather than PATH", () => {
    const python3 = resolvePython3Path();
    expect(python3).toBeDefined();
    expect(isAbsolute(python3 ?? "")).toBe(true);
    expect(python3).not.toBe("python3");
    expect(PYTHON3_CANDIDATES).toContain(python3);
    expect(PYTHON3_CANDIDATES.every((candidate) => isAbsolute(candidate))).toBe(true);
  });

  test("binds a connector to the pane pid or an ancestor of the connector", () => {
    expect(peerBoundToPanePid(process.pid, process.pid)).toBe(true);
    expect(peerBoundToPanePid(process.pid, 2_147_483_647)).toBe(false);
    expect(peerBoundToPanePid(1, process.pid)).toBe(false);
    const status = readFileSync(`/proc/${process.pid}/status`, "utf8");
    const match = /^PPid:\s*(\d+)\s*$/m.exec(status);
    const ppid = Number.parseInt(match?.[1] ?? "", 10);
    if (Number.isInteger(ppid) && ppid > 1) {
      expect(peerBoundToPanePid(process.pid, ppid)).toBe(true);
    }
  });

  test("binds a connector when /proc cwd matches the Herdr pane cwd", () => {
    const cwd = readlinkSync(`/proc/${process.pid}/cwd`);
    expect(peerBoundToPaneCwd(process.pid, cwd)).toBe(true);
    expect(peerBoundToPaneCwd(process.pid, process.cwd())).toBe(true);
    expect(peerBoundToPaneCwd(process.pid, "/tmp/herdsman-not-this-pane")).toBe(false);
    expect(peerBoundToPaneCwd(1, cwd)).toBe(false);
  });
});
