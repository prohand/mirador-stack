-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0016 (SPEC § 24.10) : listes des modèles (adresses, ports,
-- fichiers… à tester par l'agent), JSON { nom: [ligne, …] } ; une liste
-- absente est héritée du groupe.
ALTER TABLE modeles_attribues ADD COLUMN listes TEXT NOT NULL DEFAULT '{}';
