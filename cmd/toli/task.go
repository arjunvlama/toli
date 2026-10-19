package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arjunvlama/toli/internal/api"
)

const taskUsage = `usage:
  toli task list AGENT [--assigned]
  toli task show AGENT ID [--assigned]
  toli task cancel AGENT ID
Tasks are AGENT's delegated tasks, or with --assigned, the ones peers gave it.
ID can be the short one task list shows.`

// runTask is `toli task ...`: what the TUI's Delegated and Assigned tabs show.
func runTask(args []string) error {
	if len(args) < 1 {
		return errors.New(taskUsage)
	}
	ctx := context.Background()
	cmd, args := args[0], args[1:]
	switch {
	case cmd == "list" && len(args) >= 1:
		assigned, err := parseAssigned("task list", args[1:])
		if err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		tasks, err := listTasks(ctx, c, args[0], assigned)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tPEER\tUPDATED_AT\tSUMMARY")
		for _, t := range tasks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", shortID(t.ID), state(t.State), t.Peer, t.UpdatedAt.Local().Format(time.DateTime), t.Summary)
		}
		return w.Flush()
	case cmd == "show" && len(args) >= 2:
		assigned, err := parseAssigned("task show", args[2:])
		if err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		agent := args[0]
		id, err := resolveTask(ctx, c, agent, args[1], assigned)
		if err != nil {
			return err
		}
		get := c.GetTask
		if assigned {
			get = c.GetAssignedTask
		}
		t, err := get(ctx, agent, id)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "id\t%s\nstate\t%s\npeer\t%s\nupdated_at\t%s\n", t.ID, state(t.State), t.Peer, t.UpdatedAt.Local().Format(time.DateTime))
		if t.Deadline != nil {
			fmt.Fprintf(w, "deadline\t%s\n", t.Deadline.Local().Format(time.DateTime))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		for _, m := range t.Messages {
			from := agent
			if m.FromPeer {
				from = t.Peer
			}
			fmt.Printf("\n%s:\n%s\n", from, indent(m.Text))
		}
		for _, a := range t.Artifacts {
			fmt.Printf("\n%s:\n%s\n", strings.TrimSpace("artifact "+a.Name), indent(a.Text))
		}
		return nil
	case cmd == "cancel" && len(args) == 2:
		c, err := adminClient()
		if err != nil {
			return err
		}
		agent := args[0]
		id, err := resolveTask(ctx, c, agent, args[1], false)
		if err != nil {
			return err
		}
		t, err := c.CancelTask(ctx, agent, id)
		if err != nil {
			return err
		}
		if s := state(t.State); s == "CANCELED" {
			ok("Canceled task %s.", shortID(t.ID))
		} else {
			ok("Asked to cancel task %s; it's %s.", shortID(t.ID), s)
		}
		return nil
	}
	return errors.New(taskUsage)
}

func listTasks(ctx context.Context, c *api.Client, agent string, assigned bool) ([]api.TaskInfo, error) {
	if assigned {
		return c.ListAssignedTasks(ctx, agent)
	}
	return c.ListTasks(ctx, agent)
}

// resolveTask turns a task ID, or the short one task list shows (its end),
// into the full ID.
func resolveTask(ctx context.Context, c *api.Client, agent, id string, assigned bool) (string, error) {
	tasks, err := listTasks(ctx, c, agent, assigned)
	if err != nil {
		return "", err
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	full, err := matchTask(ids, id)
	if err != nil {
		return "", fmt.Errorf("agent %s: %w", agent, err)
	}
	return full, nil
}

// matchTask finds id among ids: the same ID, or the only one ending with it.
func matchTask(ids []string, id string) (string, error) {
	var found []string
	for _, full := range ids {
		if full == id {
			return full, nil
		}
		if strings.HasSuffix(full, id) {
			found = append(found, full)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no task %s", id)
	}
	return "", fmt.Errorf("%s matches %d tasks: use more of the ID", id, len(found))
}

// parseAssigned parses a command's only flag, --assigned.
func parseAssigned(name string, args []string) (bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	assigned := fs.Bool("assigned", false, "the tasks peers gave the agent, not the ones it delegated")
	err := fs.Parse(args)
	return *assigned, err
}

// shortID is the end of a task ID, as the TUI shows it.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// state is an A2A state without its prefix, e.g. COMPLETED.
func state(s string) string {
	return strings.TrimPrefix(s, "TASK_STATE_")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
