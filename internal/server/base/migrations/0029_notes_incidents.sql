-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0029 : notes des incidents (SPEC § 54). Un incident est calculé
-- à partir des alertes (rien d'autre en base) ; il porte le numéro de sa
-- première alerte, auquel les notes sont attachées.
CREATE TABLE notes_incidents (
    id       INTEGER PRIMARY KEY,
    incident INTEGER NOT NULL, -- numéro de la première alerte de l'incident
    le       INTEGER NOT NULL, -- secondes Unix
    qui      TEXT    NOT NULL,
    texte    TEXT    NOT NULL
);
CREATE INDEX notes_incidents_incident ON notes_incidents(incident);
