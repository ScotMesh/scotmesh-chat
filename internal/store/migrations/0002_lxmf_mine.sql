-- 0002: a member may ask for their own messages from RRC and the page to be
-- delivered to them by LXMF too, so their LXMF conversation shows everything.
ALTER TABLE identities ADD COLUMN lxmf_mine INTEGER NOT NULL DEFAULT 0 CHECK (lxmf_mine IN (0, 1));
