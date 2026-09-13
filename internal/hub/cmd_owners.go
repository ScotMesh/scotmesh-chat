package hub

import (
	"fmt"
)

// Hub owners' commands.

func cmdRelease(c *cmd) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	_, sk, err := CleanName(c.args[0])
	if err != nil {
		return c.fail("%s isn't a name anyone could hold", c.args[0])
	}
	n, ok, err := c.tx.NameBySkeleton(sk)
	if err != nil {
		return err
	}
	if !ok {
		return c.fail("nobody holds %s", c.args[0])
	}
	if _, _, err := c.tx.ReleaseName(n.Identity); err != nil {
		return err
	}
	guest := GuestName(n.Identity)
	c.out.emit(NameEvent{Identity: n.Identity, Old: n.Name, New: guest})
	c.out.then(func() { c.h.renamePresence(n.Identity, guest) })
	c.out.emit(NoticeEvent{Identity: n.Identity, Text: fmt.Sprintf("A hub operator has released the name %s. You are shown as %s until you choose another with /nick.", n.Name, guest)})
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "release", n.Name, hexID(n.Identity)); err != nil {
		return err
	}
	c.say("released %s (held by %s)", n.Name, hexID(n.Identity))
	return nil
}

func cmdStats(c *cmd) error {
	s := &c.h.stats
	people := 0
	for _, members := range c.h.presence {
		people += len(members)
	}
	group, err := c.tx.Members()
	if err != nil {
		return err
	}
	c.say("posts=%d duplicates=%d refusals=%d commands=%d internal_errors=%d",
		s.Posts.Load(), s.Duplicates.Load(), s.Refusals.Load(), s.Commands.Load(), s.InternalErrors.Load())
	c.say("rooms_with_people=%d rrc_presences=%d group_members=%d", len(c.h.presence), people, len(group))
	for _, sub := range c.h.subs {
		c.say("subscriber %s: queued=%d dropped=%d", sub.name, len(sub.ch), sub.Dropped())
	}
	return nil
}
