-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0020 (SPEC § 23.14) : certificat du syslog TLS de chaque
-- collecteur. csr : dernière demande reçue (clé privée restée sur le
-- collecteur) ; certificat : chaîne PEM obtenue par ACME ; erreur : dernier
-- échec ; demande_le : « Renouveler maintenant ».
CREATE TABLE certificats_syslog (
    machine_id   INTEGER PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
    nom          TEXT    NOT NULL DEFAULT '',
    csr          TEXT    NOT NULL DEFAULT '',
    csr_recue_le INTEGER NOT NULL DEFAULT 0,
    certificat   TEXT    NOT NULL DEFAULT '',
    fin          INTEGER NOT NULL DEFAULT 0,
    obtenu_le    INTEGER NOT NULL DEFAULT 0,
    essai_le     INTEGER NOT NULL DEFAULT 0,
    erreur       TEXT    NOT NULL DEFAULT '',
    demande_le   INTEGER NOT NULL DEFAULT 0
);
