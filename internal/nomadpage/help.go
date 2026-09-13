package nomadpage

import "strings"

// --- help ---------------------------------------------------------------------

func (p *Page) help(v view) string {
	var b builder
	b.line("#!c=3600")
	b.line("`c`!`F2c8How to join %s`f`!", esc(p.cfg.Title))
	b.line("`a")
	b.line("-")
	b.line("There's one conversation, #%s, and three ways into it. Everyone sees the same messages whichever they use.", esc(p.cfg.GroupRoom))
	b.blank()
	b.line(">This page")
	b.line("Read without doing anything. To post, identify yourself to this node (the fingerprint button in MeshChatX, or \"Identify when connecting\" in NomadNet), then reload.")
	b.blank()
	b.line(">An LXMF messenger")
	b.line("From Sideband, MeshChatX, NomadNet or Columba, send `!/join`! to `!%s`!. Messages arrive like any conversation, as \"Name: message\". Send /help there for commands.", esc(p.cfg.GroupAddress))
	b.blank()
	b.line(">An RRC client")
	b.line("In MeshChatX, NomadNet, rrc-tui or WeeChat, connect to hub `!%s`! and join `!%s`!. When you reconnect you get what was said since you left.", esc(p.cfg.HubAddress), esc(p.cfg.GroupRoom))
	b.blank()
	b.line(">Names")
	b.line("Your name is tied to your Reticulum identity and is the same everywhere. Nobody else can take it or a lookalike of it. Free it with /forget, or on %s.", link("Your name & settings", PathMe, ""))
	b.blank()
	b.line(">Commands")
	b.line("The same commands work everywhere: here in Say, in the LXMF group and in the RRC room. Start them with / or !, which also works in NomadNet's RRC screen. Type /help to see them.")
	b.blank()
	nav := []string{link("Back to the chat", PathIndex, v.vars())}
	if p.cfg.WikiURL != "" {
		nav = append(nav, fullLink("Everything about the chat, on the wiki", p.cfg.WikiURL))
	}
	b.line("%s", strings.Join(nav, "  ·  "))
	return b.String()
}
