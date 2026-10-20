package a2aserver_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/arjunvlama/toli/internal/a2aserver"
)

func TestInvite(t *testing.T) {
	ctx := context.Background()
	b1, url1 := setup(t)
	b2, url2 := setup(t)
	if _, err := b1.AddAgent(ctx, "backend", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.AddAgent(ctx, "web", "", nil); err != nil {
		t.Fatal(err)
	}

	inv, _, err := b1.CreateInvite(ctx, "backend", "web", a2aserver.CardURL(url1, "backend"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := b2.AcceptInvite(ctx, inv, "web", "", a2aserver.CardURL(url2, "web"))
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if p2.Name != "backend" || p2.URL != a2aserver.CardURL(url1, "backend") {
		t.Errorf("accepting side: %+v", p2)
	}
	list, err := b1.ListPeers(ctx, "backend")
	if err != nil || len(list) != 1 || list[0].Name != "web" || list[0].URL != a2aserver.CardURL(url2, "web") {
		t.Fatalf("inviting side: %+v, %v", list, err)
	}
	// Each side holds the token the other issued.
	if caller, err := b1.PeerByToken(ctx, p2.OutboundToken); err != nil || caller.ID != list[0].ID {
		t.Errorf("web's token for backend: %+v, %v", caller, err)
	}
	if caller, err := b2.PeerByToken(ctx, list[0].OutboundToken); err != nil || caller.ID != p2.ID {
		t.Errorf("backend's token for web: %+v, %v", caller, err)
	}

	// The invite is used.
	if _, err := b2.AcceptInvite(ctx, inv, "web", "other", a2aserver.CardURL(url2, "nobody")); err == nil {
		t.Error("used invite accepted again for another agent")
	}

	for _, c := range []struct {
		auth, body string
		want       int
	}{
		{"", `{"card":"x","token":"y"}`, http.StatusUnauthorized},
		{"Bearer toli_NOPE", `{}`, http.StatusBadRequest},
		{"Bearer toli_NOPE", `{"card":"x","token":"y"}`, http.StatusNotFound},
	} {
		req, _ := http.NewRequest(http.MethodPost, url1+"/a2a/backend/invite", strings.NewReader(c.body))
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("auth %q body %s: %d, want %d", c.auth, c.body, resp.StatusCode, c.want)
		}
	}
}
