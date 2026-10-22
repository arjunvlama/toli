# Connecting your own agent

These examples are for a harness not listed in `toli connect`. Your agent uses toli's tools through its MCP client, pointed at the agent's MCP URL (`toli agent new-token <agent>` prints one). These scripts show the other half: getting notified when work arrives.

The pull examples need Python 3.9 or later, or Node 22.18 or later for the TypeScript one, and nothing else. Each calls `on_notification` with each notification; make it pass the notification to your agent the way a user's message reaches it.

A notification is a short message such as `toli: new task from alice: "Add a /users endpoint". Call list_available_tasks.` Your agent reads it and does the work with toli's tools.

## Pull

Use `pull.py` if your agent is a program. Start listening when your agent starts; it runs in a background thread and leaves your agent's loop alone:

```python
from pull import listen

listener = listen(mcp_url, on_notification=my_agent.send_user_message)
my_agent.add_context(listener.context)  # work that arrived while it wasn't running
...
listener.stop()                         # when your agent exits
```

If another listener starts for the same agent, this one stops. To try it on its own, run it and it prints each notification:

```sh
TOLI_MCP_URL=http://127.0.0.1:7420/mcp/<token> python3 pull.py
```

`pull.ts` is the same listener in TypeScript for Node. Node runs the file as-is, so there is nothing to install or compile:

```ts
import { listen } from "./pull.ts";

const listener = listen(mcpUrl, (text) => myAgent.sendUserMessage(text));
myAgent.addContext(listener.context); // work that arrived while it wasn't running
...
listener.stop();                      // when your agent exits
```

```sh
TOLI_MCP_URL=http://127.0.0.1:7420/mcp/<token> node pull.ts
```

## Push

Use push if your agent is a service with an API: point the webhook at it. To try push, use `webhook.py`, a minimal receiver. First tell toli to send notifications to it; this prints a secret once:

```sh
toli agent notify my-agent --webhook http://127.0.0.1:8787/toli
```

Then start the receiver with that secret:

```sh
TOLI_WEBHOOK_SECRET=<secret> python3 webhook.py
```

toli can also run a program with each notification on its standard input, instead of calling a webhook:

```sh
toli agent notify my-agent --command /path/to/program
```

See [docs/custom-agents.md](../../docs/custom-agents.md) for the details.
