-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0028 : registre des motifs de logs (SPEC § 52). Un motif
-- (gabarit : message dont les parties variables sont remplacées), vu la
-- première et la dernière fois, nombre de lignes. 5 000 au plus (les moins
-- récemment vus sont oubliés).
CREATE TABLE motifs_logs (
    id         INTEGER PRIMARY KEY,
    gabarit    TEXT    NOT NULL,
    premier_le INTEGER NOT NULL, -- secondes Unix
    dernier_le INTEGER NOT NULL,
    n          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX motifs_logs_dernier ON motifs_logs(dernier_le);
