-- SPDX-License-Identifier: GPL-3.0-or-later
-- Connexions SSH depuis une adresse nouvelle (demande du 27/11) : adresses
-- publiques vues par machine (90 jours), et adresses « connues » (bouton
-- « C'est moi » de l'alerte), qui ne déclenchent plus rien.
CREATE TABLE ssh_adresses (
    machine         TEXT    NOT NULL,
    adresse         TEXT    NOT NULL,
    utilisateur     TEXT    NOT NULL DEFAULT '',  -- « admin · publickey »
    premiere        INTEGER NOT NULL,             -- s
    derniere        INTEGER NOT NULL,             -- s
    nouvelle_jusqua INTEGER NOT NULL DEFAULT 0,   -- s : alerte tenue jusqu'à
    PRIMARY KEY (machine, adresse)
);
CREATE TABLE ssh_connues (
    adresse TEXT    PRIMARY KEY,
    par     TEXT    NOT NULL,
    le      INTEGER NOT NULL
);
