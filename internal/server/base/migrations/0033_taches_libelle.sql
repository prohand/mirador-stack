-- SPDX-License-Identifier: GPL-3.0-or-later
-- Nom affiché d'une tâche suivie (SPEC § 89) : vide = nom d'origine, qui
-- reste la clé (rien ne casse si on renomme).
ALTER TABLE taches ADD COLUMN libelle TEXT NOT NULL DEFAULT '';
