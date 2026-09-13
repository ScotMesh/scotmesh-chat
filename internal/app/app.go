package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/lxmfgroup"
	"github.com/ScotMesh/scotmesh-chat/internal/nomadpage"
	"github.com/ScotMesh/scotmesh-chat/internal/rrcsrv"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Run starts the service and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config, version string, log *slog.Logger) error {
	if err := ensurePrivateDir(cfg.DataDir); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "hub.db"))
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing the store", "err", err)
		}
	}()

	admins, err := cfg.AdminHashes()
	if err != nil {
		return err
	}
	hcfg := hub.Defaults()
	hcfg.HubName = cfg.Hub.Name
	hcfg.Admins = admins
	hcfg.GroupRoom = cfg.Hub.GroupRoom
	hcfg.Retention = cfg.Hub.Retention.Duration
	hcfg.PostsPerMinute = cfg.Hub.PostsPerMinute
	hcfg.MaxBodyBytes = cfg.Hub.MaxBodyBytes
	hcfg.ReplayMax = cfg.Hub.ReplayMax
	hcfg.ReplayFirstVisit = cfg.Hub.ReplayFirstTime
	hcfg.Rooms = nil
	for _, r := range cfg.Hub.Rooms {
		hcfg.Rooms = append(hcfg.Rooms, hub.RoomConfig{Name: r.Name, Topic: r.Topic})
	}
	// The group's identity is loaded before the hub, whose replies say
	// where to send /join.
	var groupID *rns.Identity
	if cfg.Group.Enabled {
		if groupID, err = loadOrCreateIdentity(cfg.path(cfg.Group.Identity), log); err != nil {
			return err
		}
		hcfg.GroupAddress = fmt.Sprintf("%x", lxmfgroup.LXMFAddress(groupID.Hash()))
	}
	h, err := hub.New(ctx, hcfg, st, hub.WithLogger(log.With("component", "hub")))
	if err != nil {
		return err
	}

	tlog := slogPrintf{log.With("component", "reticulum")}
	t := rns.NewTransport(tlog)
	cache := &announceCache{t: t, path: filepath.Join(cfg.DataDir, "announces.json"), log: log}
	cache.restore()
	tc, err := rns.DialReconnectingTCP(cfg.Backbone, 15*time.Second, tlog)
	if err != nil {
		return fmt.Errorf("backbone %s: %w", cfg.Backbone, err)
	}
	t.AddInterface(tc)

	var wg sync.WaitGroup
	run := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil {
				log.Error("component stopped", "component", name, "err", err)
			}
		}()
	}

	var hubAddress, groupAddress, pageAddress string
	healthSources := map[string]func() map[string]uint64{"hub": h.Counters}
	if cfg.RRC.Enabled {
		id, err := loadOrCreateIdentity(cfg.path(cfg.RRC.Identity), log)
		if err != nil {
			return err
		}
		scfg := rrcsrv.Defaults()
		scfg.HubName = cfg.Hub.Name
		scfg.Version = version
		scfg.Greeting = cfg.RRC.Greeting
		scfg.GroupRoom = cfg.Hub.GroupRoom
		scfg.IncludeJoinedMemberList = cfg.RRC.IncludeJoinedMemberList
		if cfg.RRC.MaxMsgBodyBytes > 0 {
			scfg.MaxMsgBodyBytes = cfg.RRC.MaxMsgBodyBytes
		}
		if cfg.RRC.RatePerMinute > 0 {
			scfg.RatePerMinute = cfg.RRC.RatePerMinute
		}
		srv := rrcsrv.New(scfg, h, h.Subscribe("rrc", 4096), t, id.Hash(), rrcsrv.WithLogger(log.With("component", "rrc")))
		healthSources["rrc"] = func() map[string]uint64 {
			st := srv.Stats()
			return map[string]uint64{
				"frames_in": st.FramesIn.Load(), "frames_out": st.FramesOut.Load(), "frames_dropped": st.FramesDropped.Load(),
				"jobs_dropped": st.JobsDropped.Load(), "rate_limited": st.RateLimited.Load(), "bad_frames": st.BadFrames.Load(),
				"links_accepted": st.LinksAccepted.Load(), "links_closed": st.LinksClosed.Load(), "internal_errors": st.InternalErrors.Load(),
			}
		}
		dest := id.DestinationHashFor(wire.HubAspect)
		appData, err := wire.AnnounceAppData(cfg.Hub.Name)
		if err != nil {
			return err
		}
		build := func(c byte) (*rns.Packet, error) {
			return rns.BuildAnnounceWithContext(id, wire.HubAspect, appData, nil, c)
		}
		if err := t.RegisterLocal(&rns.LocalDestination{
			DestHash: dest, Identity: id, OnPacket: func(*rns.Packet) {},
			BuildAnnounce: build, LinkHooks: srv.Hooks(),
		}); err != nil {
			return fmt.Errorf("register rrc.hub: %w", err)
		}
		hubAddress = fmt.Sprintf("%x", dest)
		log.Info("RRC hub", "address", hubAddress, "identity", fmt.Sprintf("%x", id.Hash()))
		run("rrc", srv.Run)
		run("rrc-announce", func(ctx context.Context) error {
			announcer(ctx, t, log, "rrc.hub", 2*time.Second, cfg.RRC.AnnounceInterval.Duration, func() (*rns.Packet, error) { return build(rns.ContextNone) })
			return nil
		})
	}

	var pins [][]byte
	if cfg.Group.Enabled {
		id := groupID
		build := func(c byte) (*rns.Packet, error) {
			ad, err := rns.EncodeLXMFAppData([]byte(cfg.Group.DisplayName), nil)
			if err != nil {
				return nil, err
			}
			return rns.BuildAnnounceWithContext(id, lxmf.FullName(), ad, nil, c)
		}
		delivery, err := lxmf.NewDelivery(t, id, build)
		if err != nil {
			return fmt.Errorf("lxmf group: %w", err)
		}
		delivery.MaxStampCost = cfg.Group.MaxStampCost
		propNode, _ := hex.DecodeString(cfg.Group.PropagationNode) // validated in Config.Validate
		if len(propNode) == 16 {
			pins = append(pins, propNode)
		}
		gcfg := lxmfgroup.Defaults()
		gcfg.Name = cfg.Group.Name
		gcfg.GroupRoom = cfg.Hub.GroupRoom
		gcfg.PropagationNode = propNode
		if cfg.Group.JoinDigest > 0 {
			gcfg.JoinDigest = cfg.Group.JoinDigest
		}
		g := lxmfgroup.New(gcfg, h, h.Subscribe("lxmf", 4096), delivery, t, lxmfgroup.WithLogger(log.With("component", "lxmf")))
		healthSources["lxmf"] = func() map[string]uint64 {
			st := g.Stats()
			return map[string]uint64{
				"received": st.Received.Load(), "duplicates": st.Duplicates.Load(), "unknown_sender": st.UnknownSender.Load(),
				"sent": st.Sent.Load(), "propagated": st.Propagated.Load(), "retries": st.Retries.Load(), "gave_up": st.GaveUp.Load(),
				"inbox_dropped": st.InboxDropped.Load(),
			}
		}
		delivery.OnMessage = g.OnMessage
		glog := log.With("component", "lxmf")
		delivery.OnError = func(err error) { glog.Debug("lxmf delivery", "err", err) }
		groupAddress = fmt.Sprintf("%x", delivery.Hash())
		if groupAddress != hcfg.GroupAddress {
			log.Error("the LXMF group's address differs from the one the hub tells people", "delivery", groupAddress, "hub", hcfg.GroupAddress)
		}
		log.Info("LXMF group", "address", groupAddress)
		run("lxmf", g.Run)
		run("lxmf-announce", func(ctx context.Context) error {
			announcer(ctx, t, log, "lxmf.delivery", 20*time.Second, cfg.Group.AnnounceInterval.Duration, func() (*rns.Packet, error) { return build(rns.ContextNone) })
			return nil
		})
	}
	if cfg.Page.Enabled {
		id, err := loadOrCreateIdentity(cfg.path(cfg.Page.Identity), log)
		if err != nil {
			return err
		}
		banner, err := cfg.BannerLines() // already checked in Config.Validate; re-read in case the file changed
		if err != nil {
			return err
		}
		dest := id.DestinationHashFor("nomadnetwork.node")
		build := func(c byte) (*rns.Packet, error) {
			return rns.BuildAnnounceWithContext(id, "nomadnetwork.node", []byte(cfg.Page.NodeName), nil, c)
		}
		if err := t.RegisterLocal(&rns.LocalDestination{DestHash: dest, Identity: id, OnPacket: func(*rns.Packet) {}, BuildAnnounce: build}); err != nil {
			return fmt.Errorf("register page node: %w", err)
		}
		page := nomadpage.New(nomadpage.Config{Title: cfg.Page.Title, GroupRoom: cfg.Hub.GroupRoom, HubAddress: hubAddress, GroupAddress: groupAddress, WikiURL: cfg.Page.WikiURL, NodeAddress: fmt.Sprintf("%x", dest), Banner: banner},
			h, nomadpage.WithLogger(log.With("component", "page")))
		for _, path := range nomadpage.Paths {
			if err := t.RegisterRequestHandler(path, rns.AllowAll, nil, page.Handler()); err != nil {
				return fmt.Errorf("register %s: %w", path, err)
			}
		}
		pageAddress = fmt.Sprintf("%x", dest)
		log.Info("chat page", "address", pageAddress, "path", nomadpage.PathIndex)
		run("page-announce", func(ctx context.Context) error {
			announcer(ctx, t, log, "nomadnetwork.node", 40*time.Second, cfg.Page.AnnounceInterval.Duration, func() (*rns.Packet, error) { return build(rns.ContextNone) })
			return nil
		})
	}
	if len(pins) > 0 {
		t.PinDestinations(pins)
	}

	loc, err := time.LoadLocation(cfg.Timezone) // already checked in Config.Validate
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	hw := &healthWriter{path: filepath.Join(cfg.DataDir, "health.json"), version: version, started: time.Now().UTC(),
		addresses: map[string]string{"rrc_hub": hubAddress, "lxmf_group": groupAddress, "page": pageAddress},
		sources:   healthSources, connected: tc.Connected, log: log}
	bk := &backups{st: st, dir: filepath.Join(cfg.DataDir, "backups"), keep: 7, log: log, now: time.Now, location: loc}

	run("hub", h.Run)
	run("health", func(ctx context.Context) error { hw.run(ctx); return nil })
	run("backups", func(ctx context.Context) error { bk.run(ctx); return nil })
	run("backbone-watch", func(ctx context.Context) error { watchBackbone(ctx, tc, log); return nil })
	run("transport", func(ctx context.Context) error { t.Run(ctx); return nil })
	run("link-sweeper", func(ctx context.Context) error { t.RunLinkSweeper(ctx); return nil })
	run("announce-cache", func(ctx context.Context) error { cache.run(ctx); return nil })

	log.Info("scotmesh-chat running", "version", version, "backbone", cfg.Backbone)
	<-ctx.Done()
	log.Info("stopping")
	if err := tc.Close(); err != nil {
		log.Warn("closing the backbone connection", "err", err)
	}
	wg.Wait()
	return nil
}

// slogPrintf adapts slog to reticulum-go's Printf logger at debug level: the
// transport is chatty, and its lines are for diagnosing, not for the journal.
type slogPrintf struct{ l *slog.Logger }

func (s slogPrintf) Printf(format string, args ...any) {
	s.l.Debug(strings.TrimRight(fmt.Sprintf(format, args...), "\n"))
}

// watchBackbone logs the backbone's up/down transitions at info/warn level.
// reticulum-go's own transport logging (slogPrintf, above) is all at debug,
// so without this a cut-off hub looks healthy in the journal at the default
// log level; health.json's backbone_connected is the same signal for a
// health check or a status page.
func watchBackbone(ctx context.Context, tc interface{ Connected() bool }, log *slog.Logger) {
	watchBackboneEvery(ctx, tc, log, 5*time.Second)
}

func watchBackboneEvery(ctx context.Context, tc interface{ Connected() bool }, log *slog.Logger, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	was := tc.Connected()
	if !was {
		log.Warn("backbone not connected")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if now := tc.Connected(); now != was {
				if now {
					log.Info("backbone connected")
				} else {
					log.Warn("backbone connection lost")
				}
				was = now
			}
		}
	}
}
