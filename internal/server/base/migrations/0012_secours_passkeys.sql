-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0012 (SPEC § 20) : codes de secours de la 2FA et passkeys.

-- Codes de secours : 10 par compte, à usage unique. Seule l'empreinte
-- SHA-256 est gardée (un code fait 60 bits d'aléa : pas besoin d'Argon2id).
CREATE TABLE codes_secours (
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    empreinte      BLOB    NOT NULL,
    cree_le        INTEGER NOT NULL,
    utilise_le     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (utilisateur_id, empreinte)
);

-- Passkeys (WebAuthn) : clé publique seulement ; la clé privée ne quitte
-- jamais l'appareil ou le gestionnaire de mots de passe de l'utilisateur.
CREATE TABLE passkeys (
    id             INTEGER PRIMARY KEY,
    utilisateur_id INTEGER NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
    identifiant    BLOB    NOT NULL UNIQUE,   -- identifiant de la clé (credential id)
    cle_publique   BLOB    NOT NULL,          -- format PKIX (DER)
    algorithme     INTEGER NOT NULL,          -- COSE : -7 ES256, -8 EdDSA, -257 RS256
    compteur       INTEGER NOT NULL DEFAULT 0,
    domaine        TEXT    NOT NULL,          -- domaine de l'interface à la création (rpId)
    nom            TEXT    NOT NULL,
    cree_le        INTEGER NOT NULL,
    utilise_le     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX passkeys_utilisateur ON passkeys(utilisateur_id);
