#!/usr/bin/env node
// Pull: get toli's notifications while your agent runs. TypeScript counterpart
// to pull.py. Node 22.18+ runs this file as-is; no dependencies.
//
// In your agent's program, start listening in the background and pass each
// notification to your agent the way a user's message reaches it:
//
//     import { listen } from "./pull.ts";
//
//     const listener = listen(mcpUrl, (text) => myAgent.sendUserMessage(text));
//     myAgent.addContext(listener.context); // pending work, if your agent can take it
//     ...
//     listener.stop();                      // when your agent exits
//
// Or run this file on its own to print notifications:
//
//     TOLI_MCP_URL=http://127.0.0.1:7420/mcp/<token> node pull.ts
//
// Your agent does the work itself with toli's tools: point its MCP client at
// the same MCP URL. Protocol: docs/custom-agents.md.

const HARNESS = "my-agent"; // a name for your agent software

export type Listener = {
  /** Pass to your agent as context: work that arrived while it wasn't running. */
  context: string;
  /** Ends the session and stops the background loop. */
  stop: () => void;
};

// toli serves the pull endpoints next to the MCP URL: .../mcp/<token> becomes
// .../hook/<token>/... The token travels in the path, so no header is needed.
export function hookUrl(mcpUrl: string): string {
  const url = new URL(mcpUrl);
  const base = url.pathname.replace(/\/mcp\/([^/]+)\/?$/, "/hook/$1");
  if (base === url.pathname) {
    throw new Error(`expected an MCP URL ending in /mcp/<token>, got ${url.pathname}`);
  }
  return url.origin + base;
}

async function errorText(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    return body.error ?? response.statusText;
  } catch {
    return response.statusText;
  }
}

export function listen(mcpUrl: string, onNotification: (text: string) => void): Listener {
  const base = hookUrl(mcpUrl);
  const sessionId = crypto.randomUUID();
  const controller = new AbortController();
  let started = false;
  let summary = "";

  async function run(): Promise<void> {
    const start = await fetch(base + "/session-start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ harness: HARNESS, session_id: sessionId }),
      signal: controller.signal,
    });
    if (!start.ok) throw new Error(`session-start: ${start.status} ${await errorText(start)}`);
    started = true;
    const { context } = (await start.json()) as { context?: string };
    summary = context ?? "";
    if (summary) process.stderr.write(summary + "\n");

    for (;;) {
      const poll = await fetch(base + `/notifications?session=${sessionId}`, {
        method: "GET",
        signal: controller.signal,
      });
      if (poll.status === 410) return; // Another session replaced this one.
      if (!poll.ok) {
        process.stderr.write(`notifications: ${poll.status} ${await errorText(poll)}; retrying\n`);
        await new Promise((resolve) => setTimeout(resolve, 5000));
        continue;
      }
      const { notifications } = (await poll.json()) as { notifications?: string[] };
      for (const text of notifications ?? []) onNotification(text);
    }
  }

  run().catch((error: unknown) => {
    if (!controller.signal.aborted) {
      process.stderr.write(`toli listener stopped: ${(error as Error).message}\n`);
    }
  });

  return {
    get context() {
      return summary;
    },
    stop() {
      if (controller.signal.aborted) return;
      controller.abort();
      if (!started) return;
      // Best effort: later notifications go to push, if it's set up.
      void fetch(base + "/session-end", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session_id: sessionId }),
      }).catch(() => {});
    },
  };
}

// Run on its own to print each notification.
async function main(): Promise<void> {
  const mcpUrl = process.env.TOLI_MCP_URL;
  if (!mcpUrl) {
    console.error("set TOLI_MCP_URL to the agent's MCP URL (toli agent new-token <agent> prints one)");
    process.exit(1);
  }
  const listener = listen(mcpUrl, (text) => console.log(`[${HARNESS}] ${text}`));
  const shutdown = (): void => {
    listener.stop();
    process.exit(0);
  };
  process.once("SIGINT", shutdown);
  process.once("SIGTERM", shutdown);
}

if (process.argv[1]?.endsWith("pull.ts")) {
  void main();
}
