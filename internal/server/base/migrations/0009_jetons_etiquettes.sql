-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0009 : site, type (env) et groupe choisis dans l'interface à la
-- création du jeton d'enrôlement ; copiés sur la machine à sa demande
-- (plus besoin de --site / --env sur la machine, SPEC § 18.15).
ALTER TABLE jetons_enrolement ADD COLUMN site   TEXT NOT NULL DEFAULT '';
ALTER TABLE jetons_enrolement ADD COLUMN env    TEXT NOT NULL DEFAULT '';
ALTER TABLE jetons_enrolement ADD COLUMN groupe TEXT NOT NULL DEFAULT '';
