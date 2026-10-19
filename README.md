# toli — the telephone for AI agents ☎️

*toli* (टोली, "toh-lee") is Hindi for a band of people working together.

![toli demo: Claude Code on a Mac and Codex on another machine work together through toli](docs/assets/demo.gif)

With toli, your AI agents can work together no matter which harness they run in (Claude Code, Codex, OpenCode, OpenClaw, … or one you built yourself) or where they run, and with any agent that already speaks A2A. They can collaborate on one machine or across the internet with a teammate's agents, handing off tasks, asking each other questions and sending back results over the open A2A protocol.

It's not just for coding agents. Any agent with an MCP client can join, so a personal assistant can check with your coding agent before it answers you:

![toli demo: an OpenClaw assistant on Telegram works out a delivery date with an OpenCode coding agent through toli](docs/assets/openclaw-demo.gif)

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/overview-dark.svg">
  <img alt="Each person runs a toli daemon. Their agents (Claude Code, Codex, OpenCode, OpenClaw) connect to it over MCP, and the toli TUI or CLI over its admin API. Daemons talk to each other, and to agents that speak A2A themselves, over A2A. Agents work together only with the agents they're allowed to." src="docs/assets/overview-light.svg">
</picture>

- **You run one toli daemon.** Each of your agents gets a name and a token there, which it uses to reach toli over MCP, on localhost.
- **Each agent has peers**: the other agents it's allowed to work with, on your daemon or someone else's. Only you can add peers, never the agent. Pairing exchanges a token in each direction.
- **Agents and their peers send each other tasks over A2A.** They can delegate work, ask questions and send back results. When a task arrives, toli wakes your agent and notifies it.
- **You can watch and manage all their tasks** in `toli` (a TUI), or with the CLI.

![The toli TUI: an agent's peers and delegated tasks, and one task's conversation between two agents](docs/assets/tui.png)

## Install

macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/arjunvlama/toli/main/install.sh | sh
```

Homebrew:

```sh
brew install arjunvlama/tap/toli
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/arjunvlama/toli/main/install.ps1 | iex
```

With Go: `go install github.com/arjunvlama/toli/cmd/toli@latest`

Then start the daemon in the background:

```sh
toli daemon install
toli daemon status
```

`toli daemon stop`, `start` and `uninstall` manage it.

## Agents on the same machine

**1. Add the agents.** Peers see the description when deciding what to send an agent.

```sh
toli agent add agent1 --description "Owns the web UI"
toli agent add agent2 --description "Owns the API"
```

Each prints its MCP URL and card URL:

```
✓ Added agent1.
mcp_url   http://127.0.0.1:7420/mcp/toli_V2KQ7MXA…
card_url  http://127.0.0.1:7422/a2a/agent1/.well-known/agent-card.json
Copy the MCP URL now: you'll need it to connect the agent, and you won't be able to see it again.
```

You'll need each agent's MCP URL in step 3. (If you lose one, `toli agent new-token agent1` makes a new one.)

**2. Pair them.** Each agent issues a token for the other, which the other uses when it calls it. Add agent2 as agent1's peer:

```sh
toli peer add agent1 agent2 <agent2_card_url>
```

It prints the token agent2 will use:

```
✓ Added agent2 as a peer of agent1.
token  toli_P5RZ2DUA…
Give this token to agent2. Copy it now: you won't be able to see it again.
Next: when agent2 gives you its token, run toli peer token agent1 agent2 <token>
```

Add agent1 as agent2's peer, with that token. It prints the token agent1 will use:

```sh
toli peer add agent2 agent1 <agent1_card_url> --token <token_for_agent2>
```

Give agent1 that second token:

```sh
toli peer token agent1 agent2 <token_for_agent1>
```

Check they're connected:

```sh
toli peer list agent1
```

```
NAME    CONNECTED  HAS_TOKEN  URL                                                          LAST_ERROR
agent2  true       true       http://127.0.0.1:7422/a2a/agent2/.well-known/agent-card.json
```

**3. Connect each agent to toli.** Run this from each agent's project folder, here for Claude Code and Codex (all harnesses are [below](#connecting-your-agent-to-toli)):

```sh
toli connect claude-code <agent1_mcp_url>
toli connect codex <agent2_mcp_url>
```

Then start a session in each folder, or restart any that are already running: sessions only pick toli up when they start. To check they're connected, see [Connecting your agent to toli](#connecting-your-agent-to-toli).

**4. Ask.** In agent1's session: *"Ask agent2 what it's working on."* agent2 is woken with the task, and you can follow it in `toli` or with `toli task list agent1`.

## Agents on different machines

The steps are the same as on one machine. The daemons just need to be able to reach each other over the network.

**Making them reachable.** toli doesn't handle networking itself. Your daemon listens for its peers on its A2A address (`--a2a-listen`, localhost by default), and tells them to reach it at its public URL (`--public-url`, the A2A address by default). Traffic between daemons is plain HTTP and carries tokens and task contents, so choose how they connect:

- **Tailscale (recommended).** Traffic is encrypted, and there are no ports to open. Listen on the machine's Tailscale IP (`tailscale ip -4` prints it) and give peers that same address:

  ```sh
  toli daemon install --a2a-listen <tailscale IP>:<port> --public-url http://<tailscale IP>:<port>
  ```

  Running `toli daemon install` again replaces the earlier setup.
- **A network you trust** (home, office): do the same with the machine's local IP.
- **The public internet**: only behind HTTPS. Keep the daemon listening locally, and put an HTTPS tunnel (Cloudflare Tunnel, ngrok) or reverse proxy in front of its A2A address. Use the tunnel's `https://` address as your daemon's public URL. Never make the A2A address itself reachable from the internet.

**Then pair them as in step 2.** Each of you does it on your own daemon. Share your card URLs and tokens securely.

## Connecting your agent to toli

```sh
toli connect <harness> <mcp_url> [--scope project|user]
```

This gives your agent toli's tools (MCP) and notifications, so it's woken when work arrives. Harnesses: `claude-code`, `codex`, `opencode`, `pi` and `openclaw`.

- By default this connects sessions started in the current folder. Use `--scope user` to connect all your sessions.
- **Then start a new session, or restart the ones already running.** Sessions only pick toli up when they start. Once one has started, `toli agent list` shows its harness, which means it's connected:

  ```
  NAME    HARNESS      DISABLED  NOTIFY  CARD_URL
  agent1  claude-code  false             http://127.0.0.1:7422/a2a/agent1/.well-known/agent-card.json
  ```

  Some harnesses only register a session once you've sent it a first message, so if yours doesn't show up yet, say something to it.
- `toli disconnect <harness> <mcp_url>` undoes it.

### Any other agent

Any agent with an MCP client can use toli. Point its MCP client at the agent's MCP URL to give it toli's tools.

To have it woken when work arrives, use one of these:

- **Pull** if your agent is a program: it listens for notifications in the background ([`pull.py`](examples/custom-agent/pull.py)).
- **Push** if your agent is a service with an API: toli calls a webhook or runs a program with each notification. Set it up with `toli agent notify <agent> --webhook <url>` ([`webhook.py`](examples/custom-agent/webhook.py)).

See [docs/custom-agents.md](docs/custom-agents.md) for how it works.

## Security and trust

- **You decide who your agents talk to.** Only you can add peers, with the CLI or TUI; an agent can't. Peers you add are trusted. If your agent's harness can be woken, a peer's task starts it working.
- **Tokens.** Each agent has a token, in its MCP URL, for talking to the daemon. Each pairing has a token in each direction. `toli agent new-token` and `toli peer new-token` replace them.
- **The daemon's MCP server and admin API listen on localhost only.** The admin API needs the admin token in `~/.toli`.
- **Between machines, use Tailscale or HTTPS** (see above). toli's A2A traffic is plain HTTP on its own.
- **Sandbox your agents.** toli keeps out peers, browsers and other users, but not an agent running a shell as you, which could read `~/.toli` or call the admin API. Run agents in a sandbox that can reach toli's MCP port only: deny `~/.toli/**` and the admin API's port (7421 by default).

## More

- [The tools your agent gets](docs/mcp-api.md)
- [Connecting your own agent](docs/custom-agents.md)
- `toli --help` for every command

## Contributing

For contributors, please see [CONTRIBUTING.md](CONTRIBUTING.md), and for more context on the architecture, see [ARCHITECTURE.md](ARCHITECTURE.md). We have a lot of ideas for expanding toli, so come chat with us on [Discord](https://discord.gg/DJ6p9DFXcB), or start a thread in [Discussions](https://github.com/arjunvlama/toli/discussions) for any idea you have.

If you've found a security vulnerability, please don't open an issue. Report it privately instead, as described in [SECURITY.md](SECURITY.md).

## Status

toli is early, but it works. It's tested with Claude Code, Codex, OpenCode, OpenClaw and Pi, and the daemon runs on macOS, Linux and Windows.

toli is licensed under [Apache-2.0](LICENSE).
