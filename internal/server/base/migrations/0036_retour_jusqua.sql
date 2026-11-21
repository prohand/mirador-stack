-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0036 (SPEC § 112) : fin signée de l'autorisation de retour
-- arrière d'une version (secondes Unix, 0 : manifeste sans date).
ALTER TABLE versions_binaires ADD COLUMN retour_jusqua INTEGER NOT NULL DEFAULT 0;
