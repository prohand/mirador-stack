-- SPDX-License-Identifier: GPL-3.0-or-later
-- Pannes vues tout de suite par l'agent (SPEC § 98) : service systemd passé
-- en échec, processus tué faute de mémoire. Montrées dans le journal des
-- événements et sur la page d'une alerte (« Vu tout de suite »). 30 jours
-- gardés, 500 lignes au plus par machine.
CREATE TABLE pannes (
    id         INTEGER PRIMARY KEY,
    machine_id INTEGER NOT NULL,
    le         INTEGER NOT NULL, -- heure de réception (ms)
    type       TEXT    NOT NULL, -- service, oom
    objet      TEXT    NOT NULL DEFAULT '',
    texte      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX pannes_machine_le ON pannes(machine_id, le);
CREATE INDEX pannes_le ON pannes(le);
