-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0021 (SPEC § 26) : aides de l'interface affichées ou masquées,
-- au choix de chaque utilisateur (bouton en bas du menu). 0 = affichées.
ALTER TABLE utilisateurs ADD COLUMN sans_aides INTEGER NOT NULL DEFAULT 0;
