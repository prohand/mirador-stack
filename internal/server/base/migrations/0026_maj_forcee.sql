-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0026 : « Mettre à jour maintenant » / « Réessayer la mise à jour »
-- (SPEC § 44). Heure (secondes Unix) de la dernière demande faite dans
-- l'interface pour cette machine ; 0 = jamais.
ALTER TABLE machines ADD COLUMN maj_forcee_le INTEGER NOT NULL DEFAULT 0;
