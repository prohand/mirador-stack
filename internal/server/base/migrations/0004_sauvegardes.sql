-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0004 : sauvegardes (étape 3, partie 6).
-- Une ligne par sauvegarde : demandée (commande, interface) ou planifiée.
-- Sert aussi à l'alerte « dernière sauvegarde trop ancienne ou en échec ».
CREATE TABLE sauvegardes (
    id            INTEGER PRIMARY KEY,
    demandee_le   INTEGER NOT NULL,
    debut         INTEGER NOT NULL DEFAULT 0,
    fin           INTEGER NOT NULL DEFAULT 0,
    statut        TEXT    NOT NULL DEFAULT 'demandee', -- demandee, en_cours, ok, echec
    origine       TEXT    NOT NULL,                     -- planifiee, commande, interface
    metriques     INTEGER NOT NULL DEFAULT 0,
    logs          INTEGER NOT NULL DEFAULT 0,
    fichier       TEXT    NOT NULL DEFAULT '',
    taille        INTEGER NOT NULL DEFAULT 0,
    destinations  TEXT    NOT NULL DEFAULT '',          -- où l'archive a été copiée
    erreur        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX sauvegardes_statut ON sauvegardes(statut);
