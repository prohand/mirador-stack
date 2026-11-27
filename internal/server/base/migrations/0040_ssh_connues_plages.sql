-- SPDX-License-Identifier: GPL-3.0-or-later
-- Adresses SSH connues (retour du 27/11) : une adresse ou une plage, pour
-- toutes les machines (machine = '') ou une seule ; gérées dans
-- Réglages › Sécurité. Les adresses « C'est moi » déjà notées sont reprises
-- (toutes les machines).
CREATE TABLE ssh_connues2 (
    id      INTEGER PRIMARY KEY,
    prefixe TEXT    NOT NULL,             -- « 203.0.113.7/32 », « 203.0.113.0/24 »
    machine TEXT    NOT NULL DEFAULT '',  -- '' : toutes les machines
    note    TEXT    NOT NULL DEFAULT '',
    par     TEXT    NOT NULL,
    le      INTEGER NOT NULL,
    UNIQUE (prefixe, machine)
);
INSERT INTO ssh_connues2(prefixe, machine, par, le)
    SELECT CASE WHEN instr(adresse, ':') > 0 THEN adresse || '/128' ELSE adresse || '/32' END, '', par, le FROM ssh_connues;
DROP TABLE ssh_connues;
ALTER TABLE ssh_connues2 RENAME TO ssh_connues;
