package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/arjunvlama/toli/internal/api"
)

const peerUsage = `usage:
  toli peer invite AGENT PEER
  toli peer accept AGENT INVITE [--name NAME]
  toli peer add AGENT PEER CARD_URL [--token TOKEN]
  toli peer token AGENT PEER TOKEN
  toli peer new-token AGENT PEER
  toli peer remove AGENT PEER [--yes]
  toli peer list AGENT`

// runPeer is `toli peer ...`: pairing, as the TUI's Peers panel does it.
// Peers are named per agent; their IDs stay internal.
func runPeer(args []string) error {
	if len(args) < 1 {
		return errors.New(peerUsage)
	}
	if isHelp(args) {
		fmt.Println(peerUsage)
		return nil
	}
	ctx := context.Background()
	cmd, args := args[0], args[1:]
	switch {
	case cmd == "invite" && len(args) == 2:
		c, err := adminClient()
		if err != nil {
			return err
		}
		agent, peer := args[0], args[1]
		inv, err := c.CreateInvite(ctx, api.CreateInviteRequest{Agent: agent, Name: peer})
		if err != nil {
			return err
		}
		ok("Created an invite for peer '%s'. It works once, within 24 hours.", peer)
		fmt.Printf("\nTo accept it, run this where peer '%s' runs:\n\n    toli peer accept <agent> %s\n\n", peer, inv.Invite)
		fmt.Printf("Send it privately: whoever has it can pair with agent '%s'.\n", agent)
		return nil
	case cmd == "accept" && len(args) >= 2:
		fs := flag.NewFlagSet("peer accept", flag.ContinueOnError)
		name := fs.String("name", "", "what to call the inviting agent (default: its own name)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		agent := args[0]
		acc, err := c.AcceptInvite(ctx, api.AcceptInviteRequest{Agent: agent, Invite: args[1], Name: *name})
		if err != nil {
			return err
		}
		ok("Paired agent '%s' with peer '%s'.", agent, acc.Peer.Name)
		return nil
	case cmd == "add" && len(args) >= 3:
		fs := flag.NewFlagSet("peer add", flag.ContinueOnError)
		token := fs.String("token", "", "the token the peer gave you, if you have it")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		agent, peer := args[0], args[1]
		added, err := c.AddPeer(ctx, api.AddPeerRequest{Agent: agent, Name: peer, URL: args[2], Token: *token})
		if err != nil {
			return err
		}
		ok("Added peer '%s' to agent '%s'.", peer, agent)
		fmt.Printf("token  %s\n", added.Token)
		fmt.Printf("Give this token to peer '%s'. Copy it now: you won't be able to see it again.\n", peer)
		if *token == "" {
			fmt.Printf("Next: when peer '%s' gives you its token, run `toli peer token %s %s <token>`.\n", peer, agent, peer)
		}
		return nil
	case cmd == "token" && len(args) == 3:
		c, p, err := peerClient(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		if err := c.SetPeerToken(ctx, p.ID, args[2]); err != nil {
			return err
		}
		ok("Set the token from peer '%s' for agent '%s'.", p.Name, args[0])
		return nil
	case cmd == "new-token" && len(args) == 2:
		c, p, err := peerClient(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		rotated, err := c.RotatePeerToken(ctx, p.ID)
		if err != nil {
			return err
		}
		ok("Made a new token for peer '%s'. The old one no longer works.", p.Name)
		fmt.Printf("token  %s\n", rotated.Token)
		fmt.Printf("Give it to peer '%s'. Copy it now: you won't be able to see it again.\n", p.Name)
		return nil
	case cmd == "remove" && len(args) >= 2:
		yes, err := parseYes("peer remove", args[2:])
		if err != nil {
			return err
		}
		c, p, err := peerClient(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		if !confirm("Remove peer "+p.Name+"? Its running tasks are canceled.", yes) {
			return nil
		}
		if err := c.RemovePeer(ctx, p.ID); err != nil {
			return err
		}
		ok("Removed peer '%s' from agent '%s'.", p.Name, args[0])
		return nil
	case cmd == "list" && len(args) == 1:
		c, err := adminClient()
		if err != nil {
			return err
		}
		peers, err := c.ListPeers(ctx, args[0])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tCONNECTED\tHAS_TOKEN\tURL\tLAST_ERROR")
		for _, p := range peers {
			connected := "-" // not checked yet
			if p.Connected != nil {
				connected = fmt.Sprint(*p.Connected)
			}
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", p.Name, connected, p.HasToken, p.URL, p.LastError)
		}
		return w.Flush()
	}
	return errors.New(peerUsage)
}

// peerClient connects to the daemon and looks a peer up by its name among
// agent's peers.
func peerClient(ctx context.Context, agent, name string) (*api.Client, api.PeerInfo, error) {
	c, err := adminClient()
	if err != nil {
		return nil, api.PeerInfo{}, err
	}
	peers, err := c.ListPeers(ctx, agent)
	if err != nil {
		return nil, api.PeerInfo{}, err
	}
	for _, p := range peers {
		if p.Name == name {
			return c, p, nil
		}
	}
	return nil, api.PeerInfo{}, fmt.Errorf("agent '%s' has no peer '%s'", agent, name)
}
