-- SPDX-License-Identifier: GPL-3.0-or-later
-- Index manquants (mesure du grand parc, 27/11) : la liste des machines
-- compte les tokens actifs et cherche le jeton d'enrôlement de CHAQUE
-- machine. Sans index, SQLite relit toute la table pour chacune
-- (1 500 machines : 0,35 s par liste, appelée par la plupart des pages).
-- Coût : ~30 octets par token ou jeton sur disque, rien en mémoire.
CREATE INDEX tokens_machine ON tokens(machine_id, revoque_le);
CREATE INDEX jetons_enrolement_machine ON jetons_enrolement(machine_id);
