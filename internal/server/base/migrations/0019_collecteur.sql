-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0019 (SPEC § 23.13) : mirador-collector.
--   1. Binaires publiés par produit (mirador-agent, mirador-collector).
--   2. Type de la machine choisi à la création du jeton (agent, collecteur).
--   3. Équipements relevés par un collecteur (un équipement : un collecteur).
CREATE TABLE versions_binaires (
    produit         TEXT    NOT NULL,
    version         TEXT    NOT NULL,
    architecture    TEXT    NOT NULL,
    sha256          TEXT    NOT NULL,
    taille          INTEGER NOT NULL,
    retour_autorise INTEGER NOT NULL DEFAULT 0,
    signature       TEXT    NOT NULL,
    publiee_le      INTEGER NOT NULL,
    publiee_par     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (produit, version, architecture)
);
INSERT INTO versions_binaires(produit, version, architecture, sha256, taille, retour_autorise, signature, publiee_le, publiee_par)
    SELECT 'mirador-agent', version, architecture, sha256, taille, retour_autorise, signature, publiee_le, publiee_par FROM versions_agent;
DROP TABLE versions_agent;

ALTER TABLE jetons_enrolement ADD COLUMN type TEXT NOT NULL DEFAULT 'agent';

CREATE TABLE equipements_collecteur (
    machine_id    INTEGER PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
    collecteur_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    rattache_le   INTEGER NOT NULL,
    rattache_par  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX equipements_par_collecteur ON equipements_collecteur(collecteur_id);
