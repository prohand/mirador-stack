-- SPDX-License-Identifier: GPL-3.0-or-later
-- Historique des exécutions des tâches planifiées (SPEC § 95) : une ligne
-- par exécution terminée, notée par le serveur chaque minute (tâches lues
-- par l'agent, suivies ou non, et signaux curl). 60 lignes au plus par
-- tâche. Sert à la frise des dernières exécutions, à la durée habituelle
-- (durée anormale) et à la taille habituelle des sauvegardes (taille en baisse).
CREATE TABLE taches_executions (
    cle     TEXT    NOT NULL,           -- même clé que taches.cle
    fin     INTEGER NOT NULL,           -- unix (s)
    debut   INTEGER NOT NULL DEFAULT 0, -- 0 : inconnu
    code    INTEGER NOT NULL DEFAULT 0,
    octets  REAL    NOT NULL DEFAULT 0, -- sauvegardes : total écrit
    vms     TEXT    NOT NULL DEFAULT '', -- sauvegardes : {"101": octets, …} (JSON)
    PRIMARY KEY (cle, fin)
) WITHOUT ROWID;
