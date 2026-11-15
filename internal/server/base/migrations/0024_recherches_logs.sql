-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0024 (SPEC § 33) : recherches de logs enregistrées par chaque
-- utilisateur (raccourcis de la page Logs). requete : adresse de la
-- recherche (paramètres connus seulement, revérifiés à l'usage).
CREATE TABLE recherches_logs (
    id             INTEGER PRIMARY KEY,
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    nom            TEXT    NOT NULL,
    requete        TEXT    NOT NULL,
    cree_le        INTEGER NOT NULL
);
CREATE INDEX recherches_logs_utilisateur ON recherches_logs(utilisateur_id);
