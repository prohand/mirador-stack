-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0031 (SPEC § 79, demande du 20/11) : menu réorganisé par
-- l'utilisateur, un pour l'ordinateur et un pour le téléphone (ordre,
-- entrées masquées, 4 raccourcis de la barre du bas). Document JSON court ;
-- '' = menu par défaut.
ALTER TABLE utilisateurs ADD COLUMN menu TEXT NOT NULL DEFAULT '';
