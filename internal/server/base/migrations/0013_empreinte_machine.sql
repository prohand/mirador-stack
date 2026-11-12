-- SPDX-License-Identifier: GPL-3.0-or-later
-- Migration 0013 (SPEC § 19.8) : empreinte de la machine, envoyée par l'agent
-- (en-tête X-Mirador-Empreinte, dérivée de /etc/machine-id).
--   empreinte_vue  : dernière empreinte reçue (information) ;
--   empreinte_liee : si elle n'est pas vide, le token de la machine n'est
--                    accepté qu'avec cette empreinte (choix de l'admin).
ALTER TABLE machines ADD COLUMN empreinte_vue  TEXT NOT NULL DEFAULT '';
ALTER TABLE machines ADD COLUMN empreinte_liee TEXT NOT NULL DEFAULT '';
