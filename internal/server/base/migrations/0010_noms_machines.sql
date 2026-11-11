-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0010 (11/11, SPEC § 18.23) :
--   - nom de la future machine choisi à la création du jeton (imposé à la
--     demande d'enrôlement, '' = nom proposé par la machine) ;
--   - anciens noms d'une machine renommée : à la lecture, l'historique écrit
--     sous un ancien nom est rattaché au nom actuel (graphiques, logs).
ALTER TABLE jetons_enrolement ADD COLUMN nom TEXT NOT NULL DEFAULT '';

CREATE TABLE noms_anciens (
    machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    nom        TEXT    NOT NULL UNIQUE,     -- ancien nom (instance / host)
    jusqu_a    INTEGER NOT NULL             -- renommée ce jour-là (secondes Unix)
);
