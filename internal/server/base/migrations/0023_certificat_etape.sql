-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0023 (SPEC § 32) : avancement de la création du certificat du
-- syslog. etape : étape en cours de la démarche Let's Encrypt (vide = pas
-- de démarche en cours) ; etape_info : détail (« visible sur 1/2 serveurs
-- DNS ») ; etape_le : début de l'étape ; etape_jusqua : fin prévue d'une
-- attente (publication DNS).
ALTER TABLE certificats_syslog ADD COLUMN etape TEXT NOT NULL DEFAULT '';
ALTER TABLE certificats_syslog ADD COLUMN etape_info TEXT NOT NULL DEFAULT '';
ALTER TABLE certificats_syslog ADD COLUMN etape_le INTEGER NOT NULL DEFAULT 0;
ALTER TABLE certificats_syslog ADD COLUMN etape_jusqua INTEGER NOT NULL DEFAULT 0;
