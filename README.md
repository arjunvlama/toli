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

A two-minute quickstart.

**1. Add the agents, and connect them to toli.** Run this in the folder where you start each agent: where you type `claude` or `codex`, or the folder you opened in your editor (its built-in terminal starts there). `--connect` names the agent's harness ([all harnesses](#connecting-your-agent-to-toli)), and peers see the description when deciding what to send it.

```sh
cd ~/src/web-ui
toli agent add alice --description "Owns the web UI" --connect claude-code

cd ~/src/api
toli agent add bob --description "Owns the API" --connect codex
```

Each prints what it did:

```
✓ Added agent 'alice'.
card_url  http://127.0.0.1:7422/a2a/alice/.well-known/agent-card.json
✓ Connected Claude Code to agent 'alice' in ~/src/web-ui (~/.claude.json, .claude/settings.local.json).
Next: start a Claude Code session, or restart a running one (`claude --continue`).
```

Sessions only pick toli up when they start, so start one in each folder, or restart any that are already running. Then `toli agent list` shows each agent's harness, which means it's connected. (Some harnesses, like Codex, only show up once you've sent them a message after starting or restarting.)

**2. Pair them.** alice invites bob:

```sh
toli peer invite alice bob
```

```
✓ Created an invite for peer 'bob'. It works once, within 24 hours.

To accept it, run this where peer 'bob' runs:

    toli peer accept <agent> toli-invite:eyJ2IjoxLCJhZ2Vu…

Send it privately: whoever has it can pair with agent 'alice'.
```

bob accepts:

```sh
toli peer accept bob <invite>
```

```
✓ Paired agent 'bob' with peer 'alice'.
```

Check they're connected:

```sh
toli peer list alice
```

```
NAME  CONNECTED  HAS_TOKEN  URL                                                        LAST_ERROR
bob   true       true       http://127.0.0.1:7422/a2a/bob/.well-known/agent-card.json
```

**3. Ask.** In alice's session: *"Ask bob what it's working on."* bob is woken with the task. To watch them work together, run `toli` (the TUI), or `toli task list alice`.

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

**Then pair them as in step 2.** One of you creates the invite and sends it to the other privately, like a password; the other accepts it on their own daemon. To pair without an invite, use `toli peer add` and `toli peer token` (`toli peer --help`).

## Connecting your agent to toli

```sh
toli connect <harness> <agent> [--scope project|user]
```

This gives your agent toli's tools (MCP) and notifications, so it's woken when work arrives. Harnesses: `claude-code`, `codex`, `opencode`, `pi` and `openclaw`. `toli agent add --connect` does the same when adding an agent.

- Run it in the folder where you start the agent. It connects sessions started there; use `--scope user` to connect all your sessions.
- It makes a new MCP URL for the agent, so a session still using the old one is disconnected. To use a URL you already have, pass `--mcp-url <url>` instead of the agent.
- **Then start a new session, or restart the ones already running.** Sessions only pick toli up when they start. Once one has started, `toli agent list` shows its harness, which means it's connected:

  ```
  NAME   HARNESS      DISABLED  NOTIFY  CARD_URL
  alice  claude-code  false             http://127.0.0.1:7422/a2a/alice/.well-known/agent-card.json
  ```

  Some harnesses only register a session once you've sent it a first message, so if yours doesn't show up yet, say something to it.
- `toli disconnect <harness>` undoes it.

### Any other agent

Any agent with an MCP client can use toli. Point its MCP client at the agent's MCP URL, which `toli agent new-token <agent>` prints, to give it toli's tools.

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
