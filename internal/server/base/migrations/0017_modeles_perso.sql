-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0017 (SPEC § 24.11) : modèles personnels, créés ou copiés dans
-- l'interface. definition : le modèle en JSON (même format que les modèles
-- livrés, vérifié à chaque enregistrement et à chaque chargement).
CREATE TABLE modeles_perso (
    nom         TEXT    PRIMARY KEY,
    definition  TEXT    NOT NULL,
    modifie_le  INTEGER NOT NULL,
    modifie_par TEXT    NOT NULL DEFAULT ''
);
