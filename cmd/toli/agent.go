package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/arjunvlama/toli/internal/api"
)

const agentUsage = `usage:
  toli agent add AGENT [--description TEXT] [--skills FILE]
  toli agent edit AGENT [--description TEXT] [--skills FILE]
  toli agent new-token AGENT
  toli agent notify AGENT --command PATH | --webhook URL | --none
  toli agent disable AGENT
  toli agent enable AGENT
  toli agent remove AGENT [--yes]
  toli agent list`

// runAgent is `toli agent ...`: what the TUI's Agents panel does.
func runAgent(args []string) error {
	if len(args) < 1 {
		return errors.New(agentUsage)
	}
	ctx := context.Background()
	cmd, args := args[0], args[1:]
	switch {
	case (cmd == "add" || cmd == "edit") && len(args) >= 1:
		fs := flag.NewFlagSet("agent "+cmd, flag.ContinueOnError)
		desc := fs.String("description", "", "what the agent works on; peers see it")
		skillsFile := fs.String("skills", "", "a JSON file with the agent's skills: [{\"name\": ..., \"description\": ..., \"tags\": [...], \"examples\": [...]}]")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		var skills []api.Skill
		if set["skills"] {
			var err error
			if skills, err = readSkills(*skillsFile); err != nil {
				return err
			}
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		name := args[0]
		if cmd == "edit" {
			// The API replaces both: keep whichever wasn't given.
			a, err := findAgent(ctx, c, name)
			if err != nil {
				return err
			}
			if !set["description"] {
				*desc = a.Description
			}
			if !set["skills"] {
				skills = a.Skills
			}
			if err := c.UpdateAgent(ctx, name, api.UpdateAgentRequest{Description: *desc, Skills: skills}); err != nil {
				return err
			}
			ok("Updated %s.", name)
			return nil
		}
		added, err := c.AddAgent(ctx, api.AddAgentRequest{Name: name, Description: *desc, Skills: skills})
		if err != nil {
			return err
		}
		ok("Added %s.", added.Name)
		fmt.Printf("mcp_url   http://%s/mcp/%s\n", mcpAddr(), added.Token)
		if a, err := findAgent(ctx, c, added.Name); err == nil && a.ShareURL != "" {
			fmt.Printf("card_url  %s\n", a.ShareURL)
		}
		fmt.Println("Copy the MCP URL now: you'll need it to connect the agent, and you won't be able to see it again.")
		return nil
	case cmd == "new-token" && len(args) == 1:
		c, err := adminClient()
		if err != nil {
			return err
		}
		rotated, err := c.RotateAgentToken(ctx, args[0])
		if err != nil {
			return err
		}
		ok("Made a new MCP URL for %s. The old one no longer works.", args[0])
		fmt.Printf("mcp_url   http://%s/mcp/%s\n", mcpAddr(), rotated.Token)
		fmt.Println("Copy it now: you won't be able to see it again.")
		fmt.Println("Next: run toli connect again with it.")
		return nil
	case (cmd == "disable" || cmd == "enable") && len(args) == 1:
		c, err := adminClient()
		if err != nil {
			return err
		}
		do := c.DisableAgent
		if cmd == "enable" {
			do = c.EnableAgent
		}
		if err := do(ctx, args[0]); err != nil {
			return err
		}
		ok("%sd %s.", strings.ToUpper(cmd[:1])+cmd[1:], args[0])
		return nil
	case cmd == "remove" && len(args) >= 1:
		yes, err := parseYes("agent remove", args[1:])
		if err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		name := args[0]
		if !confirm("Remove "+name+" with its peers, sessions, and tasks?", yes) {
			return nil
		}
		if err := c.RemoveAgent(ctx, name); err != nil {
			return err
		}
		ok("Removed %s.", name)
		return nil
	case cmd == "notify" && len(args) >= 1:
		fs := flag.NewFlagSet("agent notify", flag.ContinueOnError)
		command := fs.String("command", "", "run this program with each notification on stdin")
		webhook := fs.String("webhook", "", "POST each notification to this URL (https, or http to this machine)")
		none := fs.Bool("none", false, "stop pushing notifications")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NFlag() != 1 {
			return errors.New("agent notify: give one of --command, --webhook or --none")
		}
		if *command != "" {
			abs, err := filepath.Abs(*command)
			if err != nil {
				return err
			}
			*command = abs
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		before, err := findAgent(ctx, c, args[0])
		if err != nil {
			return err
		}
		set, err := c.SetAgentNotify(ctx, before.Name, api.SetNotifyRequest{Command: *command, Webhook: *webhook})
		if err != nil {
			return err
		}
		after := notifyText(&api.NotifyInfo{Command: *command, Webhook: *webhook})
		// The daemon lists webhooks by host only: compare like with like.
		same := notifyText(&api.NotifyInfo{Command: *command, Webhook: api.HostOnly(*webhook)}) == notifyText(before.Notify)
		switch was := notifyText(before.Notify); {
		case *none && before.Notify != nil:
			ok("%s's notifications aren't pushed any more (they went to %s).", before.Name, was)
		case *none:
			ok("%s's notifications aren't pushed.", before.Name)
		case before.Notify != nil && !same:
			ok("%s's notifications now go to %s (replacing %s).", before.Name, after, was)
		default:
			ok("%s's notifications now go to %s.", before.Name, after)
		}
		if set.Secret != "" {
			fmt.Printf("secret  %s\n", set.Secret)
			fmt.Println("Your webhook gets it as a Bearer token. Copy it now: you won't be able to see it again.")
		}
		return nil
	case cmd == "list" && len(args) == 0:
		c, err := adminClient()
		if err != nil {
			return err
		}
		agents, err := c.ListAgents(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tHARNESS\tDISABLED\tNOTIFY\tCARD_URL")
		for _, a := range agents {
			notify := ""
			if a.Notify != nil {
				notify = notifyText(a.Notify)
			}
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", a.Name, a.Harness, a.Disabled, notify, a.ShareURL)
		}
		return w.Flush()
	}
	return errors.New(agentUsage)
}

// notifyText is where an agent's notifications are pushed, for people:
// "command /path", "webhook https://...", or "none".
func notifyText(n *api.NotifyInfo) string {
	switch {
	case n == nil || n.Command == "" && n.Webhook == "":
		return "none"
	case n.Command != "":
		return "command " + n.Command
	}
	return "webhook " + n.Webhook
}

// findAgent looks an agent up by name.
func findAgent(ctx context.Context, c *api.Client, name string) (api.AgentInfo, error) {
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return api.AgentInfo{}, err
	}
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	return api.AgentInfo{}, fmt.Errorf("no agent %s", name)
}

// readSkills reads a JSON array of skills, the API's format:
//
//	[{"name": "Signups API", "description": "...", "tags": ["api"], "examples": ["..."]}]
//
// Unknown fields are an error, so a typo doesn't silently drop a field. The
// daemon checks the rest (every skill needs a name).
func readSkills(path string) ([]api.Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var skills []api.Skill
	if err := dec.Decode(&skills); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field == "" {
			return nil, fmt.Errorf("%s: want a JSON array of skills", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("%s: unexpected data after the skills", path)
	}
	return skills, nil
}

// parseYes parses a command's only flag, --yes.
func parseYes(name string, args []string) (bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	err := fs.Parse(args)
	return *yes, err
}

// confirm asks question on the terminal; yes skips it.
func confirm(question string, yes bool) bool {
	if yes {
		return true
	}
	fmt.Printf("%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
