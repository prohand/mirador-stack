-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0014 (SPEC § 18.28, correction du 14/11) : adresses depuis
-- lesquelles un compte s'est déjà connecté. Avant, elles n'étaient qu'en
-- mémoire : chaque redémarrage du serveur redonnait le message « nouvelle
-- adresse ». Sert seulement à ce message (le verrouillage des comptes garde
-- ses propres règles, en mémoire).
CREATE TABLE adresses_connues (
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    adresse        TEXT    NOT NULL, -- IPv4, ou préfixe /64 en IPv6 (auth.CleAdresse)
    vue_le         INTEGER NOT NULL, -- dernière connexion réussie (s Unix)
    PRIMARY KEY (utilisateur_id, adresse)
);
