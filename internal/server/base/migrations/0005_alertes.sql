-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0005 : moteur d'alertes (étape 4, partie 1).

-- Règles d'alerte. definition : la règle complète en JSON (alertes.Regle),
-- vérifiée à l'enregistrement et à la lecture.
CREATE TABLE regles (
    nom         TEXT    PRIMARY KEY,
    definition  TEXT    NOT NULL,
    modifie_le  INTEGER NOT NULL,
    modifie_par TEXT    NOT NULL DEFAULT ''
);

-- Compteurs de modification : le serveur relit une table quand son compteur
-- change (rechargement à chaud, sans relire toute la table chaque minute).
CREATE TABLE generations (
    nom    TEXT    PRIMARY KEY,
    valeur INTEGER NOT NULL
);

-- Alertes : une ligne par alerte (règle + étiquettes), écrite seulement
-- quand son état change. Horodatages en millisecondes Unix.
CREATE TABLE alertes (
    id            INTEGER PRIMARY KEY,
    regle         TEXT    NOT NULL,
    etiquettes    TEXT    NOT NULL,                -- JSON, dont alertname et severity
    gravite       TEXT    NOT NULL,
    etat          TEXT    NOT NULL,                -- en_attente, active, resolue
    debut         INTEGER NOT NULL,                -- condition vraie depuis
    active_le     INTEGER NOT NULL DEFAULT 0,      -- passée en alerte
    fin           INTEGER NOT NULL DEFAULT 0,      -- résolue
    valeur_debut  REAL    NOT NULL DEFAULT 0,      -- valeur au passage en alerte
    valeur_max    REAL    NOT NULL DEFAULT 0,
    valeur        REAL    NOT NULL DEFAULT 0,      -- dernière valeur
    resume        TEXT    NOT NULL DEFAULT '',
    description   TEXT    NOT NULL DEFAULT '',
    acquittee_le  INTEGER NOT NULL DEFAULT 0,
    acquittee_par TEXT    NOT NULL DEFAULT '',
    commentaire   TEXT    NOT NULL DEFAULT ''
);
-- Alertes en cours (relues au démarrage) et purge de l'historique.
CREATE INDEX alertes_en_cours ON alertes(etat) WHERE etat != 'resolue';
CREATE INDEX alertes_fin ON alertes(fin) WHERE etat = 'resolue';
