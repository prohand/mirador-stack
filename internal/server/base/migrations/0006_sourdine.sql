-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0006 : silences, maintenances, inhibitions, dépendances,
-- historique des envois (étape 4, partie 2). Horodatages en millisecondes.

-- Silences : alertes qui correspondent au sélecteur, pendant [debut, fin].
-- selecteur : sélecteur PromQL d'étiquettes, ex. {alertname="CpuEleve",instance=~"pve.*"}.
CREATE TABLE silences (
    id          INTEGER PRIMARY KEY,
    selecteur   TEXT    NOT NULL,
    debut       INTEGER NOT NULL,
    fin         INTEGER NOT NULL,
    auteur      TEXT    NOT NULL,
    commentaire TEXT    NOT NULL DEFAULT '',
    cree_le     INTEGER NOT NULL,
    annule_le   INTEGER NOT NULL DEFAULT 0
);

-- Fenêtres de maintenance : une cible (machine:NOM, groupe:NOM ou sélecteur),
-- ponctuelle (debut, fin) ou récurrente (recurrence en JSON : jours, heure,
-- durée, fuseau).
CREATE TABLE maintenances (
    id          INTEGER PRIMARY KEY,
    cible       TEXT    NOT NULL,
    debut       INTEGER NOT NULL DEFAULT 0,
    fin         INTEGER NOT NULL DEFAULT 0,
    recurrence  TEXT    NOT NULL DEFAULT '',
    auteur      TEXT    NOT NULL,
    commentaire TEXT    NOT NULL DEFAULT '',
    cree_le     INTEGER NOT NULL
);

-- Inhibitions : une alerte active « source » tue les alertes « cible » qui
-- ont les mêmes valeurs pour les étiquettes de egales (JSON, liste).
CREATE TABLE inhibitions (
    id          INTEGER PRIMARY KEY,
    source      TEXT    NOT NULL,
    cible       TEXT    NOT NULL,
    egales      TEXT    NOT NULL DEFAULT '[]',
    commentaire TEXT    NOT NULL DEFAULT '',
    cree_le     INTEGER NOT NULL
);

-- Dépendances : les alertes de l'enfant (machine:NOM, groupe:NOM ou
-- sélecteur) sont tues quand la machine parent est en panne.
CREATE TABLE dependances (
    id          INTEGER PRIMARY KEY,
    enfant      TEXT    NOT NULL,
    parent      TEXT    NOT NULL,
    commentaire TEXT    NOT NULL DEFAULT '',
    cree_le     INTEGER NOT NULL
);

-- Historique des envois de notifications (ou de ce qui aurait été envoyé
-- en mode ombre).
CREATE TABLE envois (
    id        INTEGER PRIMARY KEY,
    le        INTEGER NOT NULL,
    canal     TEXT    NOT NULL,
    groupe    TEXT    NOT NULL,
    gravite   TEXT    NOT NULL,
    type      TEXT    NOT NULL,             -- alerte, rappel, resolution, panne_canal, test…
    nb        INTEGER NOT NULL DEFAULT 0,   -- nombre d'alertes dans le message
    statut    TEXT    NOT NULL,             -- ok, echec, ombre, en_file
    duree_ms  INTEGER NOT NULL DEFAULT 0,
    secours   INTEGER NOT NULL DEFAULT 0,   -- envoyé par le canal de secours
    erreur    TEXT    NOT NULL DEFAULT '',
    texte     TEXT    NOT NULL DEFAULT ''   -- message (sans secret)
);
CREATE INDEX envois_le ON envois(le);

-- File d'attente des messages quand tous les canaux sont en panne (bornée,
-- renvoyée au retour d'un canal avec l'heure d'origine). message : JSON.
CREATE TABLE file_notifications (
    id      INTEGER PRIMARY KEY,
    cree_le INTEGER NOT NULL,
    gravite TEXT    NOT NULL,
    message TEXT    NOT NULL
);

-- Demandes de test d'un canal (commande notif-tester), faites par le
-- serveur en marche (seul à détenir les secrets déchiffrés).
CREATE TABLE tests_canaux (
    id          INTEGER PRIMARY KEY,
    canal       TEXT    NOT NULL,
    demande_le  INTEGER NOT NULL,
    fait_le     INTEGER NOT NULL DEFAULT 0,
    statut      TEXT    NOT NULL DEFAULT '',
    erreur      TEXT    NOT NULL DEFAULT ''
);

-- Écarts avec vmalert pendant la période en parallèle (mode ombre) : une
-- ligne par épisode (début, fin ; fin = 0 tant qu'il dure).
CREATE TABLE ecarts_vmalert (
    id      INTEGER PRIMARY KEY,
    regle   TEXT    NOT NULL,
    machine TEXT    NOT NULL,
    type    TEXT    NOT NULL,   -- manquante (vmalert seul), en_trop (mirador seul)
    debut   INTEGER NOT NULL,
    fin     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ecarts_vmalert_fin ON ecarts_vmalert(fin);
