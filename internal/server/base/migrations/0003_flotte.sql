-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0003 : enrôlement, battements, mises à jour (étape 3, partie 5).

-- Adresse de la demande d'enrôlement (proposée pour la liste ips_autorisees).
ALTER TABLE machines ADD COLUMN ip_enrolement TEXT NOT NULL DEFAULT '';
-- Dernier battement de l'agent (écrit par lots, une fois par minute).
ALTER TABLE machines ADD COLUMN battement_le INTEGER NOT NULL DEFAULT 0;
ALTER TABLE machines ADD COLUMN demarrage_le INTEGER NOT NULL DEFAULT 0;
ALTER TABLE machines ADD COLUMN maj_essai TEXT NOT NULL DEFAULT '';
ALTER TABLE machines ADD COLUMN maj_echec TEXT NOT NULL DEFAULT '';
-- Version de l'agent assignée à cette machine ('' = celle du réglage global).
ALTER TABLE machines ADD COLUMN version_cible TEXT NOT NULL DEFAULT '';

-- Jetons d'enrôlement : dernier token remis (une nouvelle remise révoque le
-- précédent, par exemple si la réponse s'est perdue).
ALTER TABLE jetons_enrolement ADD COLUMN token_remis_le INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jetons_enrolement ADD COLUMN identifiant TEXT NOT NULL DEFAULT '';

-- Versions de mirador-agent publiées (manifeste signé par mirador-release,
-- sur le poste de l'utilisateur ; le serveur ne signe jamais).
CREATE TABLE versions_agent (
    version         TEXT    NOT NULL,
    architecture    TEXT    NOT NULL,
    sha256          TEXT    NOT NULL,
    taille          INTEGER NOT NULL,
    retour_autorise INTEGER NOT NULL DEFAULT 0,
    signature       TEXT    NOT NULL,
    publiee_le      INTEGER NOT NULL,
    publiee_par     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (version, architecture)
);
