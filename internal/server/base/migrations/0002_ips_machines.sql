-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0002 : token lié à des adresses IP (réponse du 09/11).
-- ips_autorisees : adresses ou préfixes (CIDR) séparés par des virgules ;
-- vide = toute adresse admise par la liste d'IP de nginx.
ALTER TABLE machines ADD COLUMN ips_autorisees TEXT NOT NULL DEFAULT '';
-- Dernière adresse vue (aide à mettre à jour la liste dans l'interface).
ALTER TABLE machines ADD COLUMN derniere_ip TEXT NOT NULL DEFAULT '';
