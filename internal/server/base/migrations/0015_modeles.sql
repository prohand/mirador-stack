-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0015 (SPEC § 24) : modèles de surveillance attribués.
--   portee     : « machine:ID » ou « groupe:NOM » ;
--   etat       : « actif » (attribué) ou « ignore » (proposition refusée, ou
--                modèle du groupe retiré pour cette machine) ;
--   parametres : seuils propres à cette portée (JSON { nom: valeur }).
CREATE TABLE modeles_attribues (
    portee      TEXT    NOT NULL,
    modele      TEXT    NOT NULL,
    etat        TEXT    NOT NULL DEFAULT 'actif',
    parametres  TEXT    NOT NULL DEFAULT '{}',
    modifie_le  INTEGER NOT NULL,
    modifie_par TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (portee, modele)
);
