package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Found is a Jellyfin server found without typing: on the Wi-Fi (the
// server answered the discovery broadcast) or on the tailnet (it answered
// /System/Info/Public on a tailnet device).
type Found struct {
	Address string // its URL, as MiniFin will use it
	ID      string
	Name    string
	Where   string // "Wi-Fi" or "tailnet"
	Version string
}

// discoveryPort is where Jellyfin listens for "who is JellyfinServer?".
const discoveryPort = 7359

// discoveryAddr is where the question is broadcast; tests point it at a
// fake server.
var discoveryAddr = &net.UDPAddr{IP: net.IPv4bcast, Port: discoveryPort}

// parseDiscovery reads one reply to the broadcast:
// {"Address":"http://192.168.0.10:8096","Id":"...","Name":"homeserver","EndpointAddress":null}.
func parseDiscovery(b []byte) (Found, bool) {
	var r struct {
		Address string `json:"Address"`
		ID      string `json:"Id"`
		Name    string `json:"Name"`
	}
	if json.Unmarshal(b, &r) != nil || r.Address == "" || r.ID == "" {
		return Found{}, false
	}
	addr := strings.TrimRight(strings.TrimSpace(r.Address), "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return Found{}, false
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = addr
	}
	return Found{Address: addr, ID: r.ID, Name: name, Where: "Wi-Fi"}, true
}

// discoverLAN broadcasts the question twice (UDP gets lost on a busy
// Wi-Fi) and collects the answers until ctx is done. Each server is
// reported once, to found, as it answers.
func discoverLAN(ctx context.Context, found func(Found)) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.SetReadDeadline(time.Now())
	}()
	ask := []byte("who is JellyfinServer?")
	if _, err := conn.WriteToUDP(ask, discoveryAddr); err != nil {
		return err
	}
	resend := time.AfterFunc(700*time.Millisecond, func() { conn.WriteToUDP(ask, discoveryAddr) })
	defer resend.Stop()
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if f, ok := parseDiscovery(buf[:n]); ok && !seen[f.ID] {
			seen[f.ID] = true
			found(f)
		}
	}
}

// peer is a tailnet device, with the full MagicDNS name that HTTPS
// certificates (tailscale serve) are issued for.
type peer struct {
	Name    string // short name
	DNSName string // name.tailnet.ts.net
	Online  bool
}

// tailnetPeers lists the tailnet's other devices from MiniScale's
// tailscale CLI, as tnet.Peers does, keeping the full DNS names.
// MINIDECK_PEERS_JSON (a file with `tailscale status --json` output)
// replaces the CLI in tests.
func tailnetPeers() ([]peer, error) {
	var out []byte
	var err error
	if f := os.Getenv("MINIDECK_PEERS_JSON"); f != "" {
		out, err = os.ReadFile(f)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err = exec.CommandContext(ctx, "/tmp/miniscale/tailscale",
			"--socket=/tmp/miniscale/tailscaled.sock", "status", "--json").Output()
	}
	if err != nil {
		return nil, err
	}
	var st struct {
		Peer map[string]struct {
			HostName string
			DNSName  string
			Online   bool
		}
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, err
	}
	var peers []peer
	for _, p := range st.Peer {
		dns := strings.TrimSuffix(p.DNSName, ".")
		name := strings.SplitN(dns, ".", 2)[0]
		if name == "" {
			name = strings.ToLower(p.HostName)
		}
		peers = append(peers, peer{Name: name, DNSName: dns, Online: p.Online})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	return peers, nil
}

// tailnetCandidates are the addresses tried on each online tailnet device:
// Jellyfin's own port, and HTTPS through tailscale serve on the usual
// ports (443; 8920, Jellyfin's HTTPS port; 8084, used when Jellyfin sits
// next to another served app such as Navidrome on 8083).
func tailnetCandidates(p peer) []string {
	out := []string{"http://" + p.Name + ":8096"}
	if p.DNSName != "" && strings.Contains(p.DNSName, ".") {
		for _, port := range []string{"", ":8920", ":8084"} {
			out = append(out, "https://"+p.DNSName+port)
		}
	}
	return out
}

// probe asks base for /System/Info/Public; it reports a Jellyfin server.
func probe(ctx context.Context, newClient func(base string) *Client, base string) (Found, bool) {
	c := newClient(base)
	type res struct {
		pi  PublicInfo
		err error
	}
	ch := make(chan res, 1)
	go func() {
		pi, err := c.Info()
		ch <- res{pi, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return Found{}, false
		}
		name := r.pi.ServerName
		if name == "" {
			name = base
		}
		return Found{Address: base, ID: r.pi.ID, Name: name, Version: r.pi.Version}, true
	case <-ctx.Done():
		return Found{}, false
	}
}

// discoverTailnet tries the candidates on every online tailnet device at
// once, and reports each server found (once per server id: one reachable
// over both HTTP and HTTPS shows up once, the first to answer).
func discoverTailnet(ctx context.Context, newClient func(base string) *Client, found func(Found)) {
	peers, err := tailnetPeers()
	if err != nil {
		return
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, p := range peers {
		if !p.Online {
			continue
		}
		for _, base := range tailnetCandidates(p) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f, ok := probe(ctx, newClient, base)
				if !ok {
					return
				}
				f.Where = "tailnet"
				mu.Lock()
				dup := seen[f.ID] && f.ID != ""
				seen[f.ID] = true
				mu.Unlock()
				if !dup {
					found(f)
				}
			}()
		}
	}
	wg.Wait()
}
