-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0007 : comptes de l'interface web (étape 5, partie 1).
-- Les sessions restent en mémoire (SPEC § 18.2) : rien à écrire par requête.

-- Utilisateurs de l'interface. Aucun compte par défaut : le premier admin
-- se crée avec un jeton à usage unique affiché dans le journal du serveur.
CREATE TABLE utilisateurs (
    id            INTEGER PRIMARY KEY,
    nom           TEXT    NOT NULL UNIQUE,
    role          TEXT    NOT NULL,                 -- admin, lecture
    mot_de_passe  TEXT    NOT NULL,                 -- empreinte Argon2id (format PHC)
    totp          TEXT    NOT NULL DEFAULT '',      -- secret TOTP chiffré ('' = 2FA à créer)
    totp_dernier  INTEGER NOT NULL DEFAULT 0,       -- dernier pas TOTP accepté (anti-rejeu)
    generation    INTEGER NOT NULL DEFAULT 1,       -- augmentée = toutes ses sessions fermées
    desactive     INTEGER NOT NULL DEFAULT 0,
    cree_le       INTEGER NOT NULL,
    connecte_le   INTEGER NOT NULL DEFAULT 0
);

-- Changements « qui peuvent enfermer dehors » (liste d'IP, rôle) : appliqués
-- tout de suite, annulés automatiquement s'ils ne sont pas confirmés depuis
-- la nouvelle configuration avant expire_le.
CREATE TABLE a_confirmer (
    id         INTEGER PRIMARY KEY,
    quoi       TEXT    NOT NULL,     -- securite, role:NOM
    avant      TEXT    NOT NULL,     -- valeur à remettre (JSON)
    par        TEXT    NOT NULL,
    cree_le    INTEGER NOT NULL,
    expire_le  INTEGER NOT NULL
);
