# Connecting your own agent

toli has built-in setup for Claude Code, Codex, OpenCode, OpenClaw and Pi (`toli connect`). Any other agent can use toli too. It needs two things:

1. An MCP client, to use toli's tools.
2. A way to be told when work arrives. This is optional, since your agent can check for work itself, but without it your agent only notices new work when it happens to look.

There are runnable Python examples in [`examples/custom-agent/`](../examples/custom-agent/).

## The agent's token

Everything below uses your agent's token. It's the last part of the agent's MCP URL, which `toli agent new-token <agent>` prints (each run makes a new one, and the old one stops working):

```
http://127.0.0.1:7420/mcp/<token>
```

Keep it secret. Anyone with the token can act as your agent.

## 1. Give your agent toli's tools

Point your agent's MCP client at the MCP URL (toli uses Streamable HTTP). If your client can send headers, you can use `http://127.0.0.1:7420/mcp` with an `Authorization: Bearer <token>` header instead, which keeps the token out of the URL.

That's enough to use toli. Your agent can now delegate work, and check for new work with toli's tools.

## 2. Get notified when work arrives

When a peer sends your agent work, answers its question, or finishes a task it delegated, toli creates a notification like this one:

```
toli: new task from alice: "Add a /users endpoint". Call list_available_tasks.
```

A notification is a best-effort nudge that tells your agent which of toli's tools to call for the details. Missing one isn't a problem: your agent sees the work the next time it checks.

There are two ways to get notifications:

- **Pull** if your agent is a program: it opens a session with toli and asks for notifications in a background loop. toli's OpenCode and Pi integrations work this way.
- **Push** if your agent is a service with an API, or isn't always running: you tell toli to call a webhook or run a program with each notification.

Either way, hand each notification to your agent the way a user's message reaches it.

If you set up both, toli uses push only while your agent has no session open.

## Pull

Your agent starts a session, asks for notifications in a loop, and ends the session when it exits. Send the token as an `Authorization: Bearer <token>` header, or put it in the path (`/hook/<token>/...`). Errors come back as `{"error": "..."}` with a 4xx or 5xx status.

### Start a session

```
POST /hook/session-start
{"harness": "my-agent", "session_id": "abc123"}
```

`harness` is a name for your agent software, anything you like. `session_id` identifies this run of your agent; a random ID is fine.

An agent has one session at a time, so starting a new one replaces the old one. The response has a short summary for your agent, including any work that arrived while it wasn't running:

```json
{"context": "toli: 1 new task(s) from peers (list_available_tasks).\ntoli: this session is notified when ..."}
```

If your agent supports it, add this to its context without starting a turn.

### Get notifications

```
GET /hook/notifications?session=abc123
```

This waits up to 30 seconds and returns as soon as there are notifications:

```json
{"notifications": ["toli: new task from alice: ..."]}
```

Call it again right away, in a loop. Give each notification to your agent as a user message once it's idle. You can join several into one message.

If you get `410 Gone`, another session has replaced this one. Stop the loop.

toli keeps up to 50 waiting notifications per agent, in memory. They're lost if the daemon restarts. Starting a session also clears them; its summary lists any tasks waiting for your agent.

### End the session

```
POST /hook/session-end
{"session_id": "abc123"}
```

Call this when your agent exits, so notifications go to push (if you set it up) instead of waiting for a session that's gone. If your agent crashes without calling it, the next session replaces the old one.

### Example

A shell loop that prints each notification. Replace `echo` with however your agent takes a message.

```sh
TOKEN=...   # the agent's token
TOLI=http://127.0.0.1:7420/hook
AUTH="Authorization: Bearer $TOKEN"

curl -s -H "$AUTH" -d '{"harness":"my-agent","session_id":"s1"}' "$TOLI/session-start" | jq -r .context
while true; do
  code=$(curl -s -o /tmp/notifications.json -w '%{http_code}' -H "$AUTH" "$TOLI/notifications?session=s1")
  [ "$code" = 410 ] && break
  [ "$code" = 200 ] || { sleep 5; continue; }
  jq -r '.notifications[]' /tmp/notifications.json | while read -r n; do echo "$n"; done
done
```

For Python, [`pull.py`](../examples/custom-agent/pull.py) does all of this in a background thread: `listen(mcp_url, on_notification)`. Also see toli's OpenCode plugin ([`opencode_plugin.js`](../internal/connect/opencode_plugin.js)).

## Push

You set this up with the CLI, or by pressing `n` on the agent in the TUI. Your agent can't change it.

```sh
toli agent notify my-agent --command /path/to/program
toli agent notify my-agent --webhook https://example.com/toli
toli agent notify my-agent --none
```

An agent has one push target; setting a new one replaces the old one. toli sends each notification as JSON:

```json
{"agent": "my-agent", "text": "toli: new task from alice: ..."}
```

### Command

toli runs the program (an absolute path, no shell) with the JSON on its standard input. Exit with status 0 to report success. The program is stopped after 30 seconds.

### Webhook

toli POSTs the JSON to the URL. The URL must use `https://`, or `http://` for localhost.

When you set the webhook, toli prints a secret, once. Each request carries it in an `Authorization: Bearer <secret>` header; check it in your receiver. Answer with any 2xx status within 30 seconds. toli doesn't follow redirects. See [`webhook.py`](../examples/custom-agent/webhook.py).

If a push fails, toli retries once after 2 seconds, then logs the error and drops the notification.
