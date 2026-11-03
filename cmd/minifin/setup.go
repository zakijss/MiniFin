package main

import (
	"context"
	"fmt"
	"image"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
	"github.com/zakijss/minideck/version"
)

func versionText() string { return version.Version }

// Setup asks for as little typing as it can: servers on the Wi-Fi answer
// Jellyfin's discovery broadcast, servers on the tailnet are found by
// asking each device, and sign-in is Quick Connect (a code approved on the
// phone) unless the user picks a password.

// setup finds a server and signs in; false if the user gave up.
func (a *app) setup(first bool) bool {
	for {
		base, info, ok := a.pickServer(first)
		if !ok {
			return false
		}
		ar, r := a.signIn(base, info)
		switch r {
		case signInCancelled:
			return false
		case signInOtherServer:
			continue
		}
		if a.saveLogin(base, info, ar) {
			return true
		}
	}
}

// signInAgain signs in to the same server (the token stopped working).
func (a *app) signInAgain() bool {
	if a.cfg.Server == "" {
		return a.setup(false)
	}
	info := PublicInfo{ServerName: a.cfg.ServerName, ID: a.cfg.ServerID}
	ar, r := a.signIn(a.cfg.Server, info)
	switch r {
	case signInOK:
		return a.saveLogin(a.cfg.Server, info, ar)
	case signInOtherServer:
		return a.setup(false)
	}
	return false
}

// saveLogin keeps the server in settings.json and the token, sealed, in
// secrets.enc.
func (a *app) saveLogin(base string, info PublicInfo, ar AuthResult) bool {
	a.cfg.Server, a.cfg.ServerName, a.cfg.ServerID = base, info.ServerName, info.ID
	if ar.ServerID != "" {
		a.cfg.ServerID = ar.ServerID
	}
	a.cfg.UserID, a.cfg.UserName = ar.User.ID, ar.User.Name
	a.sec["token"] = ar.AccessToken
	if err := secrets.Save(filepath.Join(a.dir, "secrets.enc"), a.sec); err != nil {
		a.scr.Notice(a.input, "Couldn't save the sign-in", errText(err))
		return false
	}
	if err := saveSettings(a.dir, &a.cfg); err != nil {
		a.scr.Notice(a.input, "Couldn't save", errText(err))
		return false
	}
	a.c.SetLogin(base, ar.AccessToken, ar.User.ID)
	log.Printf("signed in to %s as %s", info.ServerName, ar.User.Name)
	return true
}

// newClient is a client for trying a server before sign-in.
func (a *app) newClient(base string) *Client { return NewClient(base, "", "", a.cfg.DeviceID) }

// pickServer shows the servers found and the other ways in, and returns
// the one chosen with what it says about itself.
func (a *app) pickServer(first bool) (string, PublicInfo, bool) {
	var servers []Found
	searching := 0
	gen := 0
	found := make(chan func(), 32)
	scan := func() {
		gen++
		g := gen
		servers = nil
		searching = 2
		// sends give up once this screen is long gone
		gone := make(chan struct{})
		time.AfterFunc(15*time.Second, func() { close(gone) })
		send := func(f func()) {
			select {
			case found <- f:
			case <-gone:
			}
		}
		add := func(f Found) {
			send(func() {
				if g != gen {
					return
				}
				for _, s := range servers {
					if s.Address == f.Address || s.ID == f.ID && s.Where == f.Where {
						return
					}
				}
				servers = append(servers, f)
			})
		}
		finish := func() {
			send(func() {
				if g == gen {
					searching--
				}
			})
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			if err := discoverLAN(ctx, add); err != nil {
				log.Printf("discovery: %v", err)
			}
			finish()
		}()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			discoverTailnet(ctx, a.newClient, add)
			finish()
		}()
	}
	scan()
	l := ui.List{}
	for {
		a.drawServers(&l, servers, searching, first)
		a.scr.Present()
		var tick <-chan time.Time
		if searching > 0 {
			tick = time.After(250 * time.Millisecond)
		}
		select {
		case apply := <-found:
			apply()
			continue
		case <-tick:
			continue
		case ev, ok := <-a.input:
			if !ok {
				return "", PublicInfo{}, false
			}
			if !ev.Pressed {
				continue
			}
			switch ev.Button {
			case ui.B, ui.Menu:
				return "", PublicInfo{}, false
			case ui.Y:
				scan()
			case ui.A:
				sel := l.Selected()
				if sel == nil {
					continue
				}
				var base string
				var info PublicInfo
				var err error
				switch v := sel.Value.(type) {
				case Found:
					a.scr.Alert("Connecting", "Asking "+v.Name+" who it is…")
					base = v.Address
					info, err = a.newClient(base).Info()
				case string:
					if v == "pick" {
						base, info, err = a.pickDevice()
					} else {
						base, info, err = a.typeAddress()
					}
					if base == "" && err == nil {
						continue
					}
				}
				if err != nil {
					a.scr.Notice(a.input, "That didn't work", errText(err))
					continue
				}
				return base, info, true
			default:
				l.Handle(ev)
			}
		}
	}
}

// drawServers draws the server list: the ones found, then the other ways
// in.
func (a *app) drawServers(l *ui.List, servers []Found, searching int, first bool) {
	items := make([]ui.Item, 0, len(servers)+2)
	for _, f := range servers {
		sub := strings.TrimPrefix(strings.TrimPrefix(f.Address, "http://"), "https://") + " · on your " + f.Where
		items = append(items, ui.Item{Label: f.Name, Sub: sub, Icon: "server", Value: f})
	}
	items = append(items,
		ui.Item{Label: "Pick a tailnet device", Sub: "MiniFin tries Jellyfin's ports on it", Icon: "list", Tint: ui.Muted, Value: "pick"},
		ui.Item{Label: "Type the address", Sub: "For example 192.168.0.10:8096", Icon: "plus", Tint: ui.Muted, Value: "type"})
	l.Items = items
	sub := "Looking on your Wi-Fi and tailnet"
	if searching <= 0 {
		switch len(servers) {
		case 0:
			sub = "None found: pick a device or type the address"
		case 1:
			sub = "Found 1 server"
		default:
			sub = fmt.Sprintf("Found %d servers", len(servers))
		}
	}
	title, back := "Your Jellyfin server", "Back"
	if first {
		title, back = "Welcome to MiniFin", "Exit"
	}
	top := a.scr.StaticFrame(title, sub, ui.Hint{Button: "A", Label: "Choose"}, ui.Hint{Button: "Y", Label: "Look again"}, ui.Hint{Button: "B", Label: back})
	area := listArea(top)
	l.Draw(a.scr, area)
	if searching > 0 && len(servers) == 0 {
		a.scr.Loading(area.Min.Y+2*66+24, "Looking for servers")
	}
}

// typeAddress asks for the server's address with the keyboard.
func (a *app) typeAddress() (string, PublicInfo, error) {
	v, ok := a.scr.Prompt(a.input, "Server address", strings.TrimPrefix(a.cfg.Server, "http://"),
		"Jellyfin's address: 192.168.0.10:8096, or https://... with its port", false)
	if !ok || strings.TrimSpace(v) == "" {
		return "", PublicInfo{}, nil
	}
	base := tnet.NormalizeURL(v, "8096")
	a.scr.Alert("Connecting", "Asking "+strings.TrimPrefix(base, "http://")+" who it is…")
	info, err := a.newClient(base).Info()
	return base, info, err
}

// pickDevice lists the tailnet's devices and tries Jellyfin's ports on the
// one picked.
func (a *app) pickDevice() (string, PublicInfo, error) {
	u, ok := a.scr.PickHost(a.input, tnet.HostChoices, "8096")
	if !ok {
		return "", PublicInfo{}, nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(u, "http://"), ":8096")
	a.scr.Alert("Looking for Jellyfin", "Trying Jellyfin's ports on "+name+"…")
	cands := []string{u}
	if peers, err := tailnetPeers(); err == nil {
		for _, p := range peers {
			if p.Name == name {
				cands = tailnetCandidates(p)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type res struct {
		f  Found
		ok bool
	}
	ch := make(chan res, len(cands))
	for _, c := range cands {
		go func() {
			f, ok := probe(ctx, a.newClient, c)
			ch <- res{f, ok}
		}()
	}
	for range cands {
		if r := <-ch; r.ok {
			return r.f.Address, PublicInfo{ServerName: r.f.Name, ID: r.f.ID, Version: r.f.Version}, nil
		}
	}
	return "", PublicInfo{}, screenError("No Jellyfin answered on " + name + " (ports 8096, 443, 8920 and 8084). Check that the tailnet policy lets this device reach it, or type the address.")
}

const (
	signInOK = iota
	signInCancelled
	signInOtherServer
)

// signIn offers Quick Connect and a password, for the server at base.
func (a *app) signIn(base string, info PublicInfo) (AuthResult, int) {
	c := a.newClient(base)
	a.scr.Alert("Connecting", "Asking "+serverName(info, base)+" how to sign in…")
	qcOn, qcErr := c.QuickConnectEnabled()
	l := ui.List{}
	for {
		a.drawSignIn(&l, info, base, qcOn, qcErr)
		a.scr.Present()
		ev, ok := <-a.input
		if !ok {
			return AuthResult{}, signInCancelled
		}
		if !ev.Pressed {
			continue
		}
		switch ev.Button {
		case ui.B:
			return AuthResult{}, signInOtherServer
		case ui.Menu:
			return AuthResult{}, signInCancelled
		case ui.A:
			switch l.Selected().Value {
			case "qc":
				if !qcOn {
					a.scr.Notice(a.input, "Quick Connect is off", "An administrator can turn it on in Jellyfin: Dashboard > General > Quick Connect. Or sign in with a password.")
					continue
				}
				if ar, ok := a.quickConnect(c, serverName(info, base)); ok {
					return ar, signInOK
				}
			case "pw":
				if ar, ok := a.passwordSignIn(c); ok {
					return ar, signInOK
				}
			case "other":
				return AuthResult{}, signInOtherServer
			}
		default:
			l.Handle(ev)
		}
	}
}

// drawSignIn draws the ways to sign in.
func (a *app) drawSignIn(l *ui.List, info PublicInfo, base string, qcOn bool, qcErr error) {
	qc := ui.Item{Label: "Quick Connect", Sub: "Approve a code in Jellyfin on your phone", Icon: "check", Value: "qc"}
	if !qcOn {
		qc.Sub, qc.Tint = "Off on this server (Dashboard > General)", ui.Faint
		if qcErr != nil {
			qc.Sub = "Couldn't ask the server"
		}
	}
	l.Items = []ui.Item{qc,
		{Label: "User name and password", Sub: "Typed with the keyboard", Icon: "person", Value: "pw"},
		{Label: "Another server", Sub: "Back to the list of servers", Icon: "server", Tint: ui.Muted, Value: "other"}}
	sub := serverName(info, base)
	if info.Version != "" {
		sub += " · Jellyfin " + info.Version
	}
	top := a.scr.StaticFrame("Sign in", sub, ui.Hint{Button: "A", Label: "Choose"}, ui.Hint{Button: "B", Label: "Back"})
	l.Draw(a.scr, listArea(top))
}

func serverName(info PublicInfo, base string) string {
	if info.ServerName != "" {
		return info.ServerName
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// passwordSignIn asks for the user name and password and signs in. The
// password is used once and kept nowhere.
func (a *app) passwordSignIn(c *Client) (AuthResult, bool) {
	user, ok := a.scr.Prompt(a.input, "User name", a.cfg.UserName, "Your user name on Jellyfin", false)
	if !ok || user == "" {
		return AuthResult{}, false
	}
	pw, ok := a.scr.Prompt(a.input, "Password", "", "START signs in (with no password, just START). It isn't kept.", true)
	if !ok {
		return AuthResult{}, false
	}
	a.scr.Alert("Signing in", "Checking your user name and password…")
	ar, err := c.AuthenticateByName(user, pw)
	if err != nil {
		a.scr.Notice(a.input, "Couldn't sign in", errText(err))
		return AuthResult{}, false
	}
	return ar, true
}

// quickConnect shows a Quick Connect code until someone approves it in
// Jellyfin (on a phone or computer, signed in as the user), then signs in.
func (a *app) quickConnect(c *Client, server string) (AuthResult, bool) {
	type result struct {
		qc   QuickConnect
		ar   *AuthResult
		err  error
		poll bool
	}
	results := make(chan result, 4)
	var qc QuickConnect
	var errMsg string
	expired, waiting := false, true
	start := func() {
		qc, errMsg, expired, waiting = QuickConnect{}, "", false, true
		go func() {
			q, err := c.QuickConnectInitiate()
			results <- result{qc: q, err: err}
		}()
	}
	start()
	polling := false
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	for {
		a.drawQuickConnect(server, qc, errMsg, expired, waiting)
		a.scr.Present()
		var tick <-chan time.Time
		if waiting {
			tick = time.After(250 * time.Millisecond)
		}
		select {
		case r := <-results:
			switch {
			case r.ar != nil && r.err == nil:
				return *r.ar, true
			case r.err == errCodeExpired:
				expired, waiting = true, false
			case r.err != nil:
				errMsg, waiting = errText(r.err), false
				if r.poll && isOffline(r.err) {
					errMsg, waiting = "", true // a Wi-Fi blip: keep polling
				}
			case !r.poll:
				qc, waiting = r.qc, true
			}
			polling = false
		case <-poll.C:
			if qc.Secret == "" || polling || expired || !waiting {
				continue
			}
			polling = true
			secret := qc.Secret
			go func() {
				q, err := c.QuickConnectState(secret)
				if err == nil && q.Authenticated {
					ar, err := c.AuthenticateQuickConnect(secret)
					results <- result{ar: &ar, err: err, poll: true}
					return
				}
				results <- result{qc: q, err: err, poll: true}
			}()
		case <-tick:
		case ev, ok := <-a.input:
			if !ok {
				return AuthResult{}, false
			}
			if !ev.Pressed {
				continue
			}
			switch ev.Button {
			case ui.B, ui.Menu:
				return AuthResult{}, false
			case ui.A:
				if expired || errMsg != "" {
					start()
				}
			}
		}
	}
}

func (a *app) drawQuickConnect(server string, qc QuickConnect, errMsg string, expired, waiting bool) {
	s := a.scr
	hints := []ui.Hint{{Button: "B", Label: "Cancel"}}
	if expired || errMsg != "" {
		hints = append([]ui.Hint{{Button: "A", Label: "New code"}}, hints...)
	}
	top := s.StaticFrame("Quick Connect", server, hints...)
	area := image.Rect(20, top, ui.W-20, contentBottom)
	if qc.Code == "" {
		if errMsg != "" {
			drawMessage(s, area, errMsg, ui.Muted)
		} else {
			drawLoading(s, area, "Asking for a code")
		}
		return
	}
	y := top + 6
	for _, l := range wrapLines(ui.Body, "Approve it in Jellyfin on your phone: your profile, then Quick Connect, then type this code.", ui.W-60, 3) {
		s.TextCenter(ui.W/2, y, l, ui.Body, ui.Text)
		y += ui.LineHeight(ui.Body)
	}
	y += 14
	// the code, one big digit per key
	code := qc.Code
	const kw, kh, gap = 76, 100, 10
	x := (ui.W - len(code)*kw - (len(code)-1)*gap) / 2
	for i, ch := range code {
		r := image.Rect(x+i*(kw+gap), y, x+i*(kw+gap)+kw, y+kh)
		s.Panel(r, 12)
		lh := ui.LineHeight(ui.Display)
		bigText(s, (r.Min.X+r.Max.X)/2, r.Min.Y+(kh-lh*2)/2+2, string(ch), ui.Display, 2, ui.Text)
	}
	y += kh + 18
	switch {
	case expired:
		s.TextCenter(ui.W/2, y, "The code expired. Press A for a new one.", ui.Body, ui.Warn)
	case errMsg != "":
		for _, l := range wrapLines(ui.Small, errMsg, ui.W-60, 2) {
			s.TextCenter(ui.W/2, y, l, ui.Small, ui.Bad)
			y += ui.LineHeight(ui.Small)
		}
	case waiting:
		s.Loading(y, "Waiting for you to approve it")
	}
}

// ---- Settings ----

type settingsPage struct{ list ui.List }

func newSettingsPage() *settingsPage { return &settingsPage{} }

func (p *settingsPage) busy() bool { return false }

func (p *settingsPage) rows(a *app) []ui.Item {
	server := "Not signed in"
	if a.cfg.UserName != "" {
		server = a.cfg.UserName + " on " + a.serverLabel()
	}
	quality := ui.Item{Label: "Streaming quality", Icon: "wifi", Detail: rateText(a.cfg.kbps()),
		Sub: "About " + sizeText(streamBytes(a.cfg.kbps(), 3600)) + " an hour"}
	subs := ui.Item{Label: "Subtitles", Icon: "list"}
	switch a.cfg.Subtitles {
	case "off":
		subs.Detail, subs.Sub = "Off", "Pick them on a video's page"
	case "on":
		subs.Detail, subs.Sub = "On", "The default track, or the first"
	default:
		subs.Detail, subs.Sub = "Server's choice", "From your profile"
	}
	dir := a.cfg.downloadDir(a.dir)
	where := ui.Item{Label: "Downloads folder", Icon: "disc", Detail: "App folder"}
	if a.cfg.Downloads == "videos" {
		where.Detail = "Videos"
	}
	where.Sub = strings.TrimPrefix(dir, "/mnt/SDCARD/")
	if free := cardFree(dir); free >= 0 {
		where.Sub += " · " + sizeText(free) + " free"
	}
	player := ui.Item{Label: "Video player", Icon: "play", Detail: "Auto"}
	switch {
	case a.cfg.Player == "ffplay":
		player.Detail, player.Sub = "ffplay", "Always Onion's ffplay"
	case a.pl.ownInstalled():
		player.Sub = "minifin-player, else ffplay"
	default:
		player.Sub, player.Tint = "No minifin-player: ffplay", ui.Faint
	}
	return []ui.Item{
		{Label: "Server", Sub: server, Icon: "server", Chevron: true, Value: "server"},
		quality, subs, where, player,
		{Label: "Clear pictures", Sub: "Posters kept on the card; they download again", Icon: "cross", Value: "pictures"},
		ui.AppearanceItem(),
	}
}

func (p *settingsPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	sel := p.list.Sel
	p.list.Items = p.rows(a)
	p.list.Sel = min(sel, len(p.list.Items)-1)
	top := s.StaticFrame("Settings", "MiniFin "+versionText(), ui.Hint{Button: "A", Label: "Change"}, ui.Hint{Button: "B", Label: "Back"})
	p.list.Draw(s, listArea(top))
}

func (p *settingsPage) handle(a *app, ev ui.Event) bool {
	it := p.list.Selected()
	if it == nil {
		return false
	}
	step := 0
	switch ev.Button {
	case ui.A, ui.Right:
		step = 1
	case ui.Left:
		step = -1
	default:
		return p.list.Handle(ev)
	}
	old := a.cfg
	switch it.Label {
	case "Streaming quality":
		i := 0
		for j, r := range videoRates {
			if r == a.cfg.kbps() {
				i = j
			}
		}
		a.cfg.VideoKbps = videoRates[(i+step+len(videoRates))%len(videoRates)]
	case "Subtitles":
		modes := []string{"", "off", "on"}
		i := 0
		for j, m := range modes {
			if m == a.cfg.Subtitles {
				i = j
			}
		}
		a.cfg.Subtitles = modes[(i+step+len(modes))%len(modes)]
	case "Downloads folder":
		if a.cfg.Downloads == "videos" {
			a.cfg.Downloads = ""
		} else {
			a.cfg.Downloads = "videos"
		}
		a.dl.SetDir(a.cfg.downloadDir(a.dir))
	case "Video player":
		if a.cfg.Player == "ffplay" {
			a.cfg.Player = ""
		} else {
			a.cfg.Player = "ffplay"
		}
		a.pl.force = a.cfg.Player
	default:
		if ev.Button != ui.A {
			return true
		}
		switch it.Value {
		case "server":
			a.account()
		case "pictures":
			if a.scr.Confirm(a.input, "Clear the pictures?", "They download again as they're shown.", "Clear") && a.img.disk != nil {
				a.img.disk.Prune(0)
			}
		default:
			if it.Label == "Appearance" {
				a.scr.Appearance(a.input)
			}
		}
		return true
	}
	if err := saveSettings(a.dir, &a.cfg); err != nil {
		a.cfg = old
		a.scr.Notice(a.input, "Couldn't save", errText(err))
	}
	return true
}

// account is Settings > Server: sign out, or another server or user.
func (a *app) account() {
	i, ok := a.scr.Choose(a.input, "Server", []ui.Item{
		{Label: "Another server or user", Sub: "Sign in again", Icon: "server"},
		{Label: "Sign out", Sub: "Downloads stay and play offline", Icon: "cross", Tint: ui.Bad},
	})
	if !ok {
		return
	}
	if i == 1 {
		if !a.scr.Confirm(a.input, "Sign out?", "MiniFin forgets the sign-in on this device. Downloads stay.", "Sign out") {
			return
		}
		if err := a.c.Logout(); err != nil {
			log.Printf("sign out: %v", err)
		}
		a.sec["token"] = ""
		secrets.Save(filepath.Join(a.dir, "secrets.enc"), a.sec)
		a.cfg.UserID = ""
		saveSettings(a.dir, &a.cfg)
		a.c.SetLogin(a.cfg.Server, "", "")
	}
	if a.setup(false) {
		a.goOnline()
	} else if !a.c.SignedIn() {
		a.goOffline()
	}
}
