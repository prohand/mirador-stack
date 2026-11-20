-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0030 (SPEC § 78, demande du 20/11) : appareils de confiance.
-- « Rester connecté sur cet appareil » : la session survit à la fermeture de
-- l'application (PWA) et aux redémarrages du serveur. Seule l'empreinte
-- SHA-256 de l'identifiant du cookie est gardée : un vol de la base ne donne
-- aucun cookie utilisable.
CREATE TABLE appareils_confiance (
    empreinte      TEXT    PRIMARY KEY,                -- SHA-256 de l'identifiant du cookie
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    generation     INTEGER NOT NULL,                   -- génération du compte (mot de passe changé = refus)
    cree_le        INTEGER NOT NULL,                   -- connexion avec le mot de passe et la 2FA (s Unix)
    vu_le          INTEGER NOT NULL,                   -- dernier usage connu (s Unix, noté toutes les 10 min au plus)
    adresse        TEXT    NOT NULL DEFAULT '',        -- adresse de la connexion
    appareil       TEXT    NOT NULL DEFAULT ''         -- « iPhone · Safari », d'après le navigateur
);
CREATE INDEX appareils_confiance_utilisateur ON appareils_confiance(utilisateur_id);
