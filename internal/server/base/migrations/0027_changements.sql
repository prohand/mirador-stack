-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0027 : suivi des changements des machines (SPEC § 47).
-- changements : un changement repéré (paquets mis à jour, redémarrage,
-- noyau, agent, conteneur recréé). Gardé 90 jours.
CREATE TABLE changements (
    id      INTEGER PRIMARY KEY,
    machine TEXT    NOT NULL,
    le      INTEGER NOT NULL, -- secondes Unix
    type    TEXT    NOT NULL, -- paquets, redemarrage, noyau, agent, conteneur
    detail  TEXT    NOT NULL
);
CREATE INDEX changements_machine_le ON changements(machine, le);
CREATE INDEX changements_le ON changements(le);
-- changements_etat : dernière valeur vue de chaque chose suivie (heure de
-- démarrage, noyau, version de l'agent, conteneurs, paquets), pour repérer
-- un changement même après un redémarrage du serveur.
CREATE TABLE changements_etat (
    cle    TEXT PRIMARY KEY,
    valeur TEXT NOT NULL
);
