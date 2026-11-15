-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0022 (SPEC § 30) : acquittement pour une durée. Fin de la prise
-- en charge (millisecondes) ; 0 = jusqu'à la fin de l'alerte (comme avant).
ALTER TABLE alertes ADD COLUMN acquittee_jusqua INTEGER NOT NULL DEFAULT 0;
