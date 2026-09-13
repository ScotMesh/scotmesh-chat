package hub

// Ranks and the rank rule (ADR 0008).

import (
	"bytes"
	"fmt"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Rank is how much someone can do on the hub.
type Rank int

// Ranks, lowest first.
const (
	RankMember Rank = iota
	RankMod
	RankAdmin
	// RankOwner is an identity listed as an admin in the config file. It
	// can't be granted or taken away from chat.
	RankOwner
)

func (r Rank) String() string {
	switch r {
	case RankMod:
		return "mod"
	case RankAdmin:
		return "admin"
	case RankOwner:
		return "owner"
	}
	return "member"
}

// Title is the rank as shown on a profile: "" for members.
func (r Rank) Title() string {
	if r == RankMember {
		return ""
	}
	return r.String()
}

func rankOf(stored store.Rank) Rank {
	switch stored {
	case store.RankMod:
		return RankMod
	case store.RankAdmin:
		return RankAdmin
	}
	return RankMember
}

func (r Rank) stored() store.Rank {
	switch r {
	case RankMod:
		return store.RankMod
	case RankAdmin:
		return store.RankAdmin
	}
	return store.RankMember
}

// isOwner reports whether an identity is one of the config file's admins.
func (h *Hub) isOwner(id []byte) bool {
	for _, a := range h.cfg.Admins {
		if bytes.Equal(a, id) {
			return true
		}
	}
	return false
}

// identityRank is what one identity can do. An owner's own identity is an
// owner; the other apps linked to an owner are admins (ADR 0007: owner
// powers stay with the identities in the config file).
func (h *Hub) identityRank(id []byte, stored store.Rank, personHasOwner bool) Rank {
	switch {
	case h.isOwner(id):
		return RankOwner
	case personHasOwner:
		return max(RankAdmin, rankOf(stored))
	}
	return rankOf(stored)
}

// Action is something the rank rule decides.
type Action int

// Actions, with who may take them.
const (
	ActKick        Action = iota // mods and up, on lower ranks
	ActBan                       // mods and up, on lower ranks
	ActUnban                     // mods and up
	ActListBans                  // mods and up
	ActRename                    // mods and up, on lower ranks
	ActModlog                    // mods and up
	ActMakeMod                   // admins and up, on lower ranks
	ActMakeAdmin                 // admins and up, on lower ranks
	ActDemote                    // admins and up, on lower ranks
	ActDevicesOf                 // yourself, or mods and up on lower ranks
	ActUnlinkOther               // yourself, or mods and up on lower ranks
	ActRelease                   // owners
	ActStats                     // owners
	ActDelete                    // yourself, or mods and up on lower ranks
)

var actionNames = map[Action]string{
	ActKick: "kick", ActBan: "ban", ActUnban: "unban", ActListBans: "list bans", ActRename: "rename",
	ActModlog: "read the mod log", ActMakeMod: "make a mod", ActMakeAdmin: "make an admin", ActDemote: "demote",
	ActDevicesOf: "see the apps of", ActUnlinkOther: "unlink an app of", ActRelease: "release a name", ActStats: "see hub counters",
	ActDelete: "remove the messages of",
}

func (a Action) String() string { return actionNames[a] }

// minRank is the lowest rank that may take an action at all.
func (a Action) minRank() Rank {
	switch a {
	case ActMakeMod, ActMakeAdmin, ActDemote:
		return RankAdmin
	case ActRelease, ActStats:
		return RankOwner
	case ActDevicesOf, ActUnlinkOther, ActDelete:
		return RankMember // on yourself; others need RankMod, below
	}
	return RankMod
}

// hasTarget reports whether an action is on someone.
func (a Action) hasTarget() bool {
	switch a {
	case ActUnban, ActListBans, ActModlog, ActRelease, ActStats:
		return false
	}
	return true
}

// may decides whether actor may take action on target (ignored for actions
// without one). Refusals come back as a UserError to show as it is.
//
// The rank rule: nobody acts on someone of equal or higher rank, compared
// person to person, and nobody acts on an owner from chat. Owners act on
// everyone else.
func may(actor Person, action Action, target *Person) error {
	if actor.Rank < action.minRank() {
		return refuse("Only %ss and up can %s.", action.minRank(), action)
	}
	if !action.hasTarget() || target == nil {
		return nil
	}
	if actor.PersonID != 0 && actor.PersonID == target.PersonID {
		switch action {
		case ActDevicesOf, ActUnlinkOther, ActDelete:
			return nil
		}
		return refuse("You can't %s yourself.", action)
	}
	if (action == ActDevicesOf || action == ActUnlinkOther || action == ActDelete) && actor.Rank < RankMod {
		return refuse("Only mods and up can %s someone else.", action)
	}
	if target.PersonRank == RankOwner {
		return refuse("%s is a hub owner; that can only be changed in the hub's config file.", target.Name)
	}
	if actor.Rank != RankOwner && target.PersonRank >= actor.Rank {
		return refuse("%s is %s, which is not below you (%s).", target.Name, article(target.PersonRank), actor.Rank)
	}
	return nil
}

// mayActOnRoomTarget guards a room operator's action on someone else
// (kicking, banning or deopping) against reaching hub staff. Being a room
// op already includes hub mods and up (see isRoomOp), but rrcd's own
// per-room roles (founder, granted op) are unrelated to hub rank, and a
// founder or granted op is ordinarily a plain member acting on other plain
// members — that's the whole point of room-level operators, and this must
// not interfere with it. It only steps in once the target is hub staff
// (mod and up): without it, a room op with no hub rank at all could kick,
// ban or deop an admin or owner who happens to be in their room.
func (h *Hub) mayActOnRoomTarget(tx *store.Tx, actor Person, target []byte) error {
	t, err := h.person(tx, target)
	if err != nil {
		return err
	}
	if t.PersonRank < RankMod {
		return nil
	}
	if actor.PersonID != 0 && actor.PersonID == t.PersonID {
		return nil // acting on your own other app is fine
	}
	if t.PersonRank == RankOwner {
		return refuse("%s is a hub owner; that can only be changed in the hub's config file.", t.Name)
	}
	if actor.Rank != RankOwner && t.PersonRank >= actor.Rank {
		return refuse("%s is %s, which is not below you (%s).", t.Name, article(t.PersonRank), actor.Rank)
	}
	return nil
}

func article(r Rank) string {
	switch r {
	case RankAdmin, RankOwner:
		return "an " + r.String()
	case RankMod:
		return "a mod"
	}
	return "a member"
}

// personRank is a person's rank for the rank rule: the highest of its
// identities, so an owner's linked apps can't be acted on either.
func (h *Hub) personRank(ids []store.Identity, stored store.Rank) Rank {
	r := rankOf(stored)
	for _, i := range ids {
		if h.isOwner(i.ID) {
			return RankOwner
		}
	}
	return r
}

func (p Person) String() string { return fmt.Sprintf("%s (%s)", p.Name, p.Rank) }
