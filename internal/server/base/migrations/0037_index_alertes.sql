-- SPDX-License-Identifier: GPL-3.0-or-later
-- Relecture du 21/11 (SPEC § 116) : l'historique des alertes (pages
-- Événements, Incidents, Disponibilité, page de statut publique) était lu
-- en entier et trié à chaque appel : 15 ms à 100 000 alertes, et le coût
-- grandit avec la conservation (180 jours). Index sur l'heure de passage en
-- alerte (les alertes jamais actives, active_le = 0, n'y figurent pas).
CREATE INDEX alertes_active_le ON alertes(active_le) WHERE active_le > 0;
