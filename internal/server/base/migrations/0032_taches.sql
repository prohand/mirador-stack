-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0032 (SPEC § 80, demande du 20/11) : tâches planifiées suivies.
-- Sources : timers systemd, conteneurs Docker ponctuels, sauvegardes
-- Proxmox VE / Backup Server (lus par l'agent), ou signal « ping » envoyé par
-- la tâche elle-même (curl). Pour un ping, seul le SHA-256 du jeton est gardé.
CREATE TABLE taches (
    id             INTEGER PRIMARY KEY,
    cle            TEXT    NOT NULL UNIQUE,  -- machine|source|nom (agent) ou ping|<id>
    nom            TEXT    NOT NULL,
    source         TEXT    NOT NULL,         -- systemd, docker, pve, pbs, ping
    machine        TEXT    NOT NULL DEFAULT '',
    periode        INTEGER NOT NULL DEFAULT 0, -- secondes ; 0 = d'après le timer (systemd)
    marge          INTEGER NOT NULL DEFAULT 3600,
    gravite        TEXT    NOT NULL DEFAULT 'warning',
    jeton          TEXT    NOT NULL DEFAULT '', -- ping : SHA-256 du jeton (hex)
    cree_le        INTEGER NOT NULL,
    -- Dernier signal reçu (ping seulement ; les tâches lues par l'agent
    -- sont dans les métriques).
    debut          INTEGER NOT NULL DEFAULT 0,
    fin            INTEGER NOT NULL DEFAULT 0,
    code           INTEGER NOT NULL DEFAULT 0,
    message        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX taches_jeton ON taches(jeton) WHERE jeton != '';
