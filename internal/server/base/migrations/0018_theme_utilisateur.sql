-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0018 (SPEC § 25) : thème de l'interface choisi par chaque
-- utilisateur (sélecteur en bas du menu) : « clair », « sombre », « auto »
-- (suit le système) ; '' = réglage du serveur (Réglages › Interface).
ALTER TABLE utilisateurs ADD COLUMN theme TEXT NOT NULL DEFAULT '';
