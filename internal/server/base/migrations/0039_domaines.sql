-- SPDX-License-Identifier: GPL-3.0-or-later
-- Expiration des noms de domaine (demande du 27/11) : un domaine principal
-- par ligne (sous-domaines des sondes regroupés), lu par RDAP une fois par jour.
CREATE TABLE domaines (
    nom        TEXT    PRIMARY KEY,           -- « example.org »
    sous       TEXT    NOT NULL DEFAULT '',   -- « cloud, pass, www » (affichage)
    expire     INTEGER NOT NULL DEFAULT 0,    -- s ; 0 : inconnu
    verifie_le INTEGER NOT NULL DEFAULT 0,    -- s
    erreur     TEXT    NOT NULL DEFAULT ''
);
