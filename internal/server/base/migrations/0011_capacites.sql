-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0011 (SPEC § 19) : capacités annoncées par l'agent (en-tête
-- X-Mirador-Capacites du battement). Le serveur n'envoie les réglages de
-- surveillance qu'aux agents qui les connaissent, et l'interface le dit.
ALTER TABLE machines ADD COLUMN capacites TEXT NOT NULL DEFAULT '';
