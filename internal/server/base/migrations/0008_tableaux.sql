-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0008 : tableaux modifiables depuis l'interface (SPEC § 18.14).
-- Un tableau fourni (embarqué dans le binaire) modifié ou supprimé a ici sa
-- ligne : elle le remplace. « Rétablir l'original » efface la ligne.
-- Un tableau créé dans l'interface n'existe qu'ici.
CREATE TABLE tableaux (
    nom          TEXT    PRIMARY KEY,
    contenu      TEXT    NOT NULL,            -- JSON (format mirador, version 1) ; '' si supprimé
    supprime     INTEGER NOT NULL DEFAULT 0,  -- 1 = masqué (tableau fourni supprimé)
    modifie_le   INTEGER NOT NULL,
    modifie_par  TEXT    NOT NULL
);
