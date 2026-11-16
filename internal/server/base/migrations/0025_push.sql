-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0025 (SPEC § 35) : appareils abonnés aux notifications push
-- (application installée, navigateur). adresse : adresse du service de push
-- pour cet appareil, donnée par le navigateur (elle vaut un secret : jamais
-- affichée ni journalisée). p256dh et auth : clés de l'appareil, pour
-- chiffrer chaque message. origine : « https://hôte » de l'interface au
-- moment de l'abonnement. Effacés avec le compte.
CREATE TABLE abonnements_push (
    id             INTEGER PRIMARY KEY,
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    adresse        TEXT    NOT NULL UNIQUE,
    p256dh         BLOB    NOT NULL,
    auth           BLOB    NOT NULL,
    origine        TEXT    NOT NULL,
    appareil       TEXT    NOT NULL DEFAULT '',
    cree_le        INTEGER NOT NULL,
    reussi_le      INTEGER NOT NULL DEFAULT 0,   -- dernier envoi accepté par le service
    echecs         INTEGER NOT NULL DEFAULT 0    -- envois ratés de suite
);
CREATE INDEX abonnements_push_utilisateur ON abonnements_push(utilisateur_id);
