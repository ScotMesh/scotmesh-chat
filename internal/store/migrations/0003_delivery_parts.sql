-- 0003: a message longer than one LXMF packet goes out in parts, since
-- MeshChatX refuses LXMF Resources from strangers. parts_done counts the
-- parts that arrived, so a retry carries on from there instead of sending
-- the first parts again.
ALTER TABLE deliveries ADD COLUMN parts_done INTEGER NOT NULL DEFAULT 0 CHECK (parts_done >= 0);
