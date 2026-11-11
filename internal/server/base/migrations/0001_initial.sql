-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0001 : schéma initial de mirador-server (étape 3).
-- Index limités aux requêtes réelles (SPEC § 7.2).

-- Machines enrôlées (agents et collecteurs).
CREATE TABLE machines (
    id            INTEGER PRIMARY KEY,
    nom           TEXT    NOT NULL UNIQUE,      -- étiquette instance / host
    site          TEXT    NOT NULL DEFAULT '',
    env           TEXT    NOT NULL DEFAULT '',
    groupe        TEXT    NOT NULL DEFAULT '',
    type          TEXT    NOT NULL DEFAULT 'agent', -- agent, collecteur
    statut        TEXT    NOT NULL DEFAULT 'en_attente', -- en_attente, approuvee, refusee
    identifiant   TEXT    NOT NULL DEFAULT '',  -- identifiant aléatoire de l'installation
    version_agent TEXT    NOT NULL DEFAULT '',
    architecture  TEXT    NOT NULL DEFAULT '',
    cree_le       INTEGER NOT NULL,             -- secondes Unix
    vu_le         INTEGER NOT NULL DEFAULT 0
);

-- Tokens des machines : seule l'empreinte (SHA-256) est gardée, jamais le token.
CREATE TABLE tokens (
    id          INTEGER PRIMARY KEY,
    machine_id  INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    empreinte   BLOB    NOT NULL UNIQUE,
    cree_le     INTEGER NOT NULL,
    revoque_le  INTEGER NOT NULL DEFAULT 0       -- 0 = valide
);

-- Jetons d'enrôlement : usage unique, durée limitée (empreinte seulement).
CREATE TABLE jetons_enrolement (
    id          INTEGER PRIMARY KEY,
    empreinte   BLOB    NOT NULL UNIQUE,
    expire_le   INTEGER NOT NULL,
    utilise_le  INTEGER NOT NULL DEFAULT 0,
    machine_id  INTEGER REFERENCES machines(id) ON DELETE SET NULL,
    cree_par    TEXT    NOT NULL DEFAULT '',
    cree_le     INTEGER NOT NULL
);

-- Réglages modifiables dans l'interface : valeur JSON, défaut dans le code.
CREATE TABLE reglages (
    cle         TEXT    PRIMARY KEY,
    valeur      TEXT    NOT NULL,                -- JSON
    modifie_le  INTEGER NOT NULL,
    modifie_par TEXT    NOT NULL DEFAULT ''
);

-- Journal d'audit : qui, quand, quoi, ancienne et nouvelle valeur.
-- Les secrets n'y sont jamais écrits (remplacés par « *** »).
CREATE TABLE audit (
    id        INTEGER PRIMARY KEY,
    le        INTEGER NOT NULL,
    qui       TEXT    NOT NULL,
    action    TEXT    NOT NULL,
    objet     TEXT    NOT NULL DEFAULT '',
    avant     TEXT    NOT NULL DEFAULT '',
    apres     TEXT    NOT NULL DEFAULT '',
    adresse   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX audit_le ON audit(le);
